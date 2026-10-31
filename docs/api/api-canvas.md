# 画布 / 提示词库 · 前端接口文档

> **状态：已按实现核对（2026-10-31）。** 接口定义见 `app/gateway/api/gateway.api` 的 canvas / promptlib 组（后台见 `app/admin/api/admin.api`），服务实现 `app/canvas/rpc`，表 `deploy/sql/Go_V1.0.25__canvas.sql`。本文与接口定义不一致时以接口定义为准。
> 画布属于 v1.1 范围，不在 v1.0 验收内。

---

## 0. 概述

### 0.1 画布怎么存、怎么跑

- **整段快照存取。** 画布文档就是前端 Vue Flow 的原生结构 `{nodes, edges}`，服务端整段存、整段取，只做校验，不拆节点表。没有 `inputRefs`、没有 `viewport`、没有 `@[nodeId]` 占位符。
- **乐观锁。** 每张画布有字符串 `revision`，保存时带 `if_match_revision`；不一致返回冲突（4002），带 `force=true` 可强制覆盖。
- **单节点执行。** 后端只收「执行某个节点」，不做整图编排。前端仍按拓扑序逐节点调用（和原型 `runAll` 一致）；关闭页面不会续跑。整图服务端编排另立项。
- **候选结果。** 图片 / 视频生成一次可出多个候选（`candidates[]`），用户选一个（`selected_index`），也可以把某个候选单独连给下游。
- **运行态由服务端回填。** `status / progress / candidates / image / video / run_id / asset_id / error_message` 这些运行态字段**不随快照存储**：保存时服务端会把它们从快照里剥掉，`GET /canvases/{id}` 时再按最近一次 node run 回填进每个节点的 `data`。所以前端保存时不必清理它们，也不要依赖保存后的回包里还带着它们。
- **节点内禁止 dataURL。** 快照里任何图片 / 视频字段（`image`、`video`、`maskDataUrl` 等）都不能是 `data:` 地址，必须先上传换 URL（见 1.6）。快照整体 ≤ **2MB**。

### 0.2 六个工具节点：全部是真能力

原型里六个工具节点是浏览器本地伪实现（canvas 裁剪、放大拉伸、扩图返原图、反推固定文案、重绘返原图、试穿回填模特图），**上线后全部替换为后端真实能力**，前端不要再本地伪造结果：

| 工具（`data.tool`） | 节点 | 后端 | 计费 |
|---|---|---|---|
| `crop` | 图片裁剪 | media 服务 `image/crop` | **不计点** |
| `upscale` | 高清放大 | Java 兼容工具 `ai-image-upscaler` | **Java 计费**（用户 Bearer 透传，任务中心 / 资产由 Java 记） |
| `outpaint` | 扩图 | Java 兼容工具 `ai-ratio-convert` | **Java 计费** |
| `mask` | 局部重绘 | Java 兼容工具 `ai-inpaint` | **Java 计费** |
| `i2p` | 图片反推提示词 | gateway `chat.completion` 多模态 | 按 node run 计费 |
| `tryon` | 反推模特试穿 | studio 商拍链路（见 `api-studio.md`） | studio 计费 |

图片生成、视频生成（`type=model` 节点）：走 gateway 生图 / 生视频链路，**按 node run 计费**：每次 run 一张预留单，`count>1` 时部分失败只退失败张。

对前端的含义：工具节点和模型节点调用的是**同一个** run 接口，差别只在 `kind` / `tool` 与 `params`；计费不同，前端不用区分，以 `credits` 字段展示本次 run 的实际消耗（裁剪为 0；走 Java 的工具由 Java 扣点，回包 `credits` 仍如实回填）。

Java 工具（放大 / 扩图 / 局部重绘）的取消：通过 Java 现有取消接口真实取消并由 Java 退款；无有效登录态时退化为停止跟踪（详见 3.4 与 7.4）。

### 0.3 路径一览

前缀 `/api/v1/canvas` 与 `/api/v1/prompt-library`，由 nginx 转到 Go gateway-api（不与 Java 的一级前缀冲突）。下文路径都省略 `/api/v1` 之前的域名。

| 接口 | 说明 |
|---|---|
| `GET /canvas/canvases` | 画布列表 |
| `POST /canvas/canvases` | 新建画布 |
| `GET /canvas/canvases/{id}` | 取画布（含快照、运行态回填） |
| `PUT /canvas/canvases/{id}` | 保存（乐观锁） |
| `DELETE /canvas/canvases/{id}` | 删除（逻辑删除） |
| `POST /canvas/canvases/{id}/save-as` | 另存为 |
| `POST /canvas/canvases/{id}/nodes/{node_id}/run` | 执行单个节点 |
| `POST /canvas/canvases/{id}/nodes/{node_id}/select-candidate` | 选中候选（写回服务端，刷新后保持） |
| `GET /canvas/runs/{id}` | 查询一次执行 |
| `POST /canvas/runs/{id}/cancel` | 取消执行 |
| `GET /canvas/catalog` | 模型 / 参数目录（替代前端写死） |
| `GET /canvas/templates` | 画布模板列表（不含快照） |
| `GET /canvas/templates/{id}` | 模板详情（含快照） |
| `POST /canvas/canvases/from-template` | 从模板建画布 |
| `GET /prompt-library/prompts` | 提示词列表 |
| `POST /prompt-library/prompts` | 新增个人提示词 |
| `PUT /prompt-library/prompts/{id}` | 修改个人提示词 |
| `DELETE /prompt-library/prompts/{id}` | 删除个人提示词 |

---

## 1. 通用约定

与 `api-studio.md` 第 1 章一致，这里只列要点。

### 1.1 鉴权

所有接口带 `Authorization: Bearer <token>`。未登录 / 过期：**HTTP 401**，`code=2001`，这是唯一不返回 HTTP 200 的情况。联调环境（`Auth.Mode=dev`）没带 token 时认 `X-User-Id`。

### 1.2 响应信封

```json
{"code": 0, "msg": "ok", "data": { }}
```

失败多一个 `reason`，前端按 `reason` 分支，不要按数字；`msg` 是可直接展示的中文：

```json
{"code": 1003, "reason": "PARAM_INVALID", "msg": "参考图最多接 5 条", "data": null}
```

**HTTP 状态码一律 200，用 `code` 判断。** 注意这和原型不同：原型靠 HTTP 409 判冲突、靠 `ETag` 取 revision，现网一律 200 + 信封，冲突看 `code=4002`，revision 在 `data.revision`。

### 1.3 幂等：`Idempotency-Key`

只有「执行节点」必须带，值等于请求体的 `run_id`（原型已经这么做）。同一用户、同一个 `Idempotency-Key`、同一份 body：返回原 run，不重复生成、不重复扣点，`replayed=true`；同 key 不同 body：`code=4002`。网络失败重试**沿用同一个 `run_id`**。其它接口不需要。

### 1.4 字段与时间

- 请求体、查询参数、响应一律**下划线**命名（原型是 camelCase，对照表见第 9 章）。
- 时间是 **unix 毫秒**（原型是 ISO 字符串）。
- `revision` 是字符串，形如 `1761800000123-a1b2`（保存时的毫秒时间戳 + 4 位随机十六进制，每次保存换新），前端当黑盒，只原样回传，**不要比较大小**。
- 画布 `id` 是字符串（原型 `workflowId`）。**快照内部**的字段（节点 `data` 里的 `ratio`、`maxImageRefs`、`sourceNodeId` 等）保持**前端原样的 camelCase，不做下划线化**——快照是前端私有文档，服务端只校验、不翻译。下划线化只针对接口的外层字段。

### 1.5 错误码

