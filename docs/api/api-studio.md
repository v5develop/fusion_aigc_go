# 商拍换装 / 姿态复刻 / 图片生成 · 前端接口文档

三个功能共用一套「批次 - 任务」模型和同一组接口：

| 功能 | 提交接口 | 一个批次拆成几个任务 |
|---|---|---|
| 商拍换装 | `POST /tryon/submit` | 模特数 × 每位模特张数（最多 81） |
| 姿态复刻 | `POST /pose/submit` | 姿势图张数（1–9） |
| 图片生成 | `POST /image-generate/submit` | `count`（1–9） |

提交前的「消耗 N 点」用对应的 `POST /{tryon,pose,image-generate}/quote`（只读报价，见 7.1）。

> 本文以 `app/gateway/api/gateway.api` 的 studio 组为准，字段名一律下划线。实现与本文不一致时，以接口定义为准，并请通知后端更新本文。
> 管理后台的接口见最后一章，给后台同事。

---

## 0. 概述

### 0.1 一个批次，一批任务

- **批次（batch）**：用户点一次「生成」就是一个批次，对应历史记录里的一条。
- **任务（task）**：批次里的一张图。任务是计费、出图、失败重试、入素材库的最小单位。
- 一个批次提交后立刻返回 `batch_id` 和全部 `task_ids`，前端对这个批次轮询到终态即可。

### 0.2 提交方式变化：从「逐次提交」改为「整批提交」

旧前端（s1.1 前端包）是**每个模特 × 每张**调用一次、每次 `num=1`；姿态是**每个姿势**调用一次。新接口**一次提交整批，服务端拆任务**：

| | 旧（逐次） | 新（整批） |
|---|---|---|
| 商拍 | 模特 × 张数 次请求，每次 1 个模特 | 1 次请求，带全部模特和 `count_per_model` |
| 姿态 | 姿势数 次请求 | 1 次请求，带全部姿势 `pose_image_urls` |
| 图片生成 | 1 次请求带 `count` | 不变，仍是 1 次请求（`count` 张拆成 `count` 个任务） |

为什么要改：

1. **批次级缓存，省成本。** 商拍的「切图 → 视觉反推」很贵，同一批次里切图只做一次，每个模特的反推只做一次（同一模特的多张图共用一份提示词）；姿态的人物解析按人物图只做一次。逐次提交时这些缓存没法关联，9 × 9 = 81 个任务会产生 300 多次视觉调用；整批提交后每批只调用几十次。
2. **计费与失败处理更干净。** 一批的预估点数一次算出、一次冻结；任何一张冻结失败（比如点数不够）整批回滚，不会出现「提交了一半」。
3. **前端更简单。** 不用在前端循环、并发控制、失败补发；轮询一个批次接口就能拿到全部进度。

### 0.3 与 v1.0 契约字段对照

v1.0 契约里的 `tool:image-generate`、`tool:ai-posture-fission`、`tool:ai-digital-model` 是旧字段。这三个功能改调本文的接口，字段对应如下（前端适配层按此改）：

**图片生成（`tool:image-generate`）**

| 前端旧字段 | 新字段 | 说明 |
|---|---|---|
| `prompt` | `prompt` | 必填，≤3000 字 |
| `upload_0`（参考图，≤9） | `image_urls` | 不再 multipart 上传，先直传 OSS 再传地址 |
| `model` | `model` | 四档，见第 6 章 |
| `resolution`（1K/2K/4K） | `resolution` | 大小写均可 |
| `aspectRatio` | `aspect_ratio` | 默认 `1:1` |
| `count` | `count` | 1–9，必填 |
| `rewritePrompt` | `rewrite_prompt` | 先润色提示词 |
| `X-Request-Id` | `Idempotency-Key`（请求头） | 必填 |
| — | `workspace_id` | 新增，不传 = 个人空间 |

**姿态复刻（`tool:ai-posture-fission`，含 s1.1 的 `aiPostureFission` payload）**

| 前端旧字段 | 新字段 | 说明 |
|---|---|---|
| `upload_0`（人物 1 张）/ `sourceImageUrl` | `source_image_url` | 人物图，图 1 |
| `upload_1`（参考姿势）/ `poseImageUrl`（每姿势一次） | `pose_image_urls` | 一次传全部姿势图，1–9 张 |
| `prompt`（全局 + 本张换行拼接） | `global_prompt` + `pose_prompts` | **拆开传**：全局说明放 `global_prompt`，单张说明放 `pose_prompts` 的对应下标，不要自己拼 |
| `mode`（发散 / 参考 / 模仿） | 取消 | 新接口没有该字段，不要传 |
| `count` / `num` | 取消 | 张数 = `pose_image_urls` 的长度 |
| `model` | `model` | 四档名称，不是内部 id |
| `resolution` | `resolution` | 默认 `1k` |
| `aspectRatio` | `aspect_ratio` | 空 = 自动（按人物图比例取最近档） |

**商拍换装（`tool:ai-digital-model`，含 s1.1 的 `aiModelTryon` payload）**

| 前端旧字段 | 新字段 | 说明 |
|---|---|---|
| `modelImageUrl`（每模特一次） | `model_image_urls` | 一次传全部模特图，1–9 张，每张一位模特 |
| `num`（恒为 1） + 前端循环张数 | `count_per_model` | 每位模特张数 1–9，必填 |
| `garmentImages[{url, role, angle}]` | `garment_images[{url, role, angle}]` | 服装图 ≤9 张，每件 ≤6 张 |
| `items` | `items` | 单品 1–3 件 |
| `setMode` | `set_mode` | `one` / `two`，空按 `one` |
| `lengths` / `sleeves` | `lengths` / `sleeves` | 只认 `items` 里有的键，多余的键服务端会丢弃 |
| `fabric` | `fabric` | 按件写 |
| `prompt` | `prompt` | 补充说明，≤3000 字 |
| `model` / `resolution` / `aspectRatio` | `model` / `resolution` / `aspect_ratio` | 默认 `通用` / `1k` / 自动（按模特图比例取最近档） |
| `clothImageUrl`（首张服装，旧兼容） | `cloth_image_url` | **新前端不要传**；只有 `garment_images` 为空时才会被当作一张正面服装图 |
| `imageUrls[服装首图, 模特]`（兼容位） | 不再使用 | — |
| `upload_0`（商品图 ≤20） / `upload_1`（模特 1） | 不再使用 | v1.0 的 20 张商品图改为 9 张分件分角度 |

