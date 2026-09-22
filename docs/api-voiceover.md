# 数字人口播 · 前端接口文档

对应设计稿「一键成片 → 数字人口播」的 3 张页面：**主页**、**生成设置**弹窗、**选择AI文案**弹窗。

数字人口播是**一步出片**：填好文案、人物图、背景和生成设置，点「开始生成」就直接出视频，**没有「规划剧本」这一步**（设计稿已删）。

通用约定（请求头、响应格式、错误码、直传 OSS）和其他模块一样，完整说明见 [`api-app.md`](api-app.md)，下面只列口播用到的。

---

## 0. 通用约定（摘要）

- 网关：`http://<host>:8888`，所有接口前缀 `/api/v1`。
- 每个请求都带 `X-User-Id: <用户ID>`（开发模式；接正式登录后只换请求头，接口不变）。
- **提交生成**必须带 `X-Request-Id: <UUID>`：每次点「开始生成」新生成一个，网络失败重试时**沿用同一个**，避免重复生成、重复扣点。
- 响应统一是 `{"code":0,"msg":"ok","data":{…}}`，HTTP 状态码都是 200，用 `code` 判断；`code != 0` 时直接把 `msg` 提示给用户（错误码表见第 9 节）。
- 时间都是 unix 毫秒。
- **字段命名**：请求体、查询参数、响应一律下划线（`image_url`、`portrait_source`），不会改成小驼峰。
- **未登录**：HTTP 200，`code=1002`（`msg`「未登录」）。接入正式登录后，token 无效/过期也是这个码，端上据此重新登录。
- **跨域**：网关允许任意来源（含 App 离线包的 `Origin: null`），并放行 `X-User-Id`、`X-Request-Id` 的预检（OPTIONS 返回 204）。
- **在线文档**：`http://<host>:<端口>/swagger`，可在页面上调试（先点 Authorize 填 `X-User-Id`）。

---

## 1. 页面与接口对应

| 页面区域 | 用到的接口 |
|---|---|
| 进页面：取表单配置 | `GET /image-creations/options?kind=voiceover` |
| 视频文案 · 手动输入 | 无（提交时带上即可） |
| 视频文案 · 「AI帮写」 | `POST /copywriting/ai-write` |
| 视频文案 · 「AI文案」→ 选择AI文案弹窗 | `GET /copywriting?keyword=` |
| 出镜人物图 · 上传照片 | `GET /upload/credentials?kind=image`，然后直传 OSS |
| 出镜人物图 · 「AI公共数字人」 | `GET /digital-humans/public` |
| 出镜人物图 · 「AI定制数字人」 | `POST /digital-humans/portraits`，轮询 `GET /digital-humans/portraits/:id` |
| 背景图片 · 预设背景（如「卧室」） | `GET /image-creations/templates?kind=voiceover` |
| 背景图片 · 「导入背景」 | `GET /upload/credentials?kind=image`，然后直传 OSS |
| 背景图片 · 「AI智能匹配」 | 无（提交时不传背景） |
| 生成设置弹窗 | 用 options 里的 `models` / `durations` / `sizes` |
| 「开始生成」 | `POST /image-creations` |
| 生成中 / 结果 | 轮询 `GET /image-creations/:id` |
| 历史记录 | `GET /image-creations?kind=voiceover`、`DELETE /image-creations/:id` |

---

## 2. 进页面：表单配置

```
GET /api/v1/image-creations/options?kind=voiceover
```