| code | reason | 含义 | 前端怎么处理 |
|---|---|---|---|
| 0 | — | 成功 | — |
| 1002 | PARAM_MISSING | 缺必填参数（含缺 `Idempotency-Key`） | 展示 `msg` |
| 1003 | PARAM_INVALID | 参数不合法：快照校验失败（连线规则、dataURL、超 2MB）、取值不支持、提示词超长 | 展示 `msg`；快照类错误见 6.2 |
| 2001 | UNAUTHORIZED | 未登录（**HTTP 401**） | 跳登录 |
| 2007 | ACCOUNT_DISABLED | 账号被禁用 | 退出登录 |
| 3003 | INSUFFICIENT_POINTS | 点数不足（本次 run 没有提交） | 提示充值 |
| 4001 | RESOURCE_NOT_FOUND | 画布 / run / 模板 / 提示词不存在或不属于当前用户 | 返回列表 |
| 4002 | RESOURCE_CONFLICT | 保存版本冲突；同 key 不同 body；取消已结束的 run；对官方提示词做修改 / 删除 | 版本冲突见 2.4；其余展示 `msg` |
| 7003 | FEATURE_NOT_READY | 功能暂未开放 | 展示 `msg` |
| 5001 | SYSTEM_ERROR | 服务繁忙 | 稍后重试，**沿用同一个 `run_id`** |

### 1.6 上传

节点里要放图片 / 视频，一律先上传拿 URL，上传方式沿用 `api-app.md`「上传：前端直传 OSS」（原型的 `POST /api/uploads` multipart 不再使用）。限制以实际的上传接口为准：原型写的图 30MB / 视频 200MB 与 PRD 的 10MB / 500MB 不一致，**上线以 PRD 口径（图 10MB、视频 500MB）为准，待实现核对**。

- 快照里的图片字段必须是**我方 OSS / 素材库地址**；`data:`、`blob:`、`file:`、非 http(s) 一律拒绝（`code=1003`）。
- 局部重绘的蒙版（原型存 `maskDataUrl`）：先把蒙版 PNG 上传，换成 URL 再保存 / 再提交 run，不能留 dataURL。

### 1.7 参数限制

| 项 | 限制 |
|---|---|
| 快照大小 | ≤ 2MB（序列化后的 JSON 字节数） |
| 画布标题 | ≤ 100 字 |
| 单次 run 提示词 `prompt` | ≤ 3000 字（按字符数） |
| 单次 run 候选数 `count` | 1–4（以 `catalog` 的 `count_max` 为准） |
| 提示词库：标题 | ≤ 100 字 |
| 提示词库：正文 | ≤ 3000 字 |
| 提示词库：`models` / `tags` | 各 ≤ 10 项，单项 ≤ 32 字 |
| 列表 `page_size` | 默认 20，上限 50 |

---

## 2. 画布文档

### 2.1 画布列表

```
GET /api/v1/canvas/canvases?page=1&page_size=20
```

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "items": [
      {"id": "cv_8f2a91", "title": "春季上新", "updated_at": 1761800000000, "node_count": 7, "cover_url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/…/cover.jpg", "revision": "1761800000123-a1b2"}
    ],
    "total": 1
  }
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | 画布 id（原型 `workflowId`） |
| `title` | string | 标题；空标题显示「未命名画布」由前端处理 |
| `updated_at` | int | 毫秒 |
| `node_count` | int | 节点数（含 module 框） |
| `cover_url` | string | 封面，取快照里第一张可用图；没有为空串 |
| `revision` | string | 当前版本号 |

按 `updated_at` 倒序。

### 2.2 新建画布

```
POST /api/v1/canvas/canvases
```

```json
{"title": "未命名画布", "workspace_id": 0}
```

`title` 可空（默认「未命名画布」）；`workspace_id` 不传 = 个人空间。返回：

```json
{"code": 0, "msg": "ok", "data": {"id": "cv_8f2a91", "title": "未命名画布", "revision": "1761799000001-0c3e", "updated_at": 1761800000000}}
```

新画布快照是空的 `{"nodes": [], "edges": []}`。返回体与 2.3 相同（另含 `node_count`、`cover_url`、`created_at` 与空 `snapshot`）。

### 2.3 取画布