---

## 1. 通用约定

- 网关：和其它接口同一个网关域名，studio 的前缀是 **`/api/v1/studio`**（不是 `/api/v1/local-life`）。下文路径都省略这个前缀，如 `POST /tryon/submit` 即 `POST /api/v1/studio/tryon/submit`。

### 1.1 鉴权

所有接口带 `Authorization: Bearer <token>`。token 来源见 `api-app.md` 的登录一章（和 AIGC 服务同一套令牌）。联调环境（`Auth.Mode=dev`）没带 token 时认 `X-User-Id: <用户ID>`，线上会关掉。

未登录 / 令牌过期：**HTTP 401**，`code=2001`。这是唯一不返回 HTTP 200 的情况。

### 1.2 响应信封

```json
{"code": 0, "msg": "ok", "data": { }}
```

失败时多一个 `reason`（和 Java 的 ErrorCode 常量名一致），前端按 `reason` 分支，不要按数字：

```json
{"code": 1003, "reason": "PARAM_INVALID", "msg": "模特最多 9 位", "data": null}
```

**HTTP 状态码一律 200，用 `code` 判断；只有未登录是 HTTP 401。** 业务失败 `msg` 是可以直接展示给用户的中文。

> 注意：`api-app.md` 的错误码表是早期口径（1001/1002…）。studio 用的是和 Java 对齐的新口径，见 1.6。

### 1.3 幂等：`Idempotency-Key`

三个提交接口**必须**带请求头 `Idempotency-Key`（UUID，最长 64 字符）。

- 每次点「生成」新生成一个；网络失败、超时重试时**沿用同一个**。
- 同一用户、同一个 key、**同一份 body**：不会重复生成、不会重复扣点，返回**原批次**，`replayed=true`。
- 同一用户、同一个 key、**不同的 body**：返回冲突 `code=4002`（`RESOURCE_CONFLICT`），不会生成。换了内容就必须换 key。
- 缺这个头：`code=1002`，`msg` 为「缺少幂等键（Idempotency-Key）」；超过 64 字符：`code=1003`，「幂等键过长」。
- 兼容旧版本：网关在没带 `Idempotency-Key` 时会用 `X-Request-Id` 补上；新接入统一用 `Idempotency-Key`。

### 1.4 字段与时间

- 请求体、查询参数、响应一律**下划线**命名。
- 时间都是 **unix 毫秒**（`created_at`、`updated_at`、`started_at`、`finished_at`）；`started_at` / `finished_at` 为 0 表示还没发生。
- 任务号 `task_id` 是字符串；批次号 `batch_id` / `id` 是数字。

### 1.5 参数限制

校验在服务端有一份兜底，口径和前端校验必须一致，超限时整批提交失败（`code=1003`，不会生成任何任务）。

| 项 | 限制 |
|---|---|
| 单张图片大小 | ≤10MB（上传前在前端拦） |
| 图片地址 | 必须是**我方 OSS / 素材库地址**（经「上传」接口直传得到的地址，或素材库里的地址）。`blob:`（还没上传完）、`data:`、`file:`、非 http(s) 一律拒绝；单个地址 ≤1024 字符 |
| 服装图（商拍） | ≤9 张，同一单品（`role`）≤6 张 |
| 单品（商拍） | 1–3 件，取值与互斥规则见 2.1 |
| 模特图（商拍） | 1–9 张 |
| 每位模特张数（商拍 `count_per_model`） | 1–9 |
| 姿势图（姿态） | 1–9 张 |
| 参考图（图片生成 `image_urls`） | 0–9 张 |
| 生成张数（图片生成 `count`） | 1–9 |
| 提示词 `prompt` / `global_prompt` / 单张说明 / `fabric` | 各自 ≤3000 字（按字符数，不是字节） |
| 历史 `limit` | 默认 20，上限 50 |

上传图片的做法（预签名直传）见 `api-app.md`「上传：前端直传 OSS」。**提交前要等所有图片上传完成**，拿到的 `url` 才能传给本接口。

### 1.6 错误码

| code | reason | 含义 | 前端怎么处理 |
|---|---|---|---|
| 0 | — | 成功 | — |
| 1002 | PARAM_MISSING | 缺少必填参数（单品 / 服装图 / 模特图 / 姿势图 / 提示词 / 幂等键 / 图片地址为空） | 展示 `msg` |
| 1003 | PARAM_INVALID | 参数不合法（超限、取值不支持、地址不合法、格式不对） | 展示 `msg` |
| 2001 | UNAUTHORIZED | 未登录或登录过期（**HTTP 401**） | 跳登录 |
| 2007 | ACCOUNT_DISABLED | 账号已被禁用 | 提示并退出登录 |
| 3003 | INSUFFICIENT_POINTS | 点数不足（整批没有提交成功） | 提示充值 |
| 4001 | RESOURCE_NOT_FOUND | 批次 / 任务不存在、不属于当前用户或已删除 | 返回上一页 |
| 4002 | RESOURCE_CONFLICT | 状态冲突：同 key 不同 body；取消已结束的批次；删除生成中的批次 | 展示 `msg` |
| 7003 | FEATURE_NOT_READY | 功能暂未开放 | 展示 `msg` |
| 5001 | SYSTEM_ERROR | 服务繁忙 | 提示稍后重试，**重试沿用同一个 `Idempotency-Key`** |

