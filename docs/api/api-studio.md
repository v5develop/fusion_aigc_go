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
| 4001 | RESOURCE_NOT_FOUND | 批次 / 任务不存在、不属于当前用户或已删除；区域蒙版「没识别到对应区域」（5.8） | 返回上一页；蒙版接口提示 `msg` 后退回整图 |
| 4002 | RESOURCE_CONFLICT | 状态冲突：同 key 不同 body；取消已结束的批次；删除生成中的批次；重试条件不满足（见 5.2.1）；配对预览已在进行中；配对预览 / 区域蒙版超出频率配额（「识别太频繁，请稍后再试」，见 3.1.1、5.8） | 展示 `msg` |
| 6001 | THIRD_PARTY_ERROR | 区域识别服务暂不可用（5.8） | 展示 `msg`，可稍后重试 |
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
| `color_fix` | bool | 否 | — | 「默认校色」开关。`true` 时写进批次 `payload`（`"color_fix": true`），前端据此给这批结果套默认校色参数（见 5.7）；不影响生图与计费。不传 / `false` 时 `payload` 里没有这个键 |
| `fine_read` | bool | 否 | — | 「精细读图」（反推交付包验收 F7 / T2）：反推读图走 sidecar 精细模式——逐块检查领口、袖口、五金、刺绣等小细节，提示词【设计细节保留】最多多 3 条；读图更慢（单次约多 5–10 秒）。**不另收费**（交付包未定价，报价 / 冻结与不勾一致）。不勾时与旧版完全一致。`true` 时写进 `payload`（`"fine_read": true`） |
| `detail_crops` | object[] | 否 | 总数 ≤24、每张服装图 ≤12 | 「AI 找细节」确认后的细节框（2.4 的结果，用户删减 / 重新裁切后原样带回）。每项 `{garment_index, label, box, confidence}`：`garment_index` 为 `garment_images` 下标（从 0 起），`box` 为归一化 `[x, y, w, h]`（0–1、`x+w≤1`、`y+h≤1`），`label` 部位名 ≤24 字。只在 `detail_crops_manual=true` 时生效 |
| `detail_crops_manual` | bool | 否 | — | `true` = 用户确认过细节框：编排直接按 `detail_crops` 切细节图，**不再调 AI 找细节 / 质检**（`detail_crops` 为 `[]` = 用户删光了，不用细节图）；`false` / 不传 = 照旧由服务端自动找细节。生图时细节图仍按模型参考图上限截取（6.4），多出的不参与生成，提交响应 `warnings` 会说明「已选的 N 张中将保留 K 张」 |

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
| `warnings` | string[] | 参数修正提示，没有为 `[]`。如选了 Seedream 4k：`["Seedream 不支持 4K（会被降为 2K），已按 2K 生成"]`，见 6.2；商拍还会在服装图放不下 / 细节图减少时给提示，见 6.4；可直接展示。幂等重放（`replayed=true`）不回 |

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

### 2.4 AI 找细节 / 裁切缩略图 / 重新裁切（提交前，免费）

```
POST /api/v1/studio/tryon/details
```

对服装图找细节（反推交付包验收 F10 / F33：标签按图片内容生成，不是固定区域），返回每张图的细节框；**不建任务、不扣点、不落库**。前端按框在原图上本地显示裁切缩略图（CSS 背景定位 / canvas，**不调任何裁图接口**），用户可删除单张、「重新裁切」（再调一次本接口，结果整体替换）、清空；提交时把剩下的框原样放进 `detail_crops`，并带 `detail_crops_manual: true`。

请求体（字段与 2.1 同口径、同一套校验，只用这几项）：

```json
{
  "items": ["top", "shoes"], "set_mode": "two",
  "garment_images": [{"url": "https://…/shoes.jpg", "role": "鞋子"}, {"url": "https://…/top.jpg", "role": "上衣", "angle": "front"}],
  "fabric": "上衣：真丝", "prompt": "左肩金属扣"
}
```

响应：

```json
{
  "code": 0,
  "data": {
    "plans": [
      {"garment_index": 0, "angle": "unknown", "crops": []},
      {"garment_index": 1, "angle": "front", "crops": [
        {"garment_index": 1, "label": "上衣·领口", "box": [0.3, 0.1, 0.4, 0.2], "confidence": 0.86},
        {"garment_index": 1, "label": "上衣·袖口", "box": [0, 0.5, 0.2, 0.2], "confidence": 0.7}
      ]}
    ],
    "total": 2,
    "warnings": ["第 1 张服装图没有找到细节，可以点「重新裁切」再试"]
  }
}
```

| 字段 | 说明 |
|---|---|
| `plans[]` | 每张服装图一项，顺序同 `garment_images`；没找到细节的 `crops` 为 `[]` |
| `plans[].crops[]` | 已按编排同一套规则校验：框合法、部位名带单品前缀（不串件）、同图同名去重；每张 ≤12、总数 ≤24，原样带回提交一定能通过校验 |
| `warnings` | 给用户看的提示（全部没找到 / 部分图没找到），没有为 `[]` |

限流与错误：每用户每分钟 6 次、每天 200 次（网关 `Studio.DetailsPerMinute / DetailsPerDay`），超出 `4002`「识别太频繁，请稍后再试」；同一用户上一次还没结束 `4002`「上一次 AI 找细节还在进行中，请稍候」；读图名额与姿态配对预览共享（不挤占付费编排）。地址不是本服务签发 `4000`；服装图识别不了 `4000`（中文原因，不透出上游原文）；识别服务不可用 / 超时 `6001`。耗时与配对预览同级（单次读图可达 1–3 分钟），**前置代理读超时同 3.1.1 的要求（≥260 秒）**。

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
| `source_person_count` | int | 传 `mappings` 时必填 | 2–4 | 3.1.1 预览返回的 `source.person_count` 原样带回 |
| `mappings` | object[] | 否 | 每张姿势图至多一条 | 多人配对（3.1.1 用户确认 / 反转后的结果）。不传的姿势图按默认配对（左→左、右→右） |
| `color_fix` | bool | 否 | — | 「默认校色」开关，同商拍 2.1 |
| `mappings[].pose_index` | int | 是 | `0` ~ 姿势图张数-1，不重复 | 姿势序号 |
| `mappings[].mapping` | int[] | 是 | 长度 = `source_person_count`；值在 `-1` ~ `pose_person_count-1`；非负值不重复；至少一个非负值 | `mapping[i]` = 原图第 i 位人物（左→右）学姿势图第几位人物（左→右，从 0 起）；`-1` = 保持原姿势 |
| `mappings[].pose_person_count` | int | 是 | 1–4 | 3.1.1 预览返回的该姿势图 `pose.person_count` 原样带回 |