```
GET /api/v1/canvas/canvases/{id}
```

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "id": "cv_8f2a91",
    "title": "春季上新",
    "revision": "1761800000123-a1b2",
    "updated_at": 1761800000000,
    "snapshot": {
      "nodes": [
        {"id": "t_1761799000_1", "type": "text", "position": {"x": 160, "y": 140},
         "data": {"text": "白底棚拍", "title": "文字 1", "subtitle": "文字", "kind": "text", "mentions": []}},
        {"id": "m_1761799100_2", "type": "model", "position": {"x": 520, "y": 180},
         "data": {"title": "图片生成 1", "subtitle": "Banana · 1K", "kind": "model", "model": "Banana", "maxImageRefs": 5,
                  "ratio": "1:1", "size": "1K", "quality": "中", "cfg": 0.95, "count": 2, "prompt": "",
                  "status": "done", "runId": "run_20261030_ab12", "progress": 100,
                  "candidates": ["https://bucket.oss-cn-hangzhou.aliyuncs.com/…/c1.jpg", "https://bucket.oss-cn-hangzhou.aliyuncs.com/…/c2.jpg"],
                  "selectedCandidate": 0, "image": "https://bucket.oss-cn-hangzhou.aliyuncs.com/…/c1.jpg"}}
      ],
      "edges": [
        {"id": "e_1761799200_3", "type": "canvas-bezier", "source": "t_1761799000_1", "target": "m_1761799100_2",
         "sourceHandle": "text", "targetHandle": "prompt", "data": {"color": "#22c55e"}}
      ]
    }
  }
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` / `title` / `revision` / `updated_at` | — | 同列表 |
| `snapshot.nodes` | array | 节点，结构见第 5 章 |
| `snapshot.edges` | array | 连线，结构见第 5 章 |

要点：

1. 节点 `data` 里的运行态由服务端回填（0.1），分两部分：
   - **状态**（`status / progress / runId / requestId / errorMessage / requestedCount`）取该节点**最近一次 run**；
   - **成品**（`candidates / selectedCandidate / image / video / resultText / resultImage / tryonResults`）取该节点**最近一次成功的 run**。所以最近一次失败 / 取消 / 还在跑时，节点 `status` 是 `error` / `running`，但仍显示上一次成功的候选。
   - `selectedCandidate` 以服务端记录为准（`POST …/select-candidate` 写回，见 3.6），刷新后保持。
   回填后的 `candidates` 在快照内仍是 URL 字符串数组，和原型一致，前端无需改渲染。
2. 画布不存在或不是自己的：`code=4001`。
3. 取到 `revision` 后，下一次保存带上 `if_match_revision`。

### 2.4 保存（乐观锁）

```
PUT /api/v1/canvas/canvases/{id}
```

```json
{
  "title": "春季上新",
  "snapshot": {"nodes": [ … ], "edges": [ … ]},
  "if_match_revision": "1761800000123-a1b2",
  "force": false
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `title` | string | 否 | 不传 = 不改标题 |
| `snapshot` | object | 是 | 整段 `{nodes, edges}`，每次整段覆盖；网关按原文透传（不重编码，大整数不丢精度），超过 **2MB** 直接 `1003` |
| `if_match_revision` | string | 条件必填 | 当前持有的版本号；与服务端一致才保存。**不传且 `force=false` → `1002`**（不再静默覆盖；新建画布返回的 `revision` 也要带上） |
| `force` | bool | 否 | `true` = 忽略版本，强制覆盖（此时可不带 `if_match_revision`） |

`dataURL` 检测是整段扫描：节点 `data`（只有 `data` 第一层的文本键 `text / prompt / title / subtitle / maskPrompt / resultText / name / label` 豁免，嵌套层级一律检查）、节点外层其它键、连线、快照顶层其它键（如 `viewport`）里出现 `data: / blob: / file:` 开头的字符串都回 `1003`。

成功：

```json
{"code": 0, "msg": "ok", "data": {"revision": "1761800100456-9f20", "updated_at": 1761800100000, "node_count": 7, "cover_url": ""}}
```

保存成功后，前端**用返回的 `revision` 替换本地的**。原型的自动保存是 400ms 防抖，沿用即可；同一画布一次只发一个保存请求，上一个没回来先别发下一个，否则必然自己和自己冲突。

**版本冲突**（别的窗口已改过）：

```json
{"code": 4002, "reason": "RESOURCE_CONFLICT", "msg": "画布已被其他窗口修改", "data": {"conflict": "revision", "revision": "1761800090789-77d1", "updated_at": 1761800090000}}
```

`data.revision` 是服务端当前版本。前端按 `data.conflict == "revision"` 区分「版本冲突」与其它 4002，弹原型已有的两个选项：「载入最新版本」（重新 `GET`）/「覆盖保存」（同一份快照带 `force=true` 再发一次）。

校验失败（`code=1003`）：整次保存不生效，旧版本保持不变，`msg` 说明原因，见 6.2。

### 2.5 删除

```
DELETE /api/v1/canvas/canvases/{id}
```

成功 `{"code":0,"msg":"ok","data":{}}`。逻辑删除，画布从列表消失；已入素材库的成品不受影响。正在执行的 gateway / studio run 不会被取消，结果仍会入素材库；正在执行的 Java 工具 run 会由后台调 Java 取消接口取消（由 Java 退款；取消时已成功则照常交付；无有效登录态时退化为停止跟踪）。不存在 / 不是自己的：`code=4001`。

### 2.6 另存为

```
POST /api/v1/canvas/canvases/{id}/save-as
```

```json
{"title": "春季上新 - 副本"}
```

服务端复制当前**已保存**的快照成一张新画布（运行态已剥离，所以副本的节点都是 `idle`、没有候选，和原型 `snapshotForCopy` 的行为一致），`revision` 重新生成。返回同 2.3（含 `snapshot`）：

```json
{"code": 0, "msg": "ok", "data": {"id": "cv_c03d77", "title": "春季上新 - 副本", "revision": "1761799000001-0c3e", "updated_at": 1761800200000}}
```

注意：另存为复制的是**服务端已保存的版本**，不是前端当前内存里的。点「另存为」前请先等当前的自动保存落地（原型是前端拼快照再建 + 存两次，现在合成一个接口，且失败不会留下半张空画布）。

---

## 3. 执行节点

### 3.1 提交

```
POST /api/v1/canvas/canvases/{id}/nodes/{node_id}/run
Idempotency-Key: <run_id>
```

一个请求 = 一个节点跑一次。请求体字段来自原型的 `RunNodeRequest`（`workflowId` 与 `nodeId` 已在路径里，不用再放进 body）：

**图片 / 视频生成（`type=model` 节点）**

```json
{
  "run_id": "run_20261030_ab12",
  "kind": "image",
  "model": "Banana",
  "model_id": "banana-v1",
  "prompt": "白底棚拍，保持材质与颜色一致",
  "references": [
    {"handle": "image", "index": 1, "node_id": "n_1761799000_1",
     "image": "https://bucket.oss-cn-hangzhou.aliyuncs.com/…/cloth.jpg", "asset_id": "88123"},
    {"handle": "image", "index": 2, "node_id": "m_1761799100_9",
     "image": "https://bucket.oss-cn-hangzhou.aliyuncs.com/…/c2.jpg", "source_handle": "candidate-1", "candidate_index": 1}
  ],
  "ratio": "1:1",
  "size": "2K",
  "quality": "medium",
  "count": 2,
  "cfg": 0.95
}
```

视频生成多 `duration`（`"4s"`）、`sound`（bool），`kind="video"`，`references` 里可出现 `handle="video"`。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `run_id` | string | 是 | 前端生成的 UUID（≤64 字符），同时放在 `Idempotency-Key` 头；一次点击一个 |
| `kind` | string | 是 | `image` / `video` / `tool` |
| `model` | string | 生成必填 | 节点上的模型显示名（Banana / Image / SD5P / Seedance 2.5 …） |
| `model_id` | string | 生成必填 | 模型 id（`image-v1` / `banana-v1` / `sd5p-v1` / `seedance-2-5` …），以 `catalog` 为准 |
| `prompt` | string | 否 | 节点自己的提示词；通常提示词走连线（`references` 里的 `prompt`）。两者都有且不同时，服务端拼成「连线文本 + 换行 + 节点提示词」；合计 ≤3000 字 |
| `references` | array | 否 | 上游引用，见下表。服务端会拿它与快照里已保存的连线核对，所以**先保存、再执行** |
| `ratio` | string | 生成必填 | 比例，枚举见 `catalog` |
| `size` | string | 生成必填 | 图 `1K` / `2K`；视频 `720p` / `1080p` / `4K` |
| `quality` | string | 否 | `low` / `medium` / `high`（原型存中文 低/中/高，**提交时转成英文**，这一步原型已有） |
| `duration` | string | 视频 | `4s` / `6s` / `8s` |
| `sound` | bool | 视频 | 是否带声音 |
| `count` | int | 是 | 候选张数，1–4；视频固定 1 |
| `cfg` | number | 否 | 默认 0.95 |
| `tool` / `params` | — | 工具必填 | 见 3.2；`kind` 为 `tool` 时用 |

`references[]` 每项：

| 字段 | 类型 | 说明 |
|---|---|---|
| `handle` | string | `prompt` / `image` / `video` / `audio`；**视频首尾帧必须保留原口名 `image1`（或 `first`）/ `image2`（或 `last`）**，否则会被当成普通参考图；试穿可用 `garment` / `models` 区分服装与模特（或改用 `params`，见 3.2）；其余别名（`image-in` 等）可归并成 `image` |
| `index` | int | 同一 `handle` 内的序号，从 1 起，即连线的 `data.order` |
| `node_id` | string | 来源节点 id |
| `text` | string | handle=prompt 时的文本 |
| `image` / `video` / `audio` | string | 引用素材的 URL，**不能是 dataURL** |
| `asset_id` | string | 来自素材库时带上 |
| `media_id` | string | 上传返回的 id（可选） |
| `source_handle` | string | 来源节点输出口，如 `image-out`、`candidate-1` |
| `candidate_index` | int | 引用的是上游模型节点的第几个候选（从 0 起） |

提交成功（入队即返回，不等出图）：

```json
{
  "code": 0, "msg": "ok",
  "data": {"request_id": "r_5521", "run_id": "run_20261030_ab12", "status": "queued", "replayed": false, "credits": 40, "warnings": []}
}
```

| 字段 | 说明 |
|---|---|
| `request_id` | 服务端 run 号，轮询 / 取消用它（原型 `requestId`） |
| `run_id` | 回显 |
| `status` | `queued` |
| `replayed` | 幂等重放命中原 run 时为 `true` |
| `credits` | 本次 run 预留（冻结）的点数；不是最终扣费，以终态 `credits` 为准 |
| `warnings` | string[]，参数归一告警，没有时为 `[]`。目前只有 Seedream：`size` 传 `4K` / `1K` 会按 **2K** 受理并回一条告警（如「Seedream 不支持 4K（会被降为 2K），已按 2K 生成并计费」）；幂等重放同样回告警。前端可 toast 提示，不需要重提 |

提交时的失败（都是整次 run 没有创建）：

| code | 场景 |
|---|---|
| 1003 | 引用不满足连线规则（超过参考图上限、缺必填输入）、提示词超长、参数不在 `catalog` 内、引用了 dataURL |
| 3003 | 点数不足 |
| 4001 | 画布 / 节点不存在 |
| 4002 | 同 `run_id` 不同 body；该节点上一次 run 还在进行中（同一节点同一时刻只允许一个 run） |

### 3.2 工具节点

工具节点走同一个接口，`kind="tool"`，用 `tool` 指明哪一个，`params` 放该工具的参数。输入图通过 `references`（`handle=image`）给，**不要把图片放进 params**。工具的输入图有两种来源，执行时都会核对：一是从上游连一条线到工具的 `image` 口（每个工具 1 条）；二是在工具节点的 `data.sourceNodeId` 里指向上游节点，不需要连线。

| `tool` | `params` | 结果 |
|---|---|---|
| `crop` | `{"shape": "rect", "x": 0.1, "y": 0.1, "w": 0.6, "h": 0.6}`，坐标相对原图 0–1（即 `data.crop`，也可整段放在 `params.crop` 里）；`shape` 为 `rect` / `circle`（取外接矩形）/ `brush`，brush 时另带 `brush`（或 `cropBrush`）`[{x, y, r}]`，取笔迹外接矩形 | `candidates` 一张裁剪图（已转存素材库桶），同步完成，不计点 |
| `upscale` | `{"scale": 2}`（2 → Java `resolution=2k`，4 → `4k`）；也可直接给 `{"resolution": "2k"}`；都不给按 2k | `candidates` 一张放大图 |
| `outpaint` | `{"ratio": "16:9", "resolution": "2k"}`（目标比例，缺省取请求的 `ratio`；分辨率缺省 2k），或 `{"width": 1920, "height": 1080}`（须成对） | `candidates` 一张扩图 |
| `mask` | 修改要求放 `mask_prompt`（缺省取 `prompt`，必填其一）；区域四选一：`bbox`（恰好 4 个 `{x,y}` 比例点）/ `mask_strokes`（`[{x,y,r}]`）/ `mask_url`（已上传的蒙版 PNG，由媒体服务代为拉取，画布服务不直连该地址）/ `mask_data_url`（`data:image/png;base64,…`，只在 run 的 params 里允许；服务端先校验尺寸再拉入媒体服务，run 记录里只存 `mask_rid`，不存 dataURL）。蒙版 ≤20MB、宽高各 ≤4096（总像素 ≤1600 万），超出 `1003`。蒙版按「透明底取不透明像素 / 黑底取亮像素 / 原图上的蓝色标注取偏蓝像素」算外接矩形，作为 Java `ai-inpaint` 的 `bbox` | `candidates` 一张重绘图 |
| `i2p` | `{"language": "zh"}`（`en` 时输出英文），可省 | 文本结果在 `text`，`candidates` 为空 |
| `tryon` | `{"garment_image": "…", "models": [{"id": "m1", "image": "…"}]}`，可选 `items`（默认 `["top"]`）、`garment_name`、`fabric`、`model`（studio 模型档，默认「通用」）、`resolution`（默认 1k）、`aspect_ratio`（默认 3:4）；也可不给 params，用 `references` 的 `garment` / `models` 口 | `candidates` 每位模特一张；走 studio 链路（内部产生 studio 批次，`count_per_model=1`） |

输入图：除 `tryon` 外，工具的输入图都取 `references` 里第一条 `handle=image` 的引用（或 `params.image_url`）。工具一次只出 1 张（Java 工具 `num=1`）。

回包与轮询形状同 3.3；`credits` 为 0 的情况：`crop`，以及走 Java 兼容工具时的回填值取决于 Java 返回。

### 3.3 查询一次执行（轮询这个）

```
GET /api/v1/canvas/runs/{request_id}
```

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "request_id": "r_5521",
    "run_id": "run_20261030_ab12",
    "canvas_id": "cv_8f2a91",
    "node_id": "m_1761799100_2",
    "kind": "image",
    "status": "succeeded",
    "progress": 100,
    "model": "Banana",
    "candidates": [
      {"url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/…/c1.jpg", "width": 1024, "height": 1024},
      {"url": "https://bucket.oss-cn-hangzhou.aliyuncs.com/…/c2.jpg", "width": 1024, "height": 1024}
    ],
    "selected_index": 0,
    "video_url": "",
    "text": "",
    "credits": 40,
    "requested_count": 2,
    "error": "",
    "error_code": "",
    "retryable": false,
    "created_at": 1761800300000,
    "finished_at": 1761800345000
  }
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `request_id` / `run_id` | string | 同提交 |
| `canvas_id` / `node_id` | string | 所属画布与节点 |
| `status` | string | `submitting` / `queued` / `running` / `succeeded` / `failed` / `cancelled`，见第 7 章 |
| `progress` | int | 0–100 估算值，只画进度条 |
| `model` | string | 模型显示名 |
| `candidates` | array | 候选；**注意是对象数组**，原型 `RunNodeResult.candidates` 是 `string[]`（见第 9 章）。每项 `{url, width, height}`，URL 已转存到我方公有桶，长期可用 |
| `selected_index` | int | 当前选中的候选下标，默认 0 |
| `video_url` | string | 视频成品地址；图片 run 为空串。视频 run 的 `candidates[0]` 是封面图 |
| `text` | string | `i2p` 工具的反推文本，其它为空串 |
| `credits` | int | 本次实际消耗点数（终态才是最终值；失败 / 取消为 0） |
| `requested_count` | int | 本次请求的张数，用来显示「成功 n/m」 |
| `error` | string | 失败原因，可直接展示 |
| `error_code` | string | 机器可读的失败码（如 `CONTENT_BLOCKED`、`TIMEOUT`） |
| `retryable` | bool | 为 `true` 时前端可以提示「重试」；重试要**换一个新的 `run_id`** |
| `created_at` / `finished_at` | int | 毫秒；`finished_at` 为 0 = 未结束 |