---

## 2. 商拍换装

```
POST /api/v1/studio/tryon/submit
Idempotency-Key: <UUID>
```

整批提交：N 位模特 × 每位 M 张 = N × M 个任务。服务端的处理顺序是：服装切图 → 按模特反推提示词 → 生图；这些步骤的进度体现在批次状态 `status`（见第 4 章）。

### 2.1 请求

```json
{
  "workspace_id": 0,
  "model_image_urls": [
    "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/m1.jpg",
    "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/m2.jpg"
  ],
  "garment_images": [
    {"url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/g1.jpg", "role": "上衣", "angle": "front"},
    {"url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/g2.jpg", "role": "上衣", "angle": "back"},
    {"url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/g3.jpg", "role": "下装", "angle": "front"}
  ],
  "items": ["top", "bottom"],
  "set_mode": "two",
  "lengths": {"bottom": "及踝"},
  "sleeves": {"top": "长袖"},
  "fabric": "上衣：真丝；下装：棉麻",
  "prompt": "自然光，街拍氛围",
  "model": "通用",
  "resolution": "2k",
  "aspect_ratio": "3:4",
  "count_per_model": 2
}
```

| 字段 | 类型 | 必填 | 限制 | 说明 |
|---|---|---|---|---|
| `workspace_id` | int | 否 | — | 工作空间；不传 = 个人空间 |
| `model_image_urls` | string[] | 是 | 1–9 张；我方地址 | 模特图，每张一位模特。顺序即模特序号 `model_index`（从 0 起） |
| `garment_images` | object[] | 是 | 1–9 张；同一 `role` ≤6 张 | 服装图，见下表 |
| `items` | string[] | 是 | 1–3 件；取值见下；不重复；互斥规则见下 | 单品 |
| `set_mode` | string | 否 | `one` / `two` | 成套方式；1 件也传；空按 `one` |
| `lengths` | map | 否 | 键必须在 `items` 里 | `{单品: 长度}`，如 `{"bottom": "及踝"}`；不在 `items` 里的键（比如历史残留）服务端丢弃 |
| `sleeves` | map | 否 | 同上 | `{单品: 袖长}` |
| `fabric` | string | 否 | ≤3000 字 | 面料，按件写：「上衣：真丝；鞋子：皮革」 |
| `prompt` | string | 否 | ≤3000 字 | 补充说明 |
| `model` | string | 否 | 四档之一 | 空 = `通用`，见第 6 章 |
| `resolution` | string | 否 | `1k` / `2k` / `4k`，大小写均可 | 空 = `1k` |
| `aspect_ratio` | string | 否 | `宽:高` 格式 | 空 = 自动：按模特图宽高取 6.3 里最近的一档 |
| `count_per_model` | int | 是 | 1–9 | 每位模特生成几张 |
| `cloth_image_url` | string | 否 | — | 旧版兼容位，**新前端不要传** |

`garment_images[]` 每项：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `url` | string | 是 | 只收 http(s) 我方地址；`blob:` 表示还没上传完，会被拒绝 |
| `role` | string | 否 | 单品中文名（上衣 / 下装 / 连衣裙 / 外套 / 鞋子 / 配饰 / 文胸 / 内裤）或「整套」。**同一个 `role` 最多 6 张** |
| `angle` | string | 否 | `front` / `back` / `side` / `detail` / `flat` / `unknown`；空按 `unknown` |

`items` 取值：`top` 上衣、`bottom` 下装、`dress` 连衣裙、`outer` 外套、`shoes` 鞋子、`accessory` 配饰、`bra` 文胸、`panties` 内裤。互斥：连衣裙与上衣 / 下装、文胸与上衣 / 连衣裙、内裤与下装 / 连衣裙不能同时选。

校验顺序与前端一致：**单品 → 服装图 → 模特图 → 张数 → 其余**。第一个不通过的就是返回的错误。