返回（实际响应）：

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "models": [
      {"code": "seedance2.5", "label": "seedance 2.5"},
      {"code": "lite",        "label": "XQ Video 1.0 Lite"}
    ],
    "default_model": "seedance2.5",
    "durations": [15, 30],
    "default_duration": 15,
    "sizes": [{"code": "9:16", "label": "9:16"}, {"code": "16:9", "label": "16:9"}],
    "default_size": "9:16",
    "max_content_len": 0,
    "max_images": 1,
    "max_materials": 0,
    "template_required": false,
    "poster_types": [], "poster_styles": []
  }
}
```

| 字段 | 用在哪 |
|---|---|
| `models` / `default_model` | 生成设置 ·「模型选择」，展示 `label`，提交传 `code` |
| `durations` / `default_duration` | 生成设置 ·「视频时长」，单位秒，展示成「15s」「30s」 |
| `sizes` / `default_size` | 生成设置 ·「视频比例」 |
| `max_content_len` | 视频文案字数上限，**`0` 表示不限**（口播当前就是不限） |

主页底部「生成设置」那一行的摘要（设计稿：`Seedance 2.0/15s/9:16`）用当前选中的模型 `label`、时长、比例拼出来即可。

> 其余字段（`poster_types`、`max_images` 等）是海报/长图共用接口带出来的，口播忽略。

---

## 3. 视频文案

必填，**字数不限**。三种来源最后都是填进同一个输入框，提交时作为 `content` 传。

### 3.1 AI帮写

```
POST /api/v1/copywriting/ai-write
Content-Type: application/json

{"scene": "voiceover", "context": "西湖边的咖啡店，主打手冲", "max_len": 60}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `scene` | ✅ | 固定 `voiceover` |
| `context` | | 用户已经输入的内容（可以只是几个关键词），**可以为空**（为空时按场景写一段通用口播），最多 500 字 |
| `max_len` | | 期望字数，**不传就不限**。需要时可按所选时长给个参考：15 秒约 60 字，30 秒约 120 字 |

返回 `{"text": "…"}`，同步返回，几秒内完成。建议把 `text` 回填到输入框让用户再改。

### 3.2 AI文案（选择AI文案弹窗）

```
GET /api/v1/copywriting?keyword=睡眠&page=1&page_size=20
```

返回 `{"list": [...], "total": 12}`，每条：

```json
{"id": 31, "title": "改善睡眠环境的6个关键要素", "body": "1. 光线｜避光降噪……",
 "content_type": "knowledge", "content_type_label": "知识", "word_count": 500,
 "status": 1, "create_time": 1790050017000}
```

- 弹窗卡片：左上角标签用 `content_type_label`，标题 `title`，摘要取 `body` 前几行，底部 `word_count` + `create_time`。
- 搜索框传 `keyword`。
- **只让用户选 `status == 1`（已完成）的**；`0` 是还在生成、`2` 是生成失败。
- 点「应用所选文案」：把 `body` 填进文案输入框。

---

## 4. 出镜人物图（三选一，必选）

提交时用 `portrait_source` 标明来源，再带对应的字段：

| 来源 | `portrait_source` | 另外要传 |
|---|---|---|
| 上传照片 | `UPLOAD` | `image_url`：直传 OSS 后的地址 |
| AI公共数字人 | `PUBLIC` | `public_id` |
| AI定制数字人 | `AI` | `portrait_id` + `image_index`（选中第几张，**0 起**） |

### 4.1 上传照片（预签名直传，推荐）

后端签好上传地址，前端只管 PUT 文件，**不需要任何密钥，也不用实现 OSS 签名**。

1. 取上传地址：

   ```
   POST /api/v1/upload/presign
   {"kind": "image", "content_type": "image/jpeg"}
   ```

   ```json
   {"put_url": "https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/aigc-life/image/123/20260922/2aaf….jpg?OSSAccessKeyId=…&Expires=…&Signature=…",
    "url": "https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/aigc-life/image/123/20260922/2aaf….jpg",
    "content_type": "image/jpeg",
    "expires_at": 1790051817000}
   ```

2. 把文件原样 PUT 到 `put_url`，请求体就是文件内容，**必须带 `Content-Type: <返回的 content_type>`**（签名包含它，不一致 OSS 返回 403）。HTTP 200 即成功。
3. 提交时把 `url` 作为 `image_url` 传。