run 不存在或不属于当前用户：`code=4001`。

### 3.4 取消

```
POST /api/v1/canvas/runs/{request_id}/cancel
```

无请求体。成功：

```json
{"code": 0, "msg": "ok", "data": {"request_id": "r_5521", "status": "cancelled"}}
```

语义按后端区分：

- **图片 / 视频 / 反推（gateway）**：退回冻结的点数，并取消上游调用（尽力而为，已经出的图不会入画布）；
- **试穿（studio）**：取消 studio 批次，点数由 studio 按其口径退回；
- **放大 / 扩图 / 局部重绘（Java 兼容工具）**：用本次请求的用户 Bearer 调 Java 现有取消接口 `POST /api/v1/creation/{taskId}/cancel`（`taskId` 即提交回执的任务号）**真实取消**，Java 停上游并**由 Java 退款**，Go 侧不退点数。按 Java 的回包：
  - 已受理（`code=0`）→ `cancelled`，`error` 为「已取消执行；Java 任务已取消，点数由 Java 退回」；
  - Java 回「任务已完成或失败」（`3001`）→ 立即查一次 Java 任务：**已成功则按成功交付**（回包 `status=succeeded`，有 `candidates`，钱已由 Java 扣，与 gateway 后端「取消时已成功则交付」一致）；Java 已失败 / 已取消 → `cancelled` 并注明；
  - 任务在 Java 不存在（`4001`）→ `cancelled`（停止跟踪）；
  - 登录态无效（401 / `2001`）→ 退化为**停止跟踪**：`cancelled`，`error` 写明「Java 任务未能取消（无有效登录态），可能仍会执行并扣费」；
  - Java 暂时不可达（网络 / 5xx）→ 回包 `status=running`，服务端后台重试取消（最多 5 次，用该用户最近一次请求的 Bearer），超限按停止跟踪处理；前端照常轮询到终态。
  - **提交时遇暂时性错误、还没拿到 Java 任务号的 run（`status=queued`）**：Java 可能已经建了任务。画布在首次发送前把实际发出的请求（路径 + 原始请求体 + `Idempotency-Key`）存在 run 上，取消时用**同一个幂等键逐字节重放**拿回任务号，再按上面的口径取消。Java 按 `(用户, Idempotency-Key)` 幂等：同键同内容返回原任务号、不新建不扣费；同键内容不同回 `1003`，此时无法找回，退化为停止跟踪（`error` 写明「无法找回 Java 任务号…可能仍会执行并扣费」）。**副作用**：若 Java 其实没收到首次请求，这次重放会真的新建一个任务（并冻结点数），随后立即取消、由 Java 退款；用户可能在 Java 任务中心看到一条「已取消」的记录。运营强制失败、排队超时、画布已删同样走这条路径。连请求都没构造成就失败的 run 没有可重放的请求，直接取消。

已是终态（`succeeded` / `failed` / `cancelled`）：`code=4002`。取消成功后继续轮询一次，以返回的 `status` 为准（Java 工具可能是 `succeeded` / `running`，见上）。

### 3.4.1 画布的执行记录

```
GET /api/v1/canvas/canvases/{id}/runs?node_id=m_1&limit=50
```

返回 `{"items": [RunNodeResult…]}`，新的在前；`node_id` 可省（全部节点），`limit` 默认 50、上限 200。

### 3.4.2 失败码 `error_code`