### 2.2 响应

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "batch_id": 1024,
    "task_total": 4,
    "estimated_points": 12,
    "task_ids": ["tyo_a1b2c3d4", "tyo_a1b2c3d5", "tyo_a1b2c3d6", "tyo_a1b2c3d7"],
    "replayed": false,
    "warnings": []
  }
}
```

提交响应（三个接口通用）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `batch_id` | int | 批次号，之后轮询、取消、删除都用它 |
| `task_total` | int | 任务总数：商拍 = 模特数 × `count_per_model` |
| `estimated_points` | int | 预估消耗点数，见第 7 章 |
| `task_ids` | string[] | 全部任务号。商拍任务号前缀 `tyo`、姿态 `pse`、图片生成 `simg`（示例里的具体值仅作形状参考） |
| `replayed` | bool | `true` = 同一个 `Idempotency-Key` 重复提交，返回的是原批次，没有新生成也没有新扣点 |
| `warnings` | string[] | 参数修正提示，没有为 `[]`。如选了 Seedream 4k：`["Seedream 不支持 4K（会被降为 2K），已按 2K 生成"]`，见 6.2；可直接展示。幂等重放（`replayed=true`）不回 |

### 2.3 错误示例

```json
{"code": 1002, "reason": "PARAM_MISSING", "msg": "请选择单品", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "单品最多 3 件", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "未知的单品：skirt", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "单品重复：上衣", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "连衣裙和上衣不能同时选择", "data": null}
{"code": 1002, "reason": "PARAM_MISSING", "msg": "请上传服装图", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "服装图最多 9 张", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "服装图第 2 张还没上传完成，请等上传结束再提交", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "服装图角度只支持 front / back / side / detail / flat / unknown", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "上衣最多 6 张图", "data": null}
{"code": 1002, "reason": "PARAM_MISSING", "msg": "请上传模特图", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "模特最多 9 位", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "模特图第 1 张地址不合法，只支持 http(s) 地址", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "每位模特生成张数为 1–9", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "成套方式只支持 one / two", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "补充说明不能超过 3000 字", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "分辨率只支持 1k / 2k / 4k", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "比例格式不对，应为 宽:高，如 3:4", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "不支持的模型：Midjourney", "data": null}
{"code": 1002, "reason": "PARAM_MISSING", "msg": "缺少幂等键（Idempotency-Key）", "data": null}
{"code": 3003, "reason": "INSUFFICIENT_POINTS", "msg": "点数不足", "data": null}
```

最后一条的 `msg` 以实际返回为准，前端统一按 `reason` 展示「去充值」即可。

---

## 3. 姿态复刻与图片生成

### 3.1 姿态复刻

```
POST /api/v1/studio/pose/submit
Idempotency-Key: <UUID>
```

一个人物图 + 1–9 张姿势图，每张姿势图生成一张图（一个任务）。

```json
{
  "workspace_id": 0,
  "source_image_url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/person.jpg",
  "pose_image_urls": [
    "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/p1.jpg",
    "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/p2.jpg",
    "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/p3.jpg"
  ],
  "global_prompt": "保持服装不变，表情自然",
  "pose_prompts": {"0": "微笑", "2": "回头看镜头"},
  "model": "Banana",
  "resolution": "1k",
  "aspect_ratio": "3:4"
}
```

| 字段 | 类型 | 必填 | 限制 | 说明 |
|---|---|---|---|---|
| `workspace_id` | int | 否 | — | 不传 = 个人空间 |
| `source_image_url` | string | 是 | 我方地址 | 人物图（生图时的图 1）。图里要有人物且不能是未成年人，否则该批次反推失败，整批失败并退点数 |
| `pose_image_urls` | string[] | 是 | 1–9 张；我方地址 | 姿势图，每张一个任务。顺序即姿势序号 `pose_index`（从 0 起） |
| `global_prompt` | string | 否 | ≤3000 字 | 全局补充说明，对所有姿势生效 |
| `pose_prompts` | map | 否 | 键是姿势下标（字符串形式的非负整数，从 0 起），不能超出 `pose_image_urls` 范围；每条 ≤3000 字 | 单张补充说明，如 `{"0": "…", "2": "…"}`；没有说明的姿势不用传 |
| `model` | string | 否 | 四档之一 | 空 = `通用` |
| `resolution` | string | 否 | `1k` / `2k` / `4k`，大小写均可 | 空 = `1k` |
| `aspect_ratio` | string | 否 | `宽:高` 格式 | 空 = 自动：按人物图宽高取 6.3 里最近的一档 |

响应与 2.2 完全一样，`task_total` = `pose_image_urls` 的张数，任务号前缀 `pse`。

错误示例：

```json
{"code": 1002, "reason": "PARAM_MISSING", "msg": "人物图缺少地址", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "人物图还没上传完成，请等上传结束再提交", "data": null}
{"code": 1002, "reason": "PARAM_MISSING", "msg": "请上传姿势图", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "姿势最多 9 张", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "姿势图第 3 张地址不合法，只支持 http(s) 地址", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "补充说明不能超过 3000 字", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "单张说明的姿势序号 5 超出范围", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "姿势 2 的说明不能超过 3000 字", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "pose_prompts 的键应为姿势下标（从 0 起）：abc", "data": null}
```

### 3.2 图片生成

```
POST /api/v1/studio/image-generate/submit
Idempotency-Key: <UUID>
```

```json
{
  "workspace_id": 0,
  "prompt": "一只在窗边晒太阳的橘猫，胶片质感",
  "image_urls": [
    "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/ref1.jpg"
  ],
  "model": "GPT",
  "resolution": "2k",
  "aspect_ratio": "1:1",
  "count": 4,
  "rewrite_prompt": true
}
```

| 字段 | 类型 | 必填 | 限制 | 说明 |
|---|---|---|---|---|
| `workspace_id` | int | 否 | — | 不传 = 个人空间 |
| `prompt` | string | 是 | 非空；≤3000 字 | 提示词 |
| `image_urls` | string[] | 否 | ≤9 张；我方地址 | 参考图 |
| `model` | string | 否 | 四档之一 | 空 = `通用` |
| `resolution` | string | 否 | `1k` / `2k` / `4k`，大小写均可 | 空 = `1k` |
| `aspect_ratio` | string | 否 | `宽:高` 格式 | 空 = `1:1`（注意：商拍 / 姿态的空值是「按输入图自动」） |
| `count` | int | 是 | 1–9 | 生成张数，一张一个任务 |
| `rewrite_prompt` | bool | 否 | — | `true` = 先润色一次提示词再生成；润色会让批次多经过「反推中」这一步 |

响应与 2.2 完全一样，`task_total` = `count`，任务号前缀 `simg`。

错误示例：

```json
{"code": 1002, "reason": "PARAM_MISSING", "msg": "请输入提示词", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "提示词不能超过 3000 字", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "参考图最多 9 张", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "参考图第 1 张还没上传完成，请等上传结束再提交", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "生成张数为 1–9", "data": null}
```

---

## 4. 状态机与进度

### 4.1 批次状态 `batch.status`

| 值 | 含义 | 是否终态 |
|---|---|---|
| 0 | 排队 | 否 |
| 1 | 切图中（仅商拍） | 否 |
| 2 | 反推中（商拍 / 姿态；图片生成在这一步做参考图入库与 `rewrite_prompt` 润色） | 否 |
| 3 | 生图中 | 否 |
| 4 | 全部成功 | **是** |
| 5 | 全部失败 | **是** |
| 6 | 部分成功 | **是** |
| 7 | 已取消 | **是** |
| 8 | 提交中（逐张冻结点数；提交接口返回前就已转为 0，正常轮询基本看不到） | 否 |

非终态按 `0 → 1 → 2 → 3` 往前走（姿态、图片生成没有 1：`0 → 2 → 3`）。批次级失败（切图或反推整批失败）直接进 5，并在 `error_reason` 里写原因，全部点数退回；
反推 / 媒体服务暂时不可用时服务端会自动再试（最多 3 轮，每轮间隔约 5 秒），仍失败才整批失败。

全部任务到终态后批次的终态：全部成功 → 4；有成功有失败 / 取消 → 6；没有成功、有被取消的 → 7；没有成功、全部失败 → 5。

终态判断：`status` 为 4–7（**不要**用 `status >= 4`，8 是提交中）。

### 4.2 任务状态 `task.status`

| 值 | 含义 |
|---|---|
| 0 | 排队 |
| 1 | 生成中 |
| 5 | 交付中：已出图，正在转存与确认扣点（通常 1 秒内转为 2），按生成中展示 |
| 2 | 成功（`result_url` 有值） |
| 3 | 失败（`error_reason` 有值；点数已退回，例外见下表） |
| 4 | 已取消（点数已退回） |

任务失败原因 `error_reason`（可直接展示）：

| 文案 | 场景 | 点数 |
|---|---|---|
| `生成失败，点数已退回` / `内容未通过审核，点数已退回` / `输入图片不符合要求，点数已退回` / `生图服务繁忙，点数已退回` | 生图失败（限流、厂商 5xx、超时类会先自动重试 3 次，间隔 1.2 / 2.4 / 4.8 秒） | 退回 |
| `生成超时，点数已退回` | 单张超过 300 秒（服务端会主动取消上游调用） | 退回 |
| `提交生图失败，点数已退回` / `成品转存失败，点数已退回` / `生成结果为空，点数已退回` / `生成记录丢失，点数已退回` | 提交被拒、出图后转存失败等 | 退回 |
| 批次级原因（`反推服务暂不可用，点数已退回`、`反推服务未配置，点数已退回`、`图片下载或处理失败，点数已退回`、姿态人物图被拒时 sidecar 的原文 + `，点数已退回`） | 切图 / 反推整批失败 | 退回 |
| `运营强制失败，点数已退回` | 后台强制失败 | 退回 |
| `结果未知，待对账` | 上游结果无法确定（可能已产生费用）：服务端不主动退款，预留单不再续期，到期由回收器自动退回（studio-rpc 自带回收器，约 15 分钟） | 到期自动退回 |
| `点数已失效，请重新提交` | 出图了但点数预留已过期，不交付成品 | 不扣 |

### 4.3 进度口径

- **批次进度**由前端算：`(task_done + task_failed) / task_total`。`task_done` 是已成功数，`task_failed` 是已失败数（**含已取消**）。
- **任务进度** `progress` 是 0–100 的**估算值**，只用来画进度条：排队中的任务跟随批次阶段（排队 0 / 切图中 10 / 反推中 30 / 生图中 60），生成中至少 60，交付中 95，终态 100。失败 / 取消的任务不要使用 `progress`，直接按 `status` 展示。
- 单个任务的生成上限是 300 秒，超时记失败。

### 4.4 结果字段

| 字段 | 说明 |
|---|---|
| `result_url` | 成品图地址（已转存到我方公有桶，长期可用）。任务成功才有 |
| `result_width` / `result_height` | 成品像素尺寸，失败为 0 |
| `error_reason` | 失败原因，可直接展示 |
| `prompt_summary` | 提示词摘要（不是完整提示词） |
| `batch.cover_url` | 批次封面，历史列表缩略图用 |

---

## 5. 查询、轮询、历史、取消、删除

### 5.1 批次详情（轮询这个）

```
GET /api/v1/studio/batches/:id
```

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "batch": {
      "id": 1024,
      "kind": "tryon",
      "status": 3,
      "payload": "{\"items\":[\"top\",\"bottom\"],\"count_per_model\":2}",
      "task_total": 4,
      "task_done": 1,
      "task_failed": 0,
      "estimated_points": 12,
      "error_reason": "",
      "cover_url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/r1.jpg",
      "created_at": 1790755825451,
      "updated_at": 1790755885451
    },
    "tasks": [
      {
        "id": 5001, "task_id": "tyo_a1b2c3d4", "batch_id": 1024, "kind": "tryon",
        "model_index": 0, "shot_index": 0, "pose_index": 0,
        "status": 2, "progress": 100,
        "result_url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/r1.jpg",
        "result_width": 1536, "result_height": 2048,
        "error_reason": "", "prompt_summary": "上衣：真丝长袖，下装：棉麻及踝长裤……",
        "created_at": 1790755825451, "started_at": 1790755840000, "finished_at": 1790755885451
      },
      {
        "id": 5002, "task_id": "tyo_a1b2c3d5", "batch_id": 1024, "kind": "tryon",
        "model_index": 0, "shot_index": 1, "pose_index": 0,
        "status": 1, "progress": 40,
        "result_url": "", "result_width": 0, "result_height": 0,
        "error_reason": "", "prompt_summary": "上衣：真丝长袖，下装：棉麻及踝长裤……",
        "created_at": 1790755825451, "started_at": 1790755840000, "finished_at": 0
      }
    ]
  }
}
```