- `kind`：`image`（照片、背景图）或 `video`（动作迁移的动作视频）。音频目前不需要前端上传。
- `content_type`：图片 `image/jpeg` `image/png` `image/webp` `image/gif` `image/heic`，视频 `video/mp4` `video/quicktime`。
  对象名（含扩展名）由后端生成，扩展名与类型一致，每次都是新的随机名，不会覆盖。
- `put_url` 默认 30 分钟有效，**一个地址只传一个文件**，每个文件单独取。
- 大小与格式由前端控制，后端不再校验（设计稿提示「建议上传模特半身照片，增加成功率」）。
- OSS 已允许跨域 PUT（含离线包的 `Origin: null`），不需要额外配置。
- 后端只接受本服务签发的地址（`…/aigc-life/…`），别的外链会报「请先上传照片」/「请先通过上传接口上传」。
- 上传后的地址是公有桶的永久地址，不带过期签名。

> 另有 `GET /upload/credentials?kind=image`（STS 临时凭证，前端自行签名上传），功能相同，保留兼容，新接入请用预签名。
> 本地联调没有 OSS 时可用 `POST /api/v1/upload`（multipart，字段 `file`）代传，**仅本地可用**。

### 4.2 AI公共数字人

```
GET /api/v1/digital-humans/public
```

```json
{"list": [{"id": 5, "name": "小美", "role_label": "店长",
           "image_url": "https://…/xiaomei.png", "avatar_url": "https://…/xiaomei-avatar.png"}]}
```

列表展示用 `avatar_url` + `name` + `role_label`，选中后提交 `portrait_source=PUBLIC` + `public_id=id`。

### 4.3 AI定制数字人

和「AI数字人」模块是**同一套接口**，流程是：填表 → 出 4 张图 → 选一张。

1. `GET /api/v1/digital-humans/options` 取表单选项（性别、年龄、肤色、职业角色）。
2. `POST /api/v1/digital-humans/portraits`（要带 `X-Request-Id`）：

   ```json
   {"gender": "female", "age": "youth", "skin_tone": "fair", "role": "store_manager", "description": "长发，微笑"}
   ```

3. 轮询 `GET /api/v1/digital-humans/portraits/:id`，直到 `status != 0`：`1` 完成，`image_urls` 里有 4 张 9:16 图；`2` 失败，看 `fail_reason`。
4. 用户选中第 N 张（0 起）后，提交口播时传 `portrait_source=AI` + `portrait_id` + `image_index=N`。

注意：
- 生成形象前要求用户已同意《AIGC功能使用约定》，没同意会返回 **1007**，这时弹约定弹窗，同意后调 `POST /agreements/aigc/accept` 再重试（和 AI数字人 模块一致）。
- 形象还没生成完就拿去提交口播，会返回 **1006**「形象还未生成完成」。
- 形象接口的完整说明见 `api-app.md` 第二节。

---

## 5. 背景图片（三选一，选填）

| 设计稿选项 | `background_source` | 另外要传 | 说明 |
|---|---|---|---|
| AI智能匹配 | `AI`（或不传） | — | **默认**。不给执行面传背景，由模型按文案和人物自己匹配 |
| 预设背景（如「卧室」） | `PRESET` | `background_id` | 运营在后台维护的背景图 |
| 导入背景 | `UPLOAD` | `background_url` | 用户自己上传，流程同 4.1（`kind=image`） |

预设背景列表：

```
GET /api/v1/image-creations/templates?kind=voiceover
```

```json
{"list": [{"id": 21, "title": "卧室", "url": "https://…/bedroom.png"}]}
```

卡片用 `url` 做缩略图、`title` 做名称。列表可能为空（运营还没配），这时只显示「AI智能匹配」和「导入背景」两个选项。

---

## 6. 开始生成

```
POST /api/v1/image-creations
X-Request-Id: 4F1C7E0A-9D2B-4E8B-A1F0-2C3D4E5F6A7B
Content-Type: application/json
```

请求示例：