| error_code | 含义 | retryable |
|---|---|---|
| `CONTENT_BLOCKED` | 内容未通过审核 | 否 |
| `INPUT_REJECTED` | 输入不被模型接受 | 否 |
| `PARAM_INVALID` | 参数在提交后端时被拒 | 否 |
| `TIMEOUT` | 超时（图 5 分钟 / 视频 15 分钟 / 工具 10 分钟 / 反推 3 分钟） | 是 |
| `UPSTREAM_BUSY` | 模型繁忙 | 是 |
| `UPSTREAM_FAILED` / `EMPTY_RESULT` / `SUBMIT_FAILED` | 生成失败 / 无产物 / 提交失败 | 是 |
| `UPSTREAM_UNKNOWN` | 上游结果未知：点数**不退**、待对账 | 是 |
| `BILLING_REJECTED` | 冻结点数失败（与提交时的 3003 等同时返回） | 是 |
| `SUBMIT_TIMEOUT` | 提交中（冻结点数阶段）超过 5 分钟未完成，服务端兜底置失败并退回可能已冻结的点数 | 是 |
| `CANCELED` | 已取消 | 否 |
| `ADMIN_FORCE_FAIL` | 运营强制失败 | 是 |

### 3.5 终态与回填

run 到终态后，服务端把结果写回**画布快照里的该节点**（下一次 `GET /canvases/{id}` 就能看到；`status` 在节点 data 里的取值是 `done` / `error`，和 run 的 `succeeded` / `failed` 对应，见第 7 章）。前端本地也可以继续按原型方式，拿到 run 终态就更新节点，两者结果一致。

用户在节点上切换选中的候选：调 3.6 写回服务端（`selectedCandidate` 也不再从快照剥离，但有成功 run 的节点以服务端记录为准）。

### 3.6 选中候选

```
POST /api/v1/canvas/canvases/{id}/nodes/{node_id}/select-candidate
```

```json
{"run_id": "run_20261030_ab12", "index": 1}
```

| 字段 | 说明 |
|---|---|
| `run_id` | 该节点某次**成功** run 的 `run_id`（节点回填的 `data.runId` 是最近一次 run，可能是失败的；成品对应的 run 用 `GET …/runs?node_id=` 取 `status=succeeded` 的那条） |
| `index` | 候选下标，0 起，`< candidates.length` |

成功返回该 run 的 RunNodeResult（`selected_index` 为新值）。之后 `GET /canvases/{id}` 回填的 `selectedCandidate` / `image` 就是这一张。run 不存在 / 不是该节点的：`4001`；run 未成功：`4002`；下标越界：`1003`。

---

## 4. 目录 / 模板

### 4.1 目录（模型 / 参数）

```
GET /api/v1/canvas/catalog
```

替代前端写死的模型表、参考图上限、比例、分辨率、时长和假价（`data/canvasPricing.ts`）。模型表来自 canvas-rpc 配置（`Models`，缺省见下），参考图上限 / 张数上限 / 比例按 ai-gateway `GET /v1/catalog/model-groups` 的 `feature_limits` 收窄（缓存 60 秒），`ready=false` 表示 gateway 没有该组或已停用；Java 按用户报价的价目原样透传在 `java_image_configs` / `java_video_configs`（取不到为 `null`）。

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "image_models": [
      {"model": "Banana", "model_id": "banana-v1", "badge": "超强模型", "max_image_refs": 14, "max_reference_videos": 0,
       "ratios": ["1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2", "4:5", "5:4", "21:9"], "sizes": ["1K", "2K"],
       "qualities": [], "durations": [], "sound": false, "count_max": 4,
       "capabilities": {"quality": false, "count": true, "duration": false, "sound": false},
       "prices": [], "ready": true}
    ],
    "video_models": [
      {"model": "Seedance 2.5", "model_id": "seedance-2-5", "badge": "", "max_image_refs": 3, "max_reference_videos": 1,
       "ratios": ["16:9", "9:16", "1:1"], "sizes": ["720p", "1080p"], "qualities": [],
       "durations": ["4s", "6s", "8s"], "sound": true, "count_max": 1,
       "capabilities": {"quality": false, "count": false, "duration": true, "sound": true},
       "prices": [], "ready": true}
    ],
    "tools": [
      {"tool": "crop", "label": "裁剪", "billing": "free", "ready": true},
      {"tool": "upscale", "label": "高清放大", "billing": "java", "scales": [2, 4], "ready": true},
      {"tool": "outpaint", "label": "扩图", "billing": "java", "ratios": ["1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2"], "ready": true},
      {"tool": "mask", "label": "局部重绘", "billing": "java", "ready": true},
      {"tool": "i2p", "label": "反推提示词", "billing": "gateway", "ready": true},
      {"tool": "tryon", "label": "模特试穿", "billing": "studio", "ready": true}
    ],
    "java_image_configs": [ … ],
    "java_video_configs": [ … ]
  }
}
```

缺省模型表（`model` / `model_id` → gateway 组）：`Image` / `image-v1` → `img.gpt-image-2`（参考图 16，质量 low/medium/high）；`Banana` / `banana-v1` → `img.nano-banana-2.1`（14）；`SD5P` / `sd5p-v1` → `img.seedream-5-pro`（10，**只给 2K**：Seedream 1K 非 1:1 会被拒、4K 静默降 2K）；视频 `Seedance 2.5` / `seedance-2-5`、`Seedance 2.0` / `seedance-2-0`、`Seedance Fast` / `seedance-2-0-fast`、`Seedance Mini` / `seedance-2-0-mini` → `vid.seedance-2.5` / `-2` / `-2-fast` / `-2-mini`。别名（GPT、Nano Banana Pro、Seedream 5 Pro、通用…）也认。视频带首尾帧（`image1` / `image2`）时：不能再接参考图 / 参考视频；选了 Seedance 2.5 会自动改用 `vid.seedance-2`（2.5 不支持首尾帧）。`prices` 由配置给（缺省空），实扣以提交回包 `credits` 为准。

| 字段 | 说明 |
|---|---|
| `model` / `model_id` | 显示名 / 提交用 id |
| `max_image_refs` | 参考图上限（连线校验用它，原型里 Image 14 / Banana 5 / SD5P 3 / 视频 2 或 3 的不一致都以这里为准） |
| `ratios` / `sizes` / `durations` / `qualities` | 可选项 |
| `count_max` | `count` 上限 |
| `capabilities` | 对应原型 `data.capabilities`，决定设置面板显示哪些控件 |
| `prices` | 单价；仅作提示，以提交回包的 `credits` 为准 |
| `tools[].billing` | `free`（裁剪）/ `java`（放大、扩图、重绘）/ `gateway`（反推）/ `studio`（试穿） |

> 模型 id 目前有两套叫法（Image / Banana / SD5P 与 GPT Image 2 / Nano Banana Pro / Seedream 5 Pro），上线用哪套显示名由产品定，前端统一读 `catalog` 返回的 `model`。

### 4.2 模板列表

```
GET /api/v1/canvas/templates?scope=official
```

`scope`：`official`（官方，默认）/ `user`（我保存的）/ `all`。官方模板 3 个：`outfit` 模特换装、`three-view` 商品三视图、`element-replace` 元素替换（参考视频接视频生成节点的 `video` 口，元素图接 `image` 口）。

**列表不返回 `snapshot`**（字段为 `null`，最多 200 条全量快照会超过 RPC 上限）；要插入画布时用 `GET /canvas/templates/{id}` 取详情（含快照）。

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "items": [
      {"id": 1, "scope": "official", "title": "模特换装", "description": "人物图 + 单品图，保脸保姿势，只换衣服",
       "cover_url": "", "node_count": 5, "edge_count": 3, "snapshot": null}
    ]
  }
}
```

| 字段 | 说明 |
|---|---|
| `id` | 模板 id（数字） |
| `scope` | `official` / `user` |
| `title` / `description` | 展示文案 |
| `cover_url` | 封面；官方模板的封面与示例图需运营在后台替换，**为空时前端显示占位图** |
| `node_count` / `edge_count` | 节点 / 连线数 |
| `snapshot` | 列表里恒为 `null`；详情接口返回完整快照，前端「插入当前画布」用它（原型 `insertCanvasTemplate` 的子图 + module 包裹） |