`batch` 字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | 批次号 |
| `kind` | string | `tryon` / `pose` / `imagegen` |
| `status` | int | 批次状态，见 4.1 |
| `payload` | string | 提交时的全量字段，**JSON 文本**（需 `JSON.parse`），用于回填表单，见 5.4 |
| `task_total` / `task_done` / `task_failed` | int | 任务总数 / 已成功 / 已失败（含取消） |
| `estimated_points` | int | 提交时的预估点数 |
| `error_reason` | string | 批次级失败原因；没有则空 |
| `cover_url` | string | 封面图；还没有成品时为空 |
| `created_at` / `updated_at` | int | unix 毫秒 |

`tasks[]` 字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | 任务主键 |
| `task_id` | string | 任务号（对外使用，如查询单个任务、任务中心） |
| `batch_id` | int | 所属批次 |
| `kind` | string | 同批次 |
| `model_index` | int | 商拍：模特序号（从 0 起）；其它类型为 0 |
| `shot_index` | int | 商拍：同一模特的第几张；图片生成：第几张（从 0 起） |
| `pose_index` | int | 姿态：姿势序号（从 0 起）；其它类型为 0 |
| `status` | int | 任务状态，见 4.2 |
| `progress` | int | 0–100 估算，见 4.3 |
| `result_url` / `result_width` / `result_height` | — | 成品，见 4.4 |
| `error_reason` | string | 失败原因 |
| `prompt_summary` | string | 提示词摘要 |
| `created_at` / `started_at` / `finished_at` | int | unix 毫秒；`started_at` / `finished_at` 为 0 = 未开始 / 未结束 |