```jsonc
{
  "kind": "voiceover",
  "content": "欢迎光临我们的小店，今天给大家推荐一款超好用的洗面奶。",

  // 人物图（三选一，见第 4 节）
  "portrait_source": "PUBLIC",
  "public_id": 5,

  // 背景（三选一，见第 5 节；不传 = AI智能匹配）
  "background_source": "PRESET",
  "background_id": 21,

  // 生成设置（不传用 options 里的默认值）
  "model": "seedance2.5",
  "duration": 15,
  "size": "9:16"
}
```

| 字段 | 必填 | 取值 |
|---|---|---|
| `kind` | ✅ | 固定 `voiceover` |
| `content` | ✅ | 口播文案，不能为空，字数不限 |
| `portrait_source` | ✅ | `UPLOAD` / `PUBLIC` / `AI` |
| `image_url` | `UPLOAD` 时 | 直传后的照片地址 |
| `public_id` | `PUBLIC` 时 | 公共数字人 id |
| `portrait_id` / `image_index` | `AI` 时 | 定制形象记录 id / 选中第几张（0 起） |
| `background_source` | | `AI`（默认）/ `PRESET` / `UPLOAD` |
| `background_id` | `PRESET` 时 | 预设背景 id |
| `background_url` | `UPLOAD` 时 | 直传后的背景图地址 |
| `model` | | `seedance2.5`（默认）/ `lite` |
| `duration` | | `15`（默认）/ `30` |
| `size` | | `9:16`（默认）/ `16:9` |

成功后**立刻**返回记录（`status=0` 生成中），拿 `data.id` 去轮询：

```json
{"code": 0, "msg": "ok", "data": {
  "id": 143, "kind": "voiceover", "status": 0,
  "content": "欢迎光临我们的小店，今天给大家推荐一款超好用的洗面奶。",
  "input_image_url": "https://…/5b2cf100268432a73b611cf3.png",
  "input_bg_url": "", "model": "seedance2.5", "duration": 15, "size": "9:16",
  "image_urls": [], "fail_reason": "", "create_time": 1790050017000
}}
```

- 同一个 `X-Request-Id` 重复提交，返回的是**同一条**记录，不会重复生成：
  网络超时不知道有没有提交成功时，**沿用原 ID 重发**即可——后端已受理就原样返回那条记录（**无论它现在是生成中、已完成还是已失败**，都是正常返回 `code=0` + 记录当前状态，不报错）；没受理就当新请求处理。
  用户主动点「重新生成」才换新 ID。
- 按钮上的「10」点：**计费还没接**，目前不扣点。接入后余额不足会返回 1008。

---

## 7. 生成中与结果

```
GET /api/v1/image-creations/:id
```

轮询直到 `status != 0`：

| `status` | 含义 | 前端 |
|---|---|---|
| `0` | 生成中 | 继续轮询 |
| `1` | 已完成 | 视频地址在 `image_urls[0]`（mp4） |
| `2` | 失败 | 展示 `fail_reason`，引导用户重试（**重试要换新的 `X-Request-Id`**） |

完成时（实际响应）：

```json
{"code": 0, "msg": "ok", "data": {
  "id": 143, "kind": "voiceover", "status": 1,
  "image_urls": ["https://…/aigclife_75ee423f25a2e34f5ee7797a9f09e586.mp4"],
  "input_image_url": "https://…/5b2cf100268432a73b611cf3.png",
  "input_bg_url": "", "model": "seedance2.5", "duration": 15, "size": "9:16",
  "content": "欢迎光临我们的小店，今天给大家推荐一款超好用的洗面奶。",
  "fail_reason": "", "create_time": 1790050017000
}}
```