官方模板里的图片节点 `data.image` 是**空串**——示例图要运营换，用户拿到后需自己放图。模板里的 module 节点是模板分组框，`data.width / height / color` 是框的尺寸与底色。

用户模板已实现：

- `POST /api/v1/canvas/templates` `{"canvas_id": "cv_…", "title": "…", "description": "…"}`：把已保存的画布存成我的模板（`scope=user`，运行态与成品剥离），返回模板对象；
- `GET /api/v1/canvas/templates/{id}`：模板详情（官方上架的或自己的）；
- `DELETE /api/v1/canvas/templates/{id}`：删除自己的模板；官方模板返回 `4002`，不存在 / 别人的 `4001`。

### 4.3 从模板建画布

```
POST /api/v1/canvas/canvases/from-template
```

```json
{"template_id": 1, "title": "我的换装画布"}
```

服务端复制模板快照成一张新画布（节点 / 连线 id 重新生成并保持引用一致），返回同 2.2，并附带 `snapshot` 方便前端直接渲染：

```json
{"code": 0, "msg": "ok", "data": {"id": "cv_d41e02", "title": "我的换装画布", "revision": "1761799000001-0c3e", "updated_at": 1761800400000, "snapshot": {"nodes": [ … ], "edges": [ … ]}}}
```

模板不存在或已下架：`code=4001`。

「插入到当前画布」不需要接口：前端用 4.2 返回的 `snapshot` 自己把节点放到现有内容下方（原型已有这套逻辑），保存时随整段快照一起提交。

---

## 5. 快照结构

快照就是 Vue Flow 的 `{nodes, edges}`，服务端不改写结构（运行态字段除外，见 0.1）。

### 5.1 节点

```json
{"id": "m_1761799100_2", "type": "model", "position": {"x": 520, "y": 180}, "data": { }}
```

| `type` | `data.kind` | 含义 | 关键 `data` 字段 |
|---|---|---|---|
| `text` | `text` | 提示词文本 | `text`、`mentions[]` |
| `image` | `input` / `result` | 图片：输入图 / 结果图（`result` 只有一个 `image-in` 输入口） | `image`、`assetId`、`mediaId` |
| `video` | `video-input` | 视频输入 | `video` |
| `model` | `model` | 图片生成 | `model`、`modelId`、`ratio`、`size`、`quality`、`count`、`cfg`、`maxImageRefs`、`capabilities`、`prompt` |
| `model` | `video` | 视频生成（`inputOnly=true` 是「参考视频」，不执行，只做素材） | 同上 + `duration`、`sound` |
| `tool` | `tool` | 工具节点 | `tool`（`crop` / `upscale` / `outpaint` / `i2p` / `mask` / `tryon`）、`sourceNodeId`，以及各工具自己的参数（`crop`、`cropBrush`、`maskModel`、`maskPrompt`、`maskStrokes`、`garmentImage`、`tryonModels` …） |
| `module` | `module` | 模板分组框（`draggable=false`、`selectable=false`、`zIndex=-1`） | `width`、`height`、`color` |

公共 `data` 字段：`title`、`subtitle`、`width`、`height`。运行态字段（服务端回填，保存时被剥离）：`status`（`idle` / `queued` / `running` / `done` / `error`）、`progress`、`runId`、`candidates`（URL 数组）、`selectedCandidate`、`image` / `video`（成品）、`assetId`、`errorMessage`、`requestedCount`、`resultText`、`resultImage`、`tryonResults`。

> 注意： `image` 在输入图节点上是**用户内容**（要保存），在模型节点上是**成品**（运行态，回填）。服务端按节点类型区分：只剥离 `type=model` 与 `type=tool` 节点的成品字段，`type=image` / `video` 输入节点的 `image` / `video` 始终保留。

### 5.2 连线

```json
{"id": "e_1761799300_5", "type": "canvas-bezier", "source": "n_1", "target": "m_2",
 "sourceHandle": "image", "targetHandle": "image", "data": {"color": "#38bdf8", "order": 1}}
```

| 字段 | 说明 |
|---|---|
| `source` / `target` | 节点 id |
| `sourceHandle` | 来源输出口：`text`（文字）、`image`（图片）、`video`（视频）、`image-out`（模型 / 工具的输出）、`candidate-N`（模型节点的第 N 个候选单独连出） |
| `targetHandle` | 目标输入口：`prompt`、`image`、`video`、`audio`，以及别名 `first` / `last` / `image1` / `image2`（视频首尾帧 / 图 1 图 2）、`garment` / `models`（试穿）、`image-in`（结果图片节点） |
| `data.order` | 同一输入口内的序号，1 起；提示词口没有。`references[].index` 就取它 |
| `data.color` | 线色，纯展示 |

---

## 6. 连线规则与校验

规则以原型 `validateConnection` 为准，**服务端在保存和执行时用同一套再校一遍**（前端校验是体验，服务端是兜底）。

### 6.1 规则表

| 目标口 | 接受的来源 | 上限 |
|---|---|---|
| `prompt`（提示词） | `text` 节点；`tool` 节点且 `tool=i2p`（反推文本） | **1**；新线替换旧线 |
| `image`（参考图）及别名 | `image`、`model`、`video`、`tool` 节点 | `maxImageRefs`（图片 / 视频模型各自的上限，见 `catalog`）；多条线汇聚 |
| `image1` / `image2`（视频模型的首图 / 次图口） | 同上 | 各 **1**（同时也占 `image` 的名额） |
| `video`（参考视频） | 只收**视频模型**（`kind=video`）的输出 | 1 |
| `audio` | 暂不接入 | 0 |
| `garment`（试穿服装） | 图片类来源 | 1 |
| `models`（试穿模特） | 图片类来源 | 9 |
| `image-in`（结果图片节点） | 图片类来源 | 1 |
| `image`（工具的输入图：裁剪、高清放大、扩图、局部重绘、反推） | `image`、`model`、`video`、`tool` 节点 | 1 |

另外：

- 目标只能是 `model` 节点、工具节点（试穿用 `garment` / `models`，其它工具只有 `image` 口）、结果图片节点（`image` 且 `kind=result`）；文字节点、分组框没有输入口。
- 不能连自己，不能成环（从目标出发能走回来源就是环）。
- 同一对 `(source, sourceHandle, target, targetHandle)` 不重复。
- 候选单独连线：`sourceHandle=candidate-N` 只引用上游第 N 个候选，运行时 `references[]` 里带 `candidate_index`。

### 6.2 校验错误

保存（PUT）或执行（run）不满足规则时返回 `code=1003`，`msg` 直接说明位置与原因，整次请求不生效：

| `msg` 示例 | 触发 |
|---|---|
| 节点 m_2 的参考图最多接 5 条 | 超 `maxImageRefs` / 各口上限 |
| 提示词端口只收文字节点 | 来源类型不对 |
| 参考视频端口只收视频模型的输出 | 来源类型不对 |
| 会形成环 | 成环 |
| 节点 e_9 引用的节点不存在 | 连线指向不存在的节点 |
| 节点 n_3 的图片不能是 dataURL，请先上传 | 快照里有 `data:` 地址 |
| 画布内容超过 2MB | 快照超限 |
| 节点 id 重复 | 同一快照里 `nodes[].id` 重复 |

`msg` 的原文以实现为准，前端别匹配文案，直接展示。

---

## 7. 状态机与进度

### 7.1 run 状态

| `status` | 含义 | 终态 |
|---|---|---|
| `submitting` | 提交中：已落库（`task_id` 已生成），正在冻结点数；通常只持续毫秒级，提交接口返回时已是 `queued` 或之后的状态 | 否 |
| `queued` | 已入队（gateway 后端已冻结点数） | 否 |
| `running` | 生成中 | 否 |
| `succeeded` | 成功，`candidates` / `video_url` / `text` 有值 | **是** |
| `failed` | 失败，`error` 有值；gateway 后端点数已退回（`UPSTREAM_UNKNOWN` 除外，见 7.4） | **是** |
| `cancelled` | 已取消；gateway 后端点数已退回；Java 工具已调 Java 取消接口、由 Java 退款（无有效登录态时为停止跟踪，`error` 注明） | **是** |