错误：批次不存在、不是自己的、已删除，都返回 `code=4001`。

**轮询建议**

- 提交成功后立刻开始轮询 `GET /batches/:id`，**间隔 2 秒**；只轮询批次接口，不要对每个任务各轮询一次。
- 终态（`batch.status >= 4`，即 `done` / `failed` / `partial` / `canceled` 对应 4 / 5 / 6 / 7）就停止轮询。
- 页面切到后台时可放慢到 5–10 秒，切回来立即补一次。
- 网络错误或 `code=5001` 时指数退避（2s → 4s → 8s，封顶 10s），不要中断轮询；`code=4001` 表示批次没了，停止并提示。
- 一个批次最多 81 张，出完要几分钟，前端**不要**设置固定超时报错，以批次状态为准；批次级和任务级的超时由服务端处理。
- 每次轮询拿到的 `tasks` 是全量，直接整体替换本地状态，不需要增量合并。
- 通用的异步轮询说明见 `任务调用与轮询对接文档.md`；那份文档里的 `waiting/running/success` 是执行面的状态，studio 以本文 4.1 / 4.2 为准。

### 5.2 单个任务

```
GET /api/v1/studio/tasks/:task_id
```

`task_id` 是任务号字符串（如 `tyo_a1b2c3d4`）。返回单个 `tasks[]` 的结构（字段同上）。不存在或不属于当前用户：`code=4001`。一般不需要用，轮询批次接口就够了。

### 5.3 历史（游标分页）

```
GET /api/v1/studio/history?kind=tryon&cursor=&limit=20
```

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `kind` | string | 否 | `tryon` / `pose` / `imagegen`；不传 = 全部 |
| `cursor` | string | 否 | 上一页返回的 `next_cursor`；不传 = 第一页 |
| `limit` | int | 否 | 默认 20，上限 50 |

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "list": [
      {"batch": { "id": 1024, "kind": "tryon", "status": 4, "payload": "{…}", "task_total": 4, "task_done": 4, "task_failed": 0, "estimated_points": 12, "error_reason": "", "cover_url": "https://…/r1.jpg", "created_at": 1790755825451, "updated_at": 1790755985451 },
       "tasks": [ { "id": 5001, "task_id": "tyo_a1b2c3d4", "…": "…" } ]}
    ],
    "next_cursor": "1790755825451_1024"
  }
}
```

- 按创建时间倒序，每一项是「批次 + 它的全部任务」，结构和 5.1 的 `data` 完全一样。
- `next_cursor` 为空字符串 = 没有下一页。游标是不透明字符串，原样传回，不要自己拼或解析。
- 游标格式不对：`code=1003`，「游标不合法」。`kind` 不在三个取值内：`code=1003`，「类型只支持 tryon / pose / imagegen」。
- 已删除的批次不会出现。

### 5.4 「重新编辑」回填

历史项的 `batch.payload` 是提交时的**全量字段快照**（JSON 文本，字段名与提交请求体一致，原样返回，不做改写），点「重新编辑」时 `JSON.parse` 后直接回填表单：

| kind | 回填字段 |
|---|---|
| `tryon` | `model_image_urls`、`garment_images`、`items`、`set_mode`、`lengths`、`sleeves`、`fabric`、`prompt`、`model`、`resolution`、`aspect_ratio`、`count_per_model` |
| `pose` | `source_image_url`、`pose_image_urls`、`global_prompt`、`pose_prompts`、`model`、`resolution`、`aspect_ratio` |
| `imagegen` | `prompt`、`image_urls`、`model`、`resolution`、`aspect_ratio`、`count`、`rewrite_prompt` |

回填时注意：

- 快照里没有的字段当作未填，用默认值；不要假设每个字段都在。
- 回填后用户点「生成」是**新的一次提交**，必须新生成一个 `Idempotency-Key`，否则会拿到原批次（`replayed=true`）。
- 快照里的图片地址是当时的地址，素材库里已删除的图片可能失效，前端按图片加载失败处理即可。

### 5.5 取消批次

```
POST /api/v1/studio/batches/:id/cancel
```

无请求体。

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "batch": { "id": 1024, "kind": "tryon", "status": 3, "task_total": 4, "task_done": 1, "task_failed": 2, "…": "…" },
    "canceled_tasks": 2
  }
}
```

语义：

- **只取消还在排队的任务**（`task.status=0`）：它们改为「已取消」（4），点数立即退回。`canceled_tasks` 是这次取消的任务数。
- **已经在生成 / 交付的任务（`task.status=1` / `5`）不会被中止**，会继续跑完：成功就正常出图、正常扣点；失败就退点数。
- 取消后批次最终状态以 `GET /batches/:id` 为准（全部取消是 7，有成功的是 6），继续轮询到终态即可。
- 批次已经是终态（4–7）：`code=4002`（`RESOURCE_CONFLICT`），没有东西可取消。
- 批次还在提交中（8）：`code=4002`，「正在提交，请稍后再试」。
- 批次不存在或不是自己的：`code=4001`。