- 字段名叫 `image_urls` 是因为和海报/长图共用一张表，口播里放的是**视频地址**，固定只有 1 个。
- 封面可以用 `input_image_url`（人物图），素材库里也是用它当封面。
- **耗时**：云端（seedance 2.5）一般 2–10 分钟，30 秒视频和 lite 可能更久。建议 **5–10 秒轮询一次**；用户离开页面后可以停止轮询，回来再查，后端会继续跟进结果。
- 超过 **30 分钟**还没出结果，后端会按失败处理（`fail_reason`「生成失败，请重试」）。这是**后端定时任务**判定的（每 5 秒巡检一次），
  不依赖前端查询：用户很久之后回来查，拿到的已经是最终状态，不会一直是 `0`。
- **成品地址**：已转存到我们的公有桶，是**永久地址，不带过期签名**，可直接预览、下载。
- **封面**：没有单独的封面图，用 `input_image_url`（人物图）当封面；素材库里也是这样。
- **水印**：由 AIGC 执行面决定，目前还在对接真实执行面，确认后补充。
- 成品会自动进素材库的「数字人口播」分类。

---

## 8. 历史记录与删除

```
GET    /api/v1/image-creations?kind=voiceover&status=-1&page=1&page_size=20
DELETE /api/v1/image-creations/:id
```

- 列表返回 `{"list": [...], "total": N}`，每条结构同第 7 节，按创建时间倒序。
- 分页：`page` 从 **1** 开始（默认 1），`page_size` 默认 20、**最大 100**（超过按 100）。
- 生成中的条目也带 `input_image_url`（提交时就确定了），可以直接当缩略图。
- `status` 可筛选：`-1` 全部（默认）、`0` 生成中、`1` 已完成、`2` 失败。
- 删除后素材库里对应的视频也会一并删除。
- **生成中也可以删**：删除后后端不再跟进这条记录，出结果也不会写进素材库。
  但执行面那边的任务没有取消接口，**已经提交的会继续跑完**；计费规则（生成中删除是否退点）等计费方案确定后再定。

---

## 9. 错误码与提示文案

`code != 0` 时直接展示 `msg`。口播可能遇到的：

| code | `msg`（实际文案） | 什么情况 |
|---|---|---|
| 1001 | 请输入口播文案 | `content` 为空 |
| 1001 | 形象来源只能是 UPLOAD、PUBLIC 或 AI | 没选人物图 |
| 1001 | 请先上传照片 | 人物图不是直传的地址 |
| 1001 | 该公共数字人已下架，请重新选择 | 公共数字人下架了 |
| 1001 | 请选择一张形象图 | AI定制的 `image_index` 越界 |
| 1001 | 请选择背景 / 该背景已下架，请重新选择 | 预设背景没传 id / 已下架 |
| 1001 | 请上传背景图 / 请先通过上传接口上传 | 导入背景为空 / 不是直传的地址 |
| 1001 | 模型无效 · 视频时长为 15 或 30 秒 · 视频比例无效 | 生成设置取值不对（按 options 传就不会出现） |
| 1001 | 内容包含敏感信息，请修改后重试 | 文案命中敏感词 |
| 1001 | 参数错误：field "X-Request-Id" is not set | 提交没带幂等头（前端漏传，不要展示给用户） |
| 1003 | 记录不存在 / 形象不存在 | 查/删别人的或已删除的记录 / AI定制形象不存在 |
| 1006 | 形象还未生成完成 | AI定制形象还在生成就提交 |
| 1006 | AI 生成服务暂未开放，请稍后再试 | 执行面未配置（环境问题） |
| 1007 | （使用约定） | AI定制形象前未同意使用约定，见 4.3 |
| 1000 | 服务异常 | 提示稍后重试 |

---

## 10. 已确认的产品规则

- **生成完不跳转**：后端只返回数据（提交返回记录、轮询返回状态和视频地址），生成中 / 完成后页面怎么展示由前端决定。
- **文案字数不限**：提交不校验长度，options 的 `max_content_len` 返回 `0`。
- **AI帮写字数不限**：不传 `max_len` 就不约束字数。

## 11. 还没定的

- **计费**：按钮上的「10」点还没接，余额不足的交互（1008）到时再补。