流转：`submitting → queued → running → succeeded | failed`；冻结失败 `submitting → failed`；`submitting` / `queued` / `running` 都可以被取消。同一 run 终态之后不会再变。同一节点同一时刻只有一个未结束的 run（再提交回 `4002`，库里有唯一约束兜底）。

节点 `data.status` 与 run 的对应：`submitting / queued → queued`、`running → running`、`succeeded → done`、`failed / cancelled → error`（取消时 `errorMessage` 为「已取消执行」），还没跑过 = `idle`。前端遇到不认识的 `status` 按未结束处理、继续轮询。

### 7.2 progress

`progress` 是 0–100 的估算值：排队 0，生成中是估算（1–99），成功 100，失败 / 取消不要读它。`count>1` 时是整次 run 的进度，不是单张。部分候选失败但至少一张成功时，run 记 `succeeded`，`candidates` 只含成功的，`requested_count` 大于 `candidates` 长度，前端据此显示「成功 n/m」；失败那几张的点数已退回，`credits` 只算成功的。

### 7.3 轮询建议

- 间隔 **1.2 秒**（原型值），不要更快。
- 总时限：图片 **5 分钟**、视频 **15 分钟**（原型值）。超时**不要主动取消**——任务可能还在跑，提示「生成超时，任务可能仍在运行」，让用户决定继续等还是取消。
- 单次轮询请求超时 / 网络错误 ≠ 任务失败：原型是同一次最多重试 2 次、间隔 0.8 秒，再抛错；仍然不要取消。
- 页面关闭后 run 仍会在服务端跑完（计费、入素材库照常）。重新打开画布时节点 `status` 可能是 `running`，前端应按 `data.runId` 对应的 run 继续轮询；`runId` 不是 `request_id`，可直接取节点回填的 `data.requestId`（服务端回填，即该节点最近一次 run 的 `request_id`）。
- 用户点停止：先调 `cancel`，再把本地轮询中止；不要只中止本地轮询。

### 7.4 计费口径

- **图片 / 视频生成**：每次 run 一张预留单，提交时冻结，成功才结算；`count>1` 部分失败只退失败张；取消退回未完成部分。
- **裁剪**：不计点。
- **放大 / 扩图 / 局部重绘**：由 Java 兼容工具按其口径计费，画布侧只记 run 与结果引用。用户取消、运营强制失败、超时、所在画布被删时，canvas-rpc 调 Java 取消接口 `POST /api/v1/creation/{taskId}/cancel` 真实取消，**由 Java 退款**（Go 不退）；用户取消用本次请求的 Bearer，其余场景用该用户最近一次请求留下的 Bearer（canvas-rpc 内存缓存 2 小时）。没有有效登录态时退化为**停止跟踪**——Java 任务可能仍会执行并扣费，run 的 `error` 写明，canvas-rpc 打 WARN（`check=canvas.java_cancel_degraded`）并计数（`canvas_java_cancel_degraded_total`），运营可到 Java 后台手动取消。取消时 Java 已成功：用户取消 / 超时 / 画布已删按成功交付，强制失败照常置失败并注明「Java 任务已成功并已扣费」。提交暂时失败、还没拿到任务号的 run，先按同一幂等键逐字节重放找回任务号再取消（Java 没收到首次请求时重放会新建任务并冻结点数，随即取消、由 Java 退款；同键内容不同 `1003` 时无法找回，按停止跟踪处理）。
- **反推**：gateway 多模态调用，按 run 计费。
- **试穿**：按 studio 商拍口径，见 `api-studio.md` 第 7 章。
- 画布原型的假价（`canvasPricing.ts`）和 `credits: 0` 全部作废，页面上的预估价读 `catalog.prices`。
- **结果未知（`UPSTREAM_UNKNOWN`，后台 `billing_status=review`）**：提交后不立即退款，冻结单到期由 canvas-rpc 内置的回收器（Reclaimer）**自动退回**（图片 30 分钟、视频 200 分钟，按计费配置 `Hold`）；人工对账确认已扣费的再另行处理。
- **Seedream 分辨率归一（2026-10-31）**：`SD5P`（`img.seedream-5-pro`）的 `size` 在**提交受理时**就归一：`4K` / `1K` / 空 → `2K`（Seedream 1K 非 1:1 会被拒、4K 会被上游静默降 2K）。冻结与结算的计费档位（`resolution=2K`）、run 的 `request` 快照、发给 gateway 的请求三者一致，都按 2K；改版前按用户传的 4K 冻结、实际只生成 2K 的口径作废。归一时提交回包 `warnings` 带告警；`4K` 不再报「不支持分辨率」。其它模型不归一。
- **结果未知的计费镜像回写（2026-10-31）**：`UPSTREAM_UNKNOWN` 的 run 置失败、`billing_status=review`、预留单仍 `HELD`；canvas-rpc 的 Poller 每分钟挑出「run 已终态且 `billing_status` ∈ {`review`, `held`}」（最近 7 天内更新过）的 gateway run，按预留单**真实状态**（只读查 `afv_billing_reservation`，`biz_id` 折叠规则同 `pkg/billing`）CAS 回写：`REFUNDED` / `EXPIRED`（回收器到期释放）/ `CANCELLED` → `refunded`；`CONFIRMED` / `PARTIALLY_REFUNDED`（人工确认扣费）→ `settled`；仍 `HELD`（未到期）或查不到单的不动。所以 review 的 run 在冻结单到期释放后约 1 分钟内后台显示「已退回」。
- **已交付但确认扣费失败**（预留单已过期回收 / 已退，后台 `billing_status=unsettled`）：成品照常给用户，服务端记 ERROR 日志待人工对账，不会重复扣费。

---

## 8. 提示词库

提示词库只有一份数据，画布面板和各工具页的 `PromptLibraryFlyout` 共用。官方提示词 19 条（电商 12 条、通用 7 条）由运营在库里维护，用户只能增删改**自己的**；原型里的 `mine-1`、`mine-2` 是演示数据，不迁移。「插入」提示词不需要后端，前端直接把正文放进文字节点 / 输入框。

> 官方提示词共 19 条，其中 8 条来自第三方 GitHub 来源，已于 2026-10-31 确认授权并启用，`source` 字段展示原始来源。

### 8.1 字段

`PromptItem`（原型 → 接口）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | int | 提示词 id（原型是字符串，如 `ecom-model-1`） |
| `title` | string | 标题，≤100 字 |
| `prompt` | string | 正文，≤3000 字 |
| `vertical` | string | 一级分类：`ecommerce`（电商）/ `general`（通用）/ `mine`（我的）。注意是**英文枚举**，原型是中文 |
| `group` | string | 二级分类，英文 key，见下表。原型叫 `group`，库里存 `group_key` |
| `models` | string[] | 适用模型，取值 `通用` / `GPT` / `Banana` / `Seedream`（沿用原型的中文值） |
| `tags` | string[] | 标签 |
| `source` | string | 来源说明，如 `小岐AI 内置`、`Awesome GPT Image` |
| `owner` | string | `official` / `mine`，是否可编辑 |
| `created_at` / `updated_at` | int | 毫秒 |

二级分类 `group` 取值：

| `vertical` | `group` | 原型名称 |
|---|---|---|
| `ecommerce` | `model` | 模特 / 人像 |
| `ecommerce` | `scene` | 场景 |
| `ecommerce` | `product` | 商品图 / 主图 |
| `ecommerce` | `detail` | 详情页 |
| `ecommerce` | `apparel` | 服装 |
| `general` | `photo` | 摄影 / 光线 |
| `general` | `composition` | 构图 / 镜头 |
| `general` | `style` | 风格 / 艺术 |
| `general` | `material` | 材质 / 质感 |
| `general` | `text` | 文字 / 排版 |
| `mine` | `favorite` | 我的收藏（原型 `收藏`） |

用户自己的提示词 `vertical` 固定为 `mine`，`group` 固定为 `favorite`，创建时可不传。

### 8.2 列表

```
GET /api/v1/prompt-library/prompts?vertical=ecommerce&group=model&model=GPT&q=身份&page=1&page_size=20
```