### 5.6 删除批次

```
DELETE /api/v1/studio/batches/:id
```

成功返回 `{"code":0,"msg":"ok","data":{}}`。

- 逻辑删除：批次和它的任务从历史里消失；已经入素材库的成品图不受影响。
- **进行中的批次不能删**（状态 0–3、8）：`code=4002`，「生成中不能删除」。要删先取消并等它到终态。
- 批次不存在或不是自己的：`code=4001`。

---

## 6. 模型、分辨率、比例

### 6.1 模型四档 `model`

`model` 传下表「传值」一列的名称（空 = `通用`）。服务端按名称映射到实际模型；名称**大小写不敏感**（`gpt`、`GPT` 都行），其它名称返回 `code=1003`「不支持的模型：xxx」。

| 传值 | 页面展示名（v1.0 契约） | 说明 |
|---|---|---|
| `通用` | 通用 | 运营在后台指定的默认模型，会随运营配置变化，不固定 |
| `GPT` | GPT Image 2 | |
| `Banana` | Nano Banana Pro | |
| `Seedream` | Seedream 5 Pro | |

三个功能（商拍、姿态、图片生成）都用这同一张表。**传的是上表的名称，不是内部模型 id**（姿态旧页面里的 `nano` / `sunburst` / `seedream` 等内部 id 不要再传）。

### 6.2 分辨率 `resolution`

| 传值 | 说明 |
|---|---|
| `1k` | 默认 |
| `2k` | |
| `4k` | |

**大小写归一**：`1K`、`1k` 都接受，服务端统一当小写处理，响应里 `payload` 回显的是小写。空 = `1k`。其它值返回 `code=1003`，「分辨率只支持 1k / 2k / 4k」。分辨率影响单价，见第 7 章。

**Seedream 档的修正**：Seedream 在 1k 下只支持 `1:1`、4k 会被上游静默降为 2k，所以服务端在**提交时**对 Seedream 把 4k 改为 2k、1k 且比例不是 `1:1`（含商拍 / 姿态比例留空 = 自动）改为 2k，并在提交响应 `warnings` 里说明。`payload` 回显、计费档位、实际生图参数（后台任务的 `prompt_meta.gen`）都用修正后的值，**按修正后的分辨率计费**。

### 6.3 比例 `aspect_ratio`

格式 `宽:高`，两边都是 1–999 的整数（如 `3:4`）。服务端只校验格式，**不逐值校验**，格式不对返回 `code=1003`，「比例格式不对，应为 宽:高，如 3:4」。

默认值：商拍、姿态 = 空，即**自动**：服务端按模特图 / 人物图的宽高取下表里最近的一档，并显式传给生图（实际值见后台任务的 `prompt_meta.gen`）；图片生成 = `1:1`。

前端下拉的 10 档候选（v1.0 契约写的是 10 档，具体取值以前端 `/features/image-model-configs` 返回为准，下表仅供参考）：

| 比例 | 比例 | 比例 | 比例 | 比例 |
|---|---|---|---|---|
| `1:1` | `3:4` | `4:3` | `2:3` | `3:2` |
| `9:16` | `16:9` | `4:5` | `5:4` | `21:9` |

---

## 7. 计费口径

- **按任务预留与结算**：每个任务（一张图）一张预留单。提交时对所有任务各冻结一次点数，任何一张冻结失败（如点数不足）**整批回滚**（已冻结的全部退回，批次记为 5「全部失败」），返回 `code=3003`。
- **预留续期**：批次未结束期间（含排队、切图、反推阶段），服务端每分钟给在途任务的预留单续期，排队再久也不会因预留过期而不交付。
- **成功才扣**：任务成功、成品转存完成后结算（任务先进入 5「交付中」再确认扣点，此后不会再被改为失败）；失败退回该任务的点数。
- **部分失败只退失败张**：N 张里成功几张就扣几张的钱，失败的、取消的那几张退回，批次状态为 6（部分成功）。
- **整批失败全退**：切图 / 反推等批次级步骤失败，批次进入 5，所有任务的点数退回。
- **取消**只退还没开始的（排队中）任务，已经在生成的会继续并按结果结算，见 5.5。
- **单价**取决于分辨率（1k / 2k / 4k），由运营在后台配置；三个功能各有独立的计费入口，价格可以不同。
- **`estimated_points`**：提交时的预估消耗，口径是「单价 × 任务数」（`task_total`），即这次提交一共冻结的点数。它是冻结额，**不是最终扣费**：有失败 / 取消的任务，最终扣费会比它少。前端提交成功后以它为准展示「已冻结 N 点」，最终余额以点数流水为准。
- **提交前报价**：页面上「消耗 N 点」调 7.1 的报价接口，与提交同一套模型 / 分辨率归一和计费入口，算出的就是提交会冻结的点数；最终仍以提交返回的 `estimated_points` 为准。

### 7.1 提交前报价（只读）

| 功能 | 报价接口 | 计价张数 `task_total` |
|---|---|---|
| 商拍换装 | `POST /tryon/quote` | `model_image_urls` 张数 × `count_per_model` |
| 姿态复刻 | `POST /pose/quote` | `pose_image_urls` 张数 |
| 图片生成 | `POST /image-generate/quote` | `count` |

- 请求体与对应的提交接口**同构**，可以直接把提交体发过来；只读取上表的张数字段和 `model` / `resolution` / `aspect_ratio`，其余字段忽略。字段全部选填，**不需要** `Idempotency-Key`。
- 只读：不建批次、不冻结、不查余额、不校验素材与准入（这些在提交时照常拦）。
- 素材还没传够时 `task_total=0`，`points=0`，`unit_points` 仍是单价。
- 报不出价（该入口没配价格、参数不合法）不报错，回 `available=false` 和 `reason`，前端不显示消耗。
- 建议输入变化后防抖 300ms 再调；不要阻塞提交按钮。