响应与 2.2 完全一样，`task_total` = `pose_image_urls` 的张数，任务号前缀 `pse`。

配对说明：人数与人物顺序以预览为准。生成时**原图直接复用预览读到的那份结果**（同一用户、同一张原图、预览后 2 小时内），人物左→右顺序就是用户确认配对时看到的；超过 2 小时或换了图才会重新读原图，这次读出的人数与预览不同时，这组配对已指不到同一批人，服务端放弃配对、按默认配对生成，并在该任务的告警里写明。姿势图生成时会重新读一次：读出的人数与预览不同，同样放弃配对、按默认配对重读生成并记告警。`-1` = 该人物保持原姿势，按用户的选择原样生效，不会被补成默认配对（如三人 `[0,1,-1]`、两人 `[0,-1]`）；只有配对无效（越界、重复、全 `-1`）时反推服务才整组退回默认配对，并记告警「配对被反推服务校正」。

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
{"code": 1003, "reason": "PARAM_INVALID", "msg": "人物配对需要原图人数（2–4），请先预览配对", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "姿势 1 的人物配对应为 2 位（与原图人数一致），实际 3 位", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "姿势 1 的人物配对第 1 位取值 2 超出范围（-1 到 1）", "data": null}
{"code": 1003, "reason": "PARAM_INVALID", "msg": "姿势 1 的人物配对里姿势图人物 1 被重复使用", "data": null}
```

#### 3.1.1 多人配对预览（付费前确认）

```
POST /api/v1/studio/pose/preview
```

不计费、不生成批次 / 任务，不要 `Idempotency-Key`（服务端会把原图的读图结果按 用户 + 原图存一份缓存（`afv_studio_prompt_cache`，`batch_id = -用户id`，24 小时后清理），提交后编排直接复用，见下方配对说明）。服务端下载人物图与姿势图（与提交同一套地址校验），读原图的人物（1–4 位，按画面左→右编号 P1…P4）；再逐张读姿势图，返回每张姿势图的人数；**原图 ≥2 人时**另给默认配对（`mapping_enabled=true`）。原图单人时不需要配对（`mapping_enabled=false`、`default_mapping` 为 `[]`），但 `poses[]` 仍逐张给出 `pose.person_count`：姿势图人数 >1 时前端可提示「姿势图里多出的人物会被忽略」，然后直接提交即可（不用带 `mappings`）。

耗时：读图单次 20–140 秒，整个请求最长约 4 分钟（路由超时 250s），前端要有加载态，别设太短的请求超时；同一用户同一时间只能有一个预览在跑（重复请求回 `code=4002`）。

频率配额：每个用户每分钟最多 6 次、每天（北京时间自然日）最多 200 次（网关 `Studio.PosePreviewPerMinute` / `Studio.PosePreviewPerDay` 可调，多实例共享计数，计在会话 Redis）。超出回 `code=4002`、`msg`「识别太频繁，请稍后再试」，不会读图；被拒的请求不占额度，每分钟的额度到下一分钟恢复、每天的额度次日零点恢复。每次放行的请求都计一次（不论预览成功与否），前端不要在用户没换图时自动重复预览。姿势图按序号顺序读，且全站预览共用少量读图名额（`Reverse.PreviewConcurrency`，默认 2，不挤占付费生成）：时间不够时已读完的照常返回，没来得及读的那几张在 `poses[].error` 里写「姿势图识别超时，请稍后重试」，整个请求不报错（可只对这几张重新预览）。

**部署注意**：前置代理对 `/api/v1/studio/pose/preview` 的读超时必须 ≥ 260 秒（nginx `proxy_read_timeout 260s` 或更长；仓库模板 `deploy/server/nginx/aigc-local-life.conf.tmpl` 在 server 级已是 300s，宝塔 / CDN / SLB 等另有一层代理时同样要放宽），否则代理先断开、前端拿到 504 而服务端还在读图。

```json
{
  "source_image_url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/image/123/20261030/two.jpg",
  "pose_image_urls": ["https://…/p1.jpg", "https://…/p2.jpg"]
}
```

响应：

```json
{
  "source": {
    "person_count": 2,
    "people": [
      {"label": "P1", "position": "画面左侧", "primary": false, "gender_age": "成年女性", "summary": "灰色条纹西装外套；灰色短裤"},
      {"label": "P2", "position": "画面右侧", "primary": true, "gender_age": "成年女性", "summary": "米白条纹西装外套；米白短裤"}
    ],
    "group": "两人并肩，P1 在左，P2 在右",
    "interaction": "无",
    "people_truncated": false
  },
  "poses": [
    {
      "pose_index": 0,
      "pose": {"person_count": 2, "people": [{"label": "P1", "position": "画面左侧", "primary": false, "gender_age": "", "summary": "侧身站立，一臂抬至颈前"}, {"label": "P2", "position": "画面右侧", "primary": true, "gender_age": "", "summary": "正面站立，双手叉腰"}], "group": "两人一前一后", "interaction": "背靠背", "people_truncated": false},
      "default_mapping": [0, 1],
      "error": ""
    },
    {"pose_index": 1, "pose": null, "default_mapping": [], "error": "姿势参考图中没有识别到人物，请换一张"}
  ],
  "mapping_enabled": true
}
```

| 字段 | 说明 |
|---|---|
| `source.person_count` / `poses[].pose.person_count` | 人数（最多 4；`people_truncated=true` 表示超过 4 人只分析了前 4 位） |
| `people[]` | 按画面左→右；`summary` 原图为服装摘要、姿势图为动作剪影；`gender_age` 只有原图人物有 |
| `poses[].default_mapping` | 默认配对，长度 = 原图人数：`[0,1]` 表示人物 A ← 姿势图 P1、人物 B ← 姿势图 P2；参考图只有 1 人时配给原图主要人物，其余为 `-1` |
| `poses[].error` | 该姿势图读图失败的原因（只影响这一张）；成功为空串、`pose` 有值 |
| `mapping_enabled` | 原图 ≥2 人：显示配对行与「反转配对」；`false` 时只用 `source.person_count` / `poses[].pose.person_count` 做人数提示（2026-11-07 起单人原图也返回 `poses[]`，此前为 `[]`；只增不改） |

前端用法：每张姿势图下展示「人物A ← 图2画面左侧 …」；两边人数相同且 ≥2 时给「反转配对」（把 `default_mapping` 反过来，如 `[1,0]`）。参考图人多：多出来的人不进画面；原图人多：没配上的人保持原姿势。提交时把用户改过（或想固定）的姿势图写进 `mappings`，并原样带上 `source_person_count` 与各自的 `pose_person_count`。

错误：人物图无人物 / 疑似未成年等原图问题回 `code=1003` 与原因；反推服务不可用 `code=6001`；未开放 `code=7003`；同一用户已有预览在跑 `code=4002`（不变）；超出频率配额 `code=4002`「识别太频繁，请稍后再试」。

无人物的文案固定为两句（`msg` / `poses[].error`，码不变）：原图 →「原图中没有识别到人物，请换一张」（整个预览 `code=1003`）；姿势图 →「姿势参考图中没有识别到人物，请换一张」（只写在该张的 `poses[].error`）。提交后编排阶段同样原因失败时，批次 `error_reason` 为这两句加「，点数已退回」。

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
| `result_url` | 成品图地址。付费用户：已转存到我方公有桶，长期可用。免费档用户（开启免费水印时）：带水印的**短期签名地址**（默认 10 分钟有效，每次查询现签，见 4.5），不要缓存或持久化，过期重新查询即可；签不出时为空串。任务成功才有 |
| `result_width` / `result_height` | 成品像素尺寸，失败为 0 |
| `error_reason` | 失败原因，可直接展示 |
| `prompt_summary` | 提示词摘要（不是完整提示词，去掉了首行图序） |
| `prompt_text` | 实际送生图的**完整**提示词（含首行图序声明，如「图序：图1 是原图，图2 是姿势参考图。」）；姿态复刻 / 商拍换装有，编排完成前为空，图片生成为空（提示词是用户自己写的）。姿态复刻在结果卡逐张展示、可「复制」「编辑后重新生成」（5.2.2）；商拍只读 |
| `prompt_edited` | 这张图的提示词经用户编辑后重新生成过 |
| `warnings` | 给用户看的提示（中文、不含供应商 / 网关原文，直接展示在结果卡上）：读图降级「读图失败，已改用文字信息生成」/「读图失败，已按模板生成提示词，建议检查服装图后重试」、品类不符「参考图看起来是针织毛衣，与所选「文胸」不一致，请确认品类」（多件组合：「参考图里没有找齐所选的「上衣、下装」（识别为…），请确认品类或补齐服装图」）、参考图截断（6.4）、姿态配对回退「人物数量与预览时不一致，已按默认配对生成」；没有为 `[]` |
| `batch.warnings` | 本批各任务 `warnings` 去重合并，批次级提示条用；没有为 `[]` |
| `asset_id` | 成品在素材库的资产 id（成功的任务入库后才有，没入库 / 已从素材库删除为 0），见 5.7 |
| `color_fix_params` | 已保存的校色参数，没保存过为 `null`，见 5.7 |
| `mask_url` | 服装蒙版（白色 + 软 alpha 的 PNG，公开地址），商拍换装 / 姿态复刻成品成功后异步补；空 = 没有，见 5.8 |
| `batch.cover_url` | 批次封面，历史列表缩略图用（免费档同 `result_url`，是带水印的短期签名地址） |

### 4.5 免费档水印

与 Java 主项目同一口径（`app.free-watermark`，读时参数水印）。前端无需区分用户档位，照常展示 / 下载 `result_url` 即可：

- **谁是免费档**：从未收到过付费来源点数批次的用户（`afv_points_batch` 中没有 `source ∈ {RECHARGE, UNION_RECHARGE}` 且未删除的行），且后台的分层开关 `user_tier.enabled`、图片展示水印开关 `watermark.display_enabled`（`afv_system_config`，没配时默认开）都开着。只看「有没有付过」，不看余额；赠送、后台调整、退款回补都不算付费。实现：`pkg/usertier`（与 Java `UserTierService` + `WatermarkPolicy` 逐条一致）。
- **交付**：studio 开关 `FreeWatermark.Enabled` 开、`FreeWatermark.Process` 合法时，免费档用户的成品**不转存公有桶**：原件留在私有桶（网关产物 `media_purpose=intermediate`，媒体服务 `work` 类 rid），`afv_studio_task.result_url` 与素材库 `afv_asset.result_url / cover_url` 写水印视图引用 `mrw://{rid}.{ext}`。付费用户、开关关闭时与原来一样（公有桶长期地址）。
- **读取**：批次详情 / 历史 / 单个任务 / 重试 / 取消 / 校色 / 后台详情的 `result_url`、`batch.cover_url` 遇到 `mrw://` 一律签成带 `x-oss-process=<处理串>` 的签名地址：处理参数在签名内，去掉或改动即 403，拿不到干净原图。签名失败或未配处理串 → 空串（绝不下发干净地址或 `mrw://` 原文）。下载用同一个地址（带水印）。素材库（assetlib / Java）读到 `mrw://` 同样签水印地址。
- **蒙版**：成品服装蒙版在原图上识别（服务端用短期干净签名交给网关，不落库、不下发），`mask_url` 照常是公开地址。`POST /masks` 的 `image_url` 传水印地址时按地址路径（不含查询串）命中同一缓存；若私有桶域名不在本服务存储 / `InputURLPrefixes` 白名单内会被拒（1003），需运维把私有桶域名加进 `STUDIO_INPUT_URL_PREFIXES`。
- **付费后**：studio-rpc 每 `FreeWatermark.PromoteMinutes`（默认 10 分钟）扫描一次 `result_url LIKE 'mrw://%'` 且用户已付过钱的子任务，把私有原件复制到公有素材库桶，先改素材库行、再按旧值 CAS 改子任务的 `result_rid / result_url`；任务中心随 `updated_at` 同步。之后该用户的历史成品与素材库都是无水印原图。私有原件不删除（与 Java 不同，Java 转存后会删）。
- **配置**（`deploy/etc/studio-rpc.yaml` `FreeWatermark`）：`Enabled`（默认 false，= Java `FREE_WATERMARK_ENABLED`）、`Process`（= Java `FREE_WATERMARK_OSS_PROCESS` / 启动日志 `check=free_watermark.process` 的值，assetlib 与 canvas 的 `FreeWatermark.Process` 也必须相同，由同一个 `FREE_WATERMARK_OSS_PROCESS` 一次写入三处）、`SignedTTLSeconds`（600）、`UserTierDefault` / `DisplayWatermarkDefault`（true，DB 没配开关时的默认）、`PromoteMinutes`（10，≤0 关闭）、`CleanupEnabled`（默认 false）/ `RetentionDays`（90）/ `CleanupMinutes`（60）为未付费原件到期清理。服务器上由 `deploy.env` 的 `FREE_WATERMARK_ENABLED` / `FREE_WATERMARK_OSS_PROCESS` 经 mkconf 写入。

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
        "created_at": 1790755825451, "started_at": 1790755840000, "finished_at": 1790755885451,
        "asset_id": 88001,
        "color_fix_params": {"hue": 3, "saturation": -16, "value": -6, "brightness": -17, "contrast": 10}
      },
      {
        "id": 5002, "task_id": "tyo_a1b2c3d5", "batch_id": 1024, "kind": "tryon",
        "model_index": 0, "shot_index": 1, "pose_index": 0,
        "status": 1, "progress": 40,
        "result_url": "", "result_width": 0, "result_height": 0,
        "error_reason": "", "prompt_summary": "上衣：真丝长袖，下装：棉麻及踝长裤……",
        "created_at": 1790755825451, "started_at": 1790755840000, "finished_at": 0,
        "asset_id": 0, "color_fix_params": null
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
| `asset_id` | int | 成品在素材库的资产 id（`afv_asset.id`）。成功的任务入库后才有；入库与置成功不在同一步，刚成功的一两次轮询可能还是 0；素材库入库关闭、或用户已在素材库删除该图时为 0。用途见 5.7 |
| `color_fix_params` | object \| null | 已保存的校色参数 `{hue, saturation, value, brightness, contrast}`；没保存过为 `null`。见 5.7 |
| `mask_url` | string | 服装蒙版地址；空 = 还没有 / 没识别到服装（校色作用于整图）。成功后通常几秒内补上，轮询会带出。见 5.8 |
| `mask_parts` | object[] | 商拍换装按所选单品分割时的逐件蒙版 `[{part, mask_url}]`（`mask_url` 是它们的合并）；没有为空数组。见 5.8 |
| `warnings` | string[] | 给用户看的提示（读图降级 / 品类不符 / 参考图截断 / 配对回退），见 4.4；没有为 `[]` |
| `prompt_text` | string | 实际送生图的完整提示词（姿态 / 商拍），见 4.4 |
| `prompt_edited` | bool | 提示词经用户编辑后重新生成过 |

`batch` 另有 `warnings`（string[]）：各任务 `warnings` 去重合并。

错误：批次不存在、不是自己的、已删除，都返回 `code=4001`。

**轮询建议**

- 提交成功后立刻开始轮询 `GET /batches/:id`，**间隔 2 秒**；只轮询批次接口，不要对每个任务各轮询一次。
- 终态（`batch.status >= 4`，即 `done` / `failed` / `partial` / `canceled` 对应 4 / 5 / 6 / 7）就停止轮询。
- 页面切到后台时可放慢到 5–10 秒，切回来立即补一次。
- 网络错误或 `code=5001` 时指数退避（2s → 4s → 8s，封顶 10s），不要中断轮询；`code=4001` 表示批次没了，停止并提示。
- 一个批次最多 81 张，出完要几分钟，前端**不要**设置固定超时报错，以批次状态为准；批次级和任务级的超时由服务端处理。
- 每次轮询拿到的 `tasks` 是全量，直接整体替换本地状态，不需要增量合并。
- 服务端推进（只影响延迟，接口不变）：每个生成中的子任务一直对网关长轮询（`GET /v1/calls/{id}?wait=AiGateway.LongPollWait`，默认 20 秒）直到终态，出图后约 0–1 秒内开始交付；轮询名额取 `max(Poll.Workers, Poll.MaxRunning)`（默认都是 64）。一个子任务到终态立刻派发同批的下一个排队子任务（每批并发 `Poll.Concurrency`，默认 3），不等下一轮扫描（`Poll.IntervalSeconds`，5 秒，只兜底）。编排名额按 kind 分池（`Poll.KindBatchConcurrency`：商拍 2 / 姿态 2 / 图片生成 4），商拍几分钟的切图 + 反推不再挤占图片生成。提交时逐张冻结按 `Poll.HoldConcurrency`（默认 4）并发；任一张冻结失败，已冻结的每张恰好退回一次，批次与子任务置失败，接口照旧返回失败原因。任务中心同步（2026-11-11）移出提交接口与批次收敛路径：异步合并执行、暂时失败重试 2 次，后台每轮扫描兜底；新批次 / 终态出现在任务中心可能晚 1–2 秒，提交接口少等一次全量窗口同步。
- 通用的异步轮询说明见 `任务调用与轮询对接文档.md`；那份文档里的 `waiting/running/success` 是执行面的状态，studio 以本文 4.1 / 4.2 为准。

### 5.2 单个任务

```
GET /api/v1/studio/tasks/:task_id
```

`task_id` 是任务号字符串（如 `tyo_a1b2c3d4`）。返回单个 `tasks[]` 的结构（字段同上）。不存在或不属于当前用户：`code=4001`。一般不需要用，轮询批次接口就够了。

### 5.2.1 重试单个失败任务

```
POST /api/v1/studio/tasks/:task_id/retry
```

无请求体，不要 `Idempotency-Key`（幂等见下）。只重跑**这一张图的生图一步**：沿用提交时已存的提示词、图序和生图参数（比例 / 分辨率 / 质量），不再切图、不再反推（没有额外读图成本）。按原入口、原档位重新冻结 **1 张**的点数（新计费单号 `studio-<task_id>-r<n>`）；上一次失败时已退回的点数不再动。出图后结算新单，再次失败则退回新单。

响应：批次详情，结构同 5.1 `GET /batches/:id`。成功时该任务 `status=0`（排队）、`error_reason` 清空，批次 `status=3`（生图中）、`task_failed` 减 1；前端拿到后继续轮询批次即可看到 排队 → 生成中 → 成功 / 失败，批次终态由服务端重算。

同一批次里有多张失败时可以**连着重试**，不用等上一张出结果：第一张重试后批次回到生图中，其余失败的照样可以点重试（每张各冻结 1 张点数）。批次在所有子任务都到终态后才按实际结果重算为 全部成功 / 部分成功 / 全部失败，`task_done` / `task_failed` 也按实际计数重算。

```json
{
  "code": 0,
  "data": {
    "batch": {"id": 1024, "kind": "pose", "status": 3, "task_total": 3, "task_done": 2, "task_failed": 0, "...": "…"},
    "tasks": [
      {"task_id": "pse_a1b2c3d4", "status": 2, "progress": 100, "...": "…"},
      {"task_id": "pse_e5f6a7b8", "status": 0, "progress": 60, "error_reason": "", "...": "…"}
    ]
  }
}
```

可重试的条件（都满足）：

| 条件 | 不满足时 |
|---|---|
| 任务属于当前用户、未删除 | `4001` |
| 任务状态为失败（3）；已取消（4）的不提供重试 | `4002`「只有失败的任务可以重试」 |
| 批次为全部失败（5）/ 部分成功（6）；或生图中（3）但在跑的只有重试（同批其它任务的重试 / 运营重做） | 首次生成还没结束（排队 / 切图 / 反推中，或生图中仍有首轮任务在跑）`4002`「批次还在生成中，请等全部结束后再重试」；已取消（7）`4002`「批次已取消，不能重试」 |
| 该任务已有生图提示词（切图 / 反推阶段整批失败的没有） | `4002`「这张图还没生成提示词，无法单独重试，请重新提交」 |
| 该任务累计重做 <3 轮（含运营重做） | `4002`「这张图已重试多次，请调整素材后重新提交」 |
| 未被运营强制失败封禁 | `4002`「该任务已停止重试，请重新提交」 |
| 上一次的计费单已了结（已退回 / 已过期 / 本来没冻结）；结果未知待对账、仍冻结中的不行 | `4002`「上一次的点数还在结算中，请稍后再试」 |
| 点数足够 | `3003`（先按报价只读核对余额，不够直接拒绝，**不占重试次数**；核对通过后冻结仍失败（如同时在别处花掉了点数）也会把这次的次数退还） |

幂等：重复点击时，若该任务已经在这次重试里（排队 / 生成中 / 交付中，且计费单号是重做轮次），直接返回当前批次详情，不会再冻结；两个请求同时到达也只有一个会冻结点数。前端建议按钮点击后置为加载态，拿到响应后以批次轮询结果为准。

### 5.2.2 编辑提示词后重新生成（姿态复刻）

```
POST /api/v1/studio/tasks/:task_id/regenerate
{"prompt": "【任务】把图1人物摆成图2姿势，右手叉腰……"}
```

姿势裂变交付包验收 C2 / C4 / C16：每张姿势卡展示该张的 `prompt_text`（可复制、可编辑），编辑后点「重新生成」只重跑这一张。`prompt` 必填、≤3000 字、过内容审核；首行「图序：」可带可不带，服务端统一重写为「图序：图1 是原图，图2 是姿势参考图。」。

- **只支持姿态复刻**（商拍提示词只读，调用返回 `4000`）；任务属于当前用户；**失败或成功**的任务都可以（成功的旧成品已在素材库，任务改由新结果覆盖，新成品照常入库）。
- **计费同 5.2.1 重试**：按原入口、原档位重新冻结 1 张（新计费单号 `studio-<task_id>-r<n>`）；成功任务的原单已结算（那张图的钱），不退不动；出图结算新单，失败退回新单。
- 条件与 5.2.1 相同（批次状态、封禁、余额 `3003` 不占次数），差别：成功任务可用；在途的任务 `4002`「这张图正在生成中，请等结束后再修改提示词」；累计轮次上限 6（含重试 / 运营重做），超出 `4002`「这张图已重新生成多次，请调整素材后重新提交」。
- **幂等**：同一段提示词重复提交（任务已按它在重做中）直接回当前批次详情，不再冻结；在途时提交另一段提示词 `4002`。
- 响应同 5.2.1（批次详情），该任务 `status=0`、`prompt_text` 为新提示词、`prompt_edited=true`、`warnings` 清空；继续轮询批次。

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
| `tryon` | `model_image_urls`、`garment_images`、`items`、`set_mode`、`lengths`、`sleeves`、`fabric`、`prompt`、`model`、`resolution`、`aspect_ratio`、`count_per_model`；开了「默认校色」的批次另有 `color_fix: true`；勾了「精细读图」另有 `fine_read: true`；确认过细节框另有 `detail_crops_manual: true` + `detail_crops`（重新编辑时若换了服装图，要清掉或重新找细节） |
| `pose` | `source_image_url`、`pose_image_urls`、`global_prompt`、`pose_prompts`、`model`、`resolution`、`aspect_ratio`；带了多人配对的批次另有 `source_person_count`、`mappings`（重新编辑时若换了图，要重新预览）；开了「默认校色」的批次另有 `color_fix: true` |
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

### 5.7 校色（商拍换装 / 姿态复刻结果页）

校色在**前端浏览器里**完成：五个滑杆值烘进像素、导出 PNG。服务端不处理像素，只提供三件事：

1. **「默认校色」开关**：提交时带 `color_fix: true`（2.1 / 3.1），批次详情 `batch.payload` 里原样回显 `"color_fix": true`；前端初始化这批结果时，`color_fix_params` 为 `null` 的任务套默认参数，已保存过的用保存值。
2. **存成素材库版本**：成功的任务带 `asset_id`（该成品在素材库的资产）。前端把烘好的 PNG 先用 Go 上传（`goUploadImage`，地址须为本服务签发），再调资产库已有接口
   `POST /api/v1/asset-library/assets/{asset_id}/versions`（`{"image_url": "…", "params": "{\"color_fix\":{…}}"}`，见 `api-asset-library.md`）追加「校色后」版本。
   归属校验与素材库一致：资产须是本人、未删除；studio 成品入库时 `user_id` 就是提交人，所以本人的 `asset_id` 都能追加版本。`asset_id` 为 0 时（刚成功还没入库 / 已从素材库删除）不要调用。
3. **保存参数（可选）**：

```
PATCH /api/v1/studio/tasks/:task_id/color-fix
```

```json
{"params": {"hue": 3, "saturation": -16, "value": -6, "brightness": -17, "contrast": 10}}
```

| 字段 | 类型 | 范围 | 前端 `colorfix.ts` 键名 |
|---|---|---|---|
| `params.hue` | int | -30 – 30 | `hue`（色相） |
| `params.saturation` | int | -60 – 70 | `sat`（饱和度） |
| `params.value` | int | -40 – 40 | `light`（明度） |
| `params.brightness` | int | -80 – 80 | `bright`（亮度） |
| `params.contrast` | int | -50 – 50 | `contrast`（对比度） |

五个键都要传、只收整数（滑杆步长 1）；全 0 = 不校色（也会保存）。成功返回该任务（结构同 5.1 `tasks[]`，`color_fix_params` 为刚保存的值），之后批次详情 / 历史 / 单个任务都会带上。每次保存整体覆盖。

| 情况 | 返回 |
|---|---|
| 某个值越界 | `1003`「色相（hue）取值范围 -30–30」（报第一个越界的） |
| 缺 `params` 或缺某个键 | `1002` / `1003` |
| 任务不存在、不是自己的、已删除 | `4001` |
| 任务不是成功状态（2） | `4002`「只有生成成功的图片可以保存校色参数」 |

校色默认只作用于服装：任务带 `mask_url` 时前端按蒙版合成（见 5.8）；没有时整图校色并提示「未识别到服装区域，校色将作用于整张图」。

### 5.8 区域蒙版（局部校色）

蒙版 = 与原图同尺寸的 PNG：**白色 + 软 alpha**（区域内不透明、区域外透明、边缘半透明，不要二值化）。前端合成顺序：
原图 → 校色层（canvas filter）→ `destination-in` 蒙版 → 盖回原图。蒙版是公开地址（素材库桶），canvas 读像素需 `crossOrigin="anonymous"`。

**① 子任务自带服装蒙版 `mask_url`**：商拍换装 / 姿态复刻的成品置成功后，服务端**异步**识别服装（不拖慢、不影响交付与计费），
写进任务的 `mask_url`。识别失败 / 图里没有服装时一直为空，前端整图校色。图片生成任务不自动补，需要时调 ②。
商拍换装按用户选的单品分割：`mask_url` 是**恰好这些单品**的合并蒙版，`mask_parts[{part, mask_url}]` 是逐件蒙版（前端可按件切换）。
单品 → 部位：`top`/`bra`→`tops`、`bottom`/`panties`→`pants`、`dress`→`skirt`、`outer`→`coat`、`shoes`→`shoes`、`accessory`→`bag`+`hat`。
按单品识别服务不可用时退回整体服装蒙版（此时 `mask_parts` 为空）；姿态复刻始终是整体服装。

**② 通用蒙版接口**（任意页面、任意本服务生成或上传的图）：

```
POST /api/v1/studio/masks
```

```json
{"image_url": "https://cdn.example.com/…/result.png", "target": "garment", "parts": ["tops", "pants"]}
```

| 字段 | 说明 |
|---|---|
| `image_url` | 必填。本服务存储签发或素材库域名的 http(s) 地址（如任务 `result_url`、素材库图片）；第三方地址拒收（1003） |
| `target` | 必填。`garment` 服装 / `product` 商品主体 / `person` 人物 / `background` 背景（主体之外，= 主体蒙版取反） |
| `parts` | 选填，仅 `garment`（多选）：`tops` 上衣 / `coat` 外套 / `pants` 下装 / `skirt` 裙装 / `shoes` 鞋 / `bag` 包 / `hat` 帽；不传 / 空 = 全部服装。其它值或非服装区域带 `parts` 返回 `1003` |

```json
{"code": 0, "data": {"mask_url": "https://cdn.example.com/…/mask.png", "target": "garment", "cached": false,
  "parts": [{"part": "tops", "mask_url": "https://cdn.example.com/…/tops.png"}, {"part": "pants", "mask_url": "https://cdn.example.com/…/pants.png"}],
  "available_parts": ["tops", "coat", "pants", "skirt", "shoes", "bag", "hat"]}}
```

- **部位**：带 `parts` 时 `mask_url` = 这些部位的合并蒙版，`parts` = 逐件蒙版（固定顺序，图里没有的部位缺省；全都没有 → `4001`）；不带为空数组。
  `available_parts` = 该区域支持的部位（只有 `garment` 有，其它区域空数组）；**旧版服务端没有这个字段**，前端据此显示 / 隐藏部位选择。
- **缓存**：按（去掉查询串的地址, `target`, 部位组合）缓存，多部位识别出的逐件蒙版也按单部位落缓存（之后单选某件立即返回）；同一张图同一区域只识别一次，之后立即返回 `cached: true`；签名参数不同不影响命中。子任务的 `mask_url` 与本接口共用缓存。
- **免费**：不扣点数（识别成本由平台承担）。按用户限频：每分钟 30 次、每天 500 次（网关 `Studio.MaskPerMinute / MaskPerDay` 可调，命中缓存也计数），超出 `4002`「识别太频繁，请稍后再试」。
- **耗时**：未命中缓存一般数秒（背景走高清抠图会更久），上限约 120 秒；前端请自行加 loading，失败退回整图校色。

| 情况 | 返回 |
|---|---|
| 缺地址 / 区域不支持 / 地址不是本服务的 | `1002` / `1003` |
| 没识别到对应区域（图里没有服装 / 人物 / 主体） | `4001`「没识别到对应区域」（24 小时内同一张图同一区域直接返回该结果） |
| 超出频率 | `4002`「识别太频繁，请稍后再试」 |
| 识别服务暂不可用 | `6001`「区域识别暂不可用，请稍后再试」（不缓存，可重试） |
| 网关 / 媒体服务未配置 | `7003` |

识别走 ai-gateway `image.segment`：服装 `imgseg.aliyun-cloth`（阿里云 SegmentCloth）、商品 `imgseg.aliyun-commodity`（SegmentCommodity）、
人物 `imgseg.aliyun-body`（SegmentBody）、背景 `imgseg.aliyun-hd`（SegmentHDCommonImage 取反）。

---

## 6. 模型、分辨率、比例

> **按模型取可选项**：`GET /api/v1/studio/models?kind=tryon|pose|imagegen[&with_price=true]` 返回每个模型档（`通用` / `GPT` / `GPT25` / `Banana` / `Seedream`；提交时旧档名 `GPT25Dev` 仍按 `GPT25` 受理，输出只用 `GPT25`）的展示名 `label`、实际可选的 `resolutions` / `aspect_ratios`、是否支持「自动」比例 `auto_aspect`、本 kind 的参考图上限 `max_ref_images`、限制说明 `notes`、gateway 目录里是否可用 `available`；`with_price=true` 时附每档分辨率的单张点数 `prices[]`（与 quote 同一计费入口；新增可选字段 `prices[].settle_mode`，`ACTUAL_COST` 时 `unit_points` 是单张最多扣多少，见第 7 章）。
> 可选项与提交时的参数归一同源（Seedream 5.5 无 `4:5` / `5:4`、旧 Seedream 无 4K、GPT Image 2.6 参考图上限按 kind 收紧），再与 gateway 目录 `feature_limits` 求交；目录拉不到时只按组规则。前端据此收窄选项、换模型时把不支持的值落到第一个可选项；下文的表是兜底口径。

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


### 6.4 商拍参考图上限与细节图预算

一次商拍生图送给模型的参考图 = 模特 1 张（图 1）+ 服装主图 + 细节裁图，**总数永远不超过该模型的参考图上限 `cap`，且任何模型都不超过 14 张**（GPT 标称 16，也按 14；不会发出 16 张）。`GET /studio/models` 的 `max_ref_images` 就是这个 `cap`，与实际组装同一口径（`pipeline.RefCap`）：

| 模型 | `cap`（`max_ref_images`） |
| --- | --- |
| 通用 / Banana / GPT / GPT25 等 | 14（目录标称更大也压到 14；目录给更小的以目录为准） |
| Seedream 5 Pro | 10 |
| GPT Image 2.6 | 6（计费硬上限，商拍提交时服装图 >5 张直接 `code=1003`） |
| 目录里更小的模型（如 Qwen 3） | 目录值 |

**组装规则**（每个子任务按自己的模型 `cap` 各算一次）：

1. 服装主图最多 `cap − 1` 张，按单品轮询保留（每件至少 1 张），超出的不参与生成；
2. 细节裁图名额 `crops = max(0, cap − 主图张数 − 1)`，没有主图入选的单品优先；
3. 顺序：模特 → 主图 → 细节。

**反推读图与生图参考图是两件事**：sidecar 一次反推最多读 9 张，且主图非 look/compose 品类总共最多 4 张、look/compose 最多 6 张，所以服务端另选一份 `SelectImages`（≤9 张，主图→细节→模特）给反推；每件 6 张服装图（总 9 张）都允许进入生图参考（只受 `cap` 约束），不受反推 4 张读图限制。sidecar 本身不改。

**`warnings`（提交响应，中文，不含模型 / 厂商名）**。提交时尚未切图，按服装图张数推算（预估每张主图切 4 个细节框）：

| 情形 | 文案 |
| --- | --- |
| 服装图超过 `cap − 1` | 所选模型最多支持 {cap} 张参考图，已保留 {n} 张服装图，其余 {m} 张未参与生成；可减少服装图，或换用支持更多参考图的模型 |
| 细节名额小于预估切图数，但 >0 | 所选模型参考图数量有限，细节图已减少为 {k} 张 |
| 细节名额为 0 | 所选模型的参考图数量已被服装图占满，细节图已省略 |

切图完成后若实际仍有裁剪，同样文案写入子任务 `prompt_meta.warnings`（后台可见），并透出到任务 `warnings`（用户可见，见 4.4）。例：Seedream（cap 10）传 6 张服装图 → `细节图已减少为 3 张`；通用（cap 14）传 6 张 → 不提示。

用户自选细节框（`detail_crops_manual=true`）时，「细节图将减少为约 k 张」这句换成按实际框数：`所选模型最多还能放 {k} 张细节图，已选的 {n} 张中将保留 {k} 张`（名额为 0：`…已选的 {n} 张细节图将不参与生成`）。

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
- **按实际用量结算（2026-11-10 起，GPT Image 2.6）**：载体模型（档位默认模型）`afv_ai_model.settle_mode=ACTUAL_COST` 的档位（目前 `studio-gpt-image-2.6-sunburst-tryon` / `-2ref`、`creaft-gpt-image-2.6-sunburst`）冻结照旧按配置价（= **最多扣多少**）；任务成功时按 ai-gateway 返回的供应商**实报成本**（调用 `cost.source=reported` 且 `final=true`，单位美元；网关内多次尝试已合计，或按各扣费尝试求和）确认：
  `实扣 = min(冻结, max(1, ceil(成本USD × 汇率 × 加价倍率 × 100)))`，差额在同一事务退回原批次。汇率 / 默认倍率读 `afv_system_config` 的 `billing_actual_cost_usd_cny_rate`（缺省 7.2）/ `billing_actual_cost_default_markup`（缺省 1.3），载体 `cost_markup` 非空时用它。拿不到实报成本时按冻结全额确认，服务端 ERROR 日志 `判据=ACTUAL_COST_UNAVAILABLE`（事后可用 `tools/ops/actual-cost-refund` 补退）。用户重试 / 重新生成的新一轮同样按实际结算。
  报价 / 模型价目里 `settle_mode=ACTUAL_COST` 时，前端把「消耗 N 点」换成「**按实际用量结算，最多 N 点**」（N = `max_points`，与 `points` 相同）；`estimated_points` 仍是冻结额。
- **按本次参数冻结（2026-11-10 起，Java V1.0.223）**：上面这些载体配了 `afv_ai_model.freeze_cap_json` 时，冻结不再按价目表最坏档，而是按本次的分辨率、质量与参考图张数，用 Atlas 公布的预扣上限算每张最多扣多少：
  `每张 = min(价目表单价, max(1, ceil((输出上限 + 参考图张数 × $0.056192 + 辅助 $0.005) × 汇率 × 加价倍率 × 100)))`（输出上限 1024² low $0.0687 / medium $0.1061 / high $0.2326，2K / 4K 按档位最大面积放大；有参考图时网关固定 medium）。
  质量与编排同口径（商拍 high、其余 medium，有参考图一律 medium）；参考图张数取上界：商拍 6（细节裁图编排时才知道）、姿态 2、图片生成 = 用户参考图张数（≤5）。例：图片生成 1k 文生图 104 点、1k 3 张参考图 262 点，姿态 1k 210 点，商拍 1k 仍 420 点。成功后照旧按实际用量结算（上限 = 冻结额）。
  `max_points` / `points` / `estimated_points` 都是这个按参数算出的数。图片生成的报价要知道参考图张数：请求体带 `image_urls`（只用张数，可直接发提交体；文生图传 `[]`），不带时按该模型参考图上限报（最多扣多少不会比提交冻得少）。
- **历史多扣补退**：按实际用量结算上线前（Java V1.0.215 起）已按冻结上限确认的画布 / 商拍单，用 `go run ./tools/ops/actual-cost-refund -dsn … -gateway … -secret … -since '<V1.0.215 上线时间>'`（默认 dry-run，加 `-apply` 才退）按同一公式补退差额；每张预留单只退一次（流水幂等键 `actual-cost-refund:{reservationNo}`），预留单置 `PARTIALLY_REFUNDED`。**先跑 ai-gateway 的历史调用成本回填脚本**（让 `GET /v1/calls/{id}` 的 `cost` 变成 `source=reported, final=true`），否则这些单会判为 `no_reported_cost` 跳过；回填后重跑即可。成功后又「重新生成」的旧轮次（请求号被新一轮覆盖）对不上调用号，工具只列为 `UNMATCHED` 供人工处理。

### 7.1 提交前报价（只读）

| 功能 | 报价接口 | 计价张数 `task_total` |
|---|---|---|
| 商拍换装 | `POST /tryon/quote` | `model_image_urls` 张数 × `count_per_model` |
| 姿态复刻 | `POST /pose/quote` | `pose_image_urls` 张数 |
| 图片生成 | `POST /image-generate/quote` | `count`（另读 `image_urls` 张数，见上文「按本次参数冻结」） |

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
| `settle_mode` | string | 新增（可选）：结算方式，`FIXED` 按报价扣 / `ACTUAL_COST` 成功后按供应商实际用量结算（`points` 是最多扣多少）；报不出价 / 计费关闭时不返回 |
| `max_points` | int | 新增（可选）：本次最多扣多少点（= `points`，即冻结额）；`ACTUAL_COST` 时显示「按实际用量结算，最多 N 点」 |

```json
// POST /api/v1/studio/image-generate/quote   {"model":"通用","resolution":"2k","aspect_ratio":"1:1","count":3}
{"code": 0, "msg": "ok", "data": {
  "available": true, "points": 45, "unit_points": 15, "price_unit": "PER_IMAGE", "task_total": 3,
  "resolution": "2k", "billing_mode": "on", "charged": true, "reason": "", "warnings": [],
  "settle_mode": "FIXED", "max_points": 45
}}
// GPT Image 2.6（按实际用量结算）：显示「按实际用量结算，最多 1672 点」
{"code": 0, "msg": "ok", "data": {"available": true, "points": 1672, "unit_points": 836, "price_unit": "PER_IMAGE", "task_total": 2,
  "resolution": "1k", "billing_mode": "on", "charged": true, "reason": "", "warnings": [], "settle_mode": "ACTUAL_COST", "max_points": 1672}}
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
| GET | `/batches/:id` | 批次详情：`batch` + 切图框 `crop_plan`、切图质检 `crop_check`、组图 `selected_images`（商拍 = 反推读图选中的图，≤9 张/模特，**不是**生图输入；实际送生图的图序看各任务 `input_images`）、每步耗时与成本 `step_log`（成本字段 `cost_cny`，均为 JSON 文本）+ 全部任务（含完整提示词 `prompt_text`、`prompt_meta`、`input_images`、`gateway_call_id`、`gateway_status`、`attempt`、`admin_locked`、`cost_cny`（人民币元）、`billing_request_id`、`billing_status`、`duration_ms`） |
| POST | `/tasks/:task_id/retry` | 重做子任务：只收失败 / 已取消、且没被强制失败封禁的；换新计费请求号（`studio-<task_id>-r<attempt>`）重新冻结、`attempt+1`、清空错误与 gateway 字段后回到排队，由轮询器用新幂等键重新提交。批次已终态时拉回生图中（还没反推过的拉回排队重新编排）；拉回出错时撤销本次重做（任务回原状态、新冻结退回）并报错，轮询器另有兜底。冻结失败（如点数不足）且计费服务里查无这张新单时退还本轮（`attempt` 回退，下次仍用同一个号），查不清时本轮作废、下次换下一个号。成功 / 进行中 / 批次提交中的返回 4002 |
| POST | `/tasks/:task_id/force-fail` | 强制失败并封禁重做（`admin_locked=1`）：排队 / 生成中的改为失败并退回点数（预留单若已扣费则走已确认退款），有上游调用的先尽力取消（取消时已出图则返回 4002、照常交付）；交付中（5）的返回 4002「任务正在交付，不能强制失败」；已失败 / 已取消的只封禁、不动账；已成功的返回 4002 |
| GET | `/sidecar/status` | 依赖状态：反推 sidecar / ai-gateway / 媒体服务的 `ok`、`latency_ms`、`version`、`detail`，外加 `running_tasks`、`queued_batches` |
| GET | `/cost-summary` | 成本汇总。查询参数：`start_at`、`end_at`、`group_by`（`kind` 或 `model_group`，默认 `kind`）。不给时间默认最近 30 天（按任务创建时间，起含止不含）。返回 `rows` 与 `total`，每行含 `task_total`、`task_succeeded`、`task_failed`（含取消）、`cost_cny`（上游成本，人民币元，取 gateway `call.cost.amount_cny`）、`points`（汇总查询不含预留单，目前恒为 0） |

任务状态 `status` 与用户侧同口径（0 排队 / 1 生成中 / 2 成功 / 3 失败 / 4 已取消 / 5 交付中）；批次状态另有 8 提交中；`billing_status`：0 未计费 / 1 已冻结 / 2 已结算 / 3 已退回 / 4 影子模式（不动账）。