| 参数 | 说明 |
|---|---|
| `vertical` | 一级分类；不传 = 官方全部 + 我的全部 |
| `group` | 二级分类 |
| `model` | 适用模型（`models` 数组包含即命中） |
| `q` | 关键词，匹配标题 / 正文 / 标签 |
| `page` / `page_size` | 默认 1 / 20，上限 50 |

`vertical=mine` 只返回当前用户自己的；其它分类返回官方（`enabled=1`）。

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "items": [
      {"id": 101, "title": "同一模特 · 身份锁",
       "prompt": "同一位模特，参考图第 1 张为唯一身份基准。…",
       "vertical": "ecommerce", "group": "model",
       "models": ["通用", "GPT", "Seedream"], "tags": ["模特一致性", "身份锁", "换装"],
       "source": "小岐AI 内置", "owner": "official",
       "created_at": 1761800000000, "updated_at": 1761800000000}
    ],
    "total": 1
  }
}
```

排序：官方按运营配置的顺序，我的按最近创建在前。

### 8.3 新增

```
POST /api/v1/prompt-library/prompts
```

```json
{"title": "我的 · 香水冷调主图", "prompt": "白瓷香水瓶放在浅蓝色背景上，柔和棚拍光，1:1。", "models": ["通用", "GPT"], "tags": ["香水", "冷调"]}
```

对应原型 `UserPromptInput {title, prompt, group, models[], tags[]}`；`group` 不传（见 8.1）。`title`、`prompt` 必填；`models` 缺省 `["通用"]`。返回完整 `PromptItem`（含 `id`）。超长 / 缺必填：`code=1003` / `1002`。

### 8.4 修改

```
PUT /api/v1/prompt-library/prompts/{id}
```

body 同新增，整体覆盖。只能改自己的：官方条目返回 `code=4002`（「官方提示词不可修改」）；不存在或别人的返回 `4001`。

### 8.5 删除

```
DELETE /api/v1/prompt-library/prompts/{id}
```

成功 `{"code":0,"msg":"ok","data":{}}`。规则同修改：官方 `4002`，不存在 / 别人的 `4001`。

---

## 9. 与原型 `services/canvasApi.ts` 的字段对照

适配层只改 `httpCanvasApi` 一份，页面不动。camelCase → snake_case，以及形状变化：

### 9.1 路径

| 原型 | 新接口 |
|---|---|
| `GET /api/workflows` | `GET /api/v1/canvas/canvases` |
| `POST /api/workflows` `{title}` | `POST /api/v1/canvas/canvases` |
| `GET /api/workflows/{id}/canvas` | `GET /api/v1/canvas/canvases/{id}` |
| `PUT /api/workflows/{id}/canvas` | `PUT /api/v1/canvas/canvases/{id}` |
| `DELETE /api/workflows/{id}` | `DELETE /api/v1/canvas/canvases/{id}` |
| （前端 createWorkflow + saveCanvas 组合） | `POST /api/v1/canvas/canvases/{id}/save-as` |
| `POST /api/workflows/{wf}/nodes/{node}/run` | `POST /api/v1/canvas/canvases/{id}/nodes/{node_id}/run` |
| `GET /api/runs/{requestId}` | `GET /api/v1/canvas/runs/{request_id}` |
| `POST /api/runs/{requestId}/cancel` | `POST /api/v1/canvas/runs/{request_id}/cancel` |
| `POST /api/uploads`（multipart） | 直传 OSS（`api-app.md`），不再走此接口 |
| 写死的模型表 / 假价 | `GET /api/v1/canvas/catalog` |
| 写死的 3 个模板 | `GET /api/v1/canvas/templates` |

### 9.2 信封与协议

| 原型 | 新接口 |
|---|---|
| 裸 JSON，HTTP 状态码表达错误 | 信封 `{code, reason, msg, data}`，HTTP 恒 200（未登录除外） |
| HTTP 409 + `CanvasConflictError` | `code=4002` 且 `data.conflict=="revision"` |
| 请求头 `If-Match: <revision>` | body `if_match_revision` |
| 响应头 `ETag` 当 revision | `data.revision` |
| 强制覆盖：不带 `If-Match` | body `force: true` |
| 保存回包 `{revision}` 或 204 | `data.revision` |
| `Idempotency-Key: runId` | 不变，值 = body `run_id` |
| 时间 ISO 字符串 | 毫秒整数 |

### 9.3 画布

| 原型 | 新接口 | 备注 |
|---|---|---|
| `CanvasSnapshot.workflowId` | `id`（外层） | 快照内不再带 |
| `title` | `title` | |
| `updatedAt` | `updated_at` | 毫秒 |
| `revision` | `revision` | 字符串 |
| `nodes` / `edges` | `snapshot.nodes` / `snapshot.edges` | **多包了一层 `snapshot`** |
| `WorkflowSummary.nodeCount` | `node_count` | |
| `WorkflowSummary.cover` | `cover_url` | |
| `SaveOptions.baseRevision` | `if_match_revision` | |
| `SaveOptions.force` | `force` | |
| 快照内节点 / 边字段 | 不变（仍 camelCase） | 见 1.4 |

### 9.4 `RunNodeRequest`

| 原型 | 新接口 | 备注 |
|---|---|---|
| `workflowId` / `nodeId` | 路径参数 | 不放 body |
| `runId` | `run_id` | |
| `model` | `model` | |
| `modelId` | `model_id` | |
| `kind` | `kind` | 新增取值 `tool` |
| `prompt` | `prompt` | |
| `references[]` | `references[]` | 见下 |
| `ratio` / `size` / `quality` | 同名 | `quality` 提交英文 |
| `duration` / `sound` / `count` / `cfg` | 同名 | |
| — | `tool` / `params` | 新增，工具节点用 |

`references[]`：

| 原型 | 新接口 |
|---|---|
| `handle` / `index` / `text` / `image` / `video` / `audio` / `mime` | 同名 |
| `nodeId` | `node_id` |
| `assetId` | `asset_id` |
| `mediaId` | `media_id` |
| `sourceHandle` | `source_handle` |
| `candidateIndex` | `candidate_index` |

### 9.5 `RunNodeResult`

| 原型 | 新接口 | 备注 |
|---|---|---|
| 提交返回 `{requestId}` | `request_id`，另有 `run_id` / `status` / `replayed` / `credits` | |
| `requestId` | `request_id` | |
| `runId` | `run_id` | |
| `status` | `status` | 取值不变 |
| `candidates: string[]` | `candidates: [{url, width, height}]` | **形状变了**：适配层取 `.url` 还原成 `string[]` |
| `videoUrl` | `video_url` | 无则空串（原型是 undefined） |
| `credits` | `credits` | 真实点数 |
| `model` | `model` | |
| `progress` | `progress` | |
| `error` | `error` | |
| `errorCode` | `error_code` | |
| `retryable` | `retryable` | |
| — | `canvas_id` / `node_id` / `kind` / `selected_index` / `text` / `requested_count` / `created_at` / `finished_at` | 新增 |

### 9.6 `PromptItem` / `UserPromptInput`

| 原型 | 新接口 | 备注 |
|---|---|---|
| `id: string` | `id: int` | |
| `vertical: '电商' \| '通用' \| '我的'` | `ecommerce` / `general` / `mine` | 英文枚举 |
| `group: '模特' …` | `group: model …` | 英文 key，见 8.1 |
| `models` / `tags` / `title` / `prompt` / `source` | 同名 | `models` 值仍是中文 |
| — | `owner` / `created_at` / `updated_at` | 新增 |
| localStorage | 服务端 | 用户数据迁服务端 |

### 9.7 删掉的东西

- `mockCanvasApi` 全部（localStorage 存画布、`RESULT_POOL`、`MOCK_VIDEO`），不迁移；原型 `demo-workflow` 不迁。
- 前端本地伪实现的 6 个工具（canvas 裁剪、放大拉伸、扩图返原图、固定文案反推、重绘返原图、试穿回填）。
- `canvasPricing.ts` 假价。
- `uploadMedia` 的 multipart 路径（改直传 OSS）与 mock 的 FileReader→dataURL 逻辑。
- 「Web Locks 单标签」是前端行为，不变。