响应 `data`：

| 字段 | 类型 | 说明 |
|---|---|---|
| `available` | bool | 能否报出价；`false` 时点数为 0，原因见 `reason` |
| `points` | int | 本次提交预计冻结点数 = `unit_points` × `task_total`，与提交返回的 `estimated_points` 同口径 |
| `unit_points` | int | 每张点数 |
| `price_unit` | string | 计量单位，`PER_IMAGE` |
| `task_total` | int | 计价张数 |
| `resolution` | string | 计价分辨率，已按模型档归一（如 Seedream 4k → 2k） |
| `billing_mode` | string | 计费开关：`on` 真扣 / `shadow` 只算价不扣 / `off` 不计费（点数为 0） |
| `charged` | bool | 提交是否真的冻结扣点（仅 `billing_mode=on` 为 `true`）；前端只在它为 `true` 且 `points>0` 时显示「消耗 N 点」 |
| `reason` | string | `available=false` 的原因（给用户看的短句）；可用时为空串 |
| `warnings` | string[] | 参数归一告警，同提交；没有为 `[]` |

```json
// POST /api/v1/studio/image-generate/quote   {"model":"通用","resolution":"2k","aspect_ratio":"1:1","count":3}
{"code": 0, "msg": "ok", "data": {
  "available": true, "points": 45, "unit_points": 15, "price_unit": "PER_IMAGE", "task_total": 3,
  "resolution": "2k", "billing_mode": "on", "charged": true, "reason": "", "warnings": []
}}
// 没配价格
{"code": 0, "msg": "ok", "data": {"available": false, "points": 0, "unit_points": 0, "price_unit": "PER_IMAGE",
  "task_total": 3, "resolution": "2k", "billing_mode": "on", "charged": true, "reason": "该功能暂未配置价格", "warnings": []}}
```

---

## 8. 管理侧接口简表（给后台同事）

前缀 `/api/v1/studio/admin`（由 admin-api 处理，nginx 单独转发），鉴权为后台账号登录态，信封和错误码同第 1 章。权限码：GET 要 `studio:view`，POST（重做 / 强制失败）要 `studio:update`，缺了返回 `code=2005`「没有权限：studio:view」。
任务中心「本地生活任务」页对 `tyo:` / `pse:` / `simg:` 坐标的重做 / 强制失败走 `/api/v1/local-life/admin/tasks/:origin_ref/…`，入口权限 `local_life:update`，studio 坐标另外要 `studio:update`，行为同下。
权限按规范化后的路径判断（`//`、`..` 先清理），前缀按整段匹配。字段定义见 `app/admin/api/admin.api` 的 studio 组。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/batches` | 批次列表。查询参数：`page`（默认 1）、`page_size`（默认 20）、`user_id`、`kind`（`tryon` / `pose` / `imagegen`）、`status`（-1 = 全部）、`start_at` / `end_at`（创建时间，unix 毫秒，起含止不含）。返回 `list` + `total`，每项含用户、`request_id`、`cost_cny`（上游成本合计，人民币元）、`model_group`、`payload` 等 |
| GET | `/batches/:id` | 批次详情：`batch` + 切图框 `crop_plan`、切图质检 `crop_check`、组图 `selected_images`、每步耗时与成本 `step_log`（成本字段 `cost_cny`，均为 JSON 文本）+ 全部任务（含完整提示词 `prompt_text`、`prompt_meta`、`input_images`、`gateway_call_id`、`gateway_status`、`attempt`、`admin_locked`、`cost_cny`（人民币元）、`billing_request_id`、`billing_status`、`duration_ms`） |
| POST | `/tasks/:task_id/retry` | 重做子任务：只收失败 / 已取消、且没被强制失败封禁的；换新计费请求号（`studio-<task_id>-r<attempt>`）重新冻结、`attempt+1`、清空错误与 gateway 字段后回到排队，由轮询器用新幂等键重新提交。批次已终态时拉回生图中（还没反推过的拉回排队重新编排）；拉回出错时撤销本次重做（任务回原状态、新冻结退回）并报错，轮询器另有兜底。成功 / 进行中 / 批次提交中的返回 4002 |
| POST | `/tasks/:task_id/force-fail` | 强制失败并封禁重做（`admin_locked=1`）：排队 / 生成中的改为失败并退回点数（预留单若已扣费则走已确认退款），有上游调用的先尽力取消（取消时已出图则返回 4002、照常交付）；交付中（5）的返回 4002「任务正在交付，不能强制失败」；已失败 / 已取消的只封禁、不动账；已成功的返回 4002 |
| GET | `/sidecar/status` | 依赖状态：反推 sidecar / ai-gateway / 媒体服务的 `ok`、`latency_ms`、`version`、`detail`，外加 `running_tasks`、`queued_batches` |
| GET | `/cost-summary` | 成本汇总。查询参数：`start_at`、`end_at`、`group_by`（`kind` 或 `model_group`，默认 `kind`）。不给时间默认最近 30 天（按任务创建时间，起含止不含）。返回 `rows` 与 `total`，每行含 `task_total`、`task_succeeded`、`task_failed`（含取消）、`cost_cny`（上游成本，人民币元，取 gateway `call.cost.amount_cny`）、`points`（汇总查询不含预留单，目前恒为 0） |

任务状态 `status` 与用户侧同口径（0 排队 / 1 生成中 / 2 成功 / 3 失败 / 4 已取消 / 5 交付中）；批次状态另有 8 提交中；`billing_status`：0 未计费 / 1 已冻结 / 2 已结算 / 3 已退回 / 4 影子模式（不动账）。
