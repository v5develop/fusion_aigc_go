# App 接口文档

网关地址：`http://<host>:8888`（服务器部署默认 18888），所有接口前缀 `/api/v1`。

**在线文档（Swagger，可调试）**：`http://<host>:<端口>/swagger`，先点「Authorize」填 `X-User-Id`。

## 通用约定

**身份**：所有接口都要带 `X-User-Id: <用户ID>`。这是开发模式，接入正式登录后会换成 token，届时只换请求头，接口本身不变。

**响应**：统一包一层，业务数据在 `data` 里。

```json
{"code": 0, "msg": "ok", "data": { ... }}
```

HTTP 状态码一律 200，用 `code` 判断：

| code | 含义 | 前端怎么处理 |
|---|---|---|
| 0 | 成功 | — |
| 1001 | 参数错误 / 校验不通过 | 直接展示 `msg` |
| 1002 | 未登录 | 跳登录 |
| 1003 | 不存在或不属于当前用户 | 返回上一页 |
| 1005 | 状态冲突（如笔记还没构思完就点生成） | 展示 `msg` |
| 1006 | 服务暂未开放（AI 执行面未配置） | 展示 `msg` |
| 1007 | 未同意《AIGC功能使用约定》 | 弹约定弹窗 |
| 1000 | 服务异常 | 提示稍后重试 |

**幂等**：所有**提交生成**的接口必须带 `X-Request-Id`（UUID，每次新生成一个；失败重试时沿用同一个）。
同一用户同一个 `X-Request-Id` 只会生成一次，连点或网络重试都返回同一条记录。缺这个头会返回 1001。

**异步生成**：提交后立刻返回记录（状态=生成中），前端轮询详情接口。建议 2–3 秒一次；图片类 30 秒～2 分钟，视频类 2–10 分钟。

**时间**：都是 unix 毫秒。

**跨域**：网关允许任意来源（含 App 离线包的 `Origin: null`），放行 `X-User-Id`、`X-Request-Id` 预检。

**命名**：请求与响应字段一律下划线，不会改成小驼峰。

## 上传：前端直传 OSS

**推荐：预签名直传**（前端不需要密钥、不用实现签名）

1. `POST /upload/presign`，body `{"kind":"image","content_type":"image/jpeg"}`，返回 `put_url`、`url`、`content_type`、`expires_at`。
2. 把文件原样 PUT 到 `put_url`，**必须带 `Content-Type: <返回的 content_type>`**，HTTP 200 即成功。
3. 把 `url` 传给业务接口（如 `image_url`）。

`kind`：`image` / `video`。`content_type`：`image/jpeg|png|webp|gif|heic`、`video/mp4|quicktime`。对象名由后端生成；地址 30 分钟有效，一个地址传一个文件。
OSS 已允许跨域 PUT。文件大小与类型由前端控制；后端只校验地址是不是本服务签发的。详细见 `api-voiceover.md` 4.1。

**兼容：STS 临时凭证**（前端自行签名上传）

1. `GET /upload/credentials?kind=image|video` 取临时凭证：

```json
{"access_key_id":"STS.…","access_key_secret":"…","security_token":"…",
 "expiration":"2026-09-21T06:12:00Z","bucket":"xx-shop-img",
 "host":"xx-shop-img.oss-cn-hangzhou.aliyuncs.com",
 "dir":"aigc-life/image/123/20260921/","base_url":"https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com"}
```

2. 用凭证把文件 PUT 到 `https://{host}/{dir}{随机文件名}`，**对象 key 必须在 `dir` 下**，否则 OSS 返回 403。
3. 上传成功后，文件地址是 `{base_url}/{key}`，把它传给后续接口。凭证默认 30 分钟有效。

## 一、AI文案

| 接口 | 说明 |
|---|---|
| `GET /copywriting/options` | 选项：内容类型、风格、行业、字数、当前模型、字数上限 |
| `POST /copywriting/ai-write` | AI帮写（同步，几秒返回）。`scene`：`topic` 主题 / `selling_point` 卖点优化 / `dh_script` 数字人配音 / `note_topic` 笔记主题 / `poster_info` 海报信息润色 / `long_image` 长图内容 / `voiceover` 数字人口播文案（字数不限，可传 `max_len` 约束） |
| `POST /copywriting/generate` | 提交生成（`source`：`AI_WRITE` 帮写 / `IMITATE` 仿写），轮询详情 |
| `POST /copywriting/:id/regenerate` | 重新生成，产生新记录 |
| `POST /copywriting` | 自主撰写，直接保存 |
| `GET /copywriting?keyword=&content_type=&page=&page_size=` | 列表（也是各模块「选择AI文案」弹窗的数据源） |
| `GET /copywriting/:id` | 详情（生成中轮询这个） |
| `PUT /copywriting/:id` | 编辑标题正文 |
| `DELETE /copywriting/:id` | 删除 |

文案状态：`0` 生成中、`1` 已完成、`2` 失败。

## 二、AI数字人

| 接口 | 说明 |
|---|---|
| `GET /agreements/aigc` · `POST /agreements/aigc/accept` | 使用约定：是否已同意当前版本 / 同意 |
| `GET /digital-humans/public` | 公共数字人（名字、职业标签、形象图、头像） |
| `GET /digital-humans/options` | AI定制表单：性别、年龄、肤色（带色值）、职业角色、字数上限 |
| `POST /digital-humans/portraits` | AI定制形象，出 4 张 9:16 图，轮询下面这个 |
| `GET /digital-humans/portraits/:id` | 形象生成状态与结果图 |
| `POST /digital-humans/portraits/:id/regenerate` | 重新生成（新记录） |
| `GET /voices/categories` · `GET /voices?category=` | 系统音色分类与列表（名称、风格、试听地址） |
| `POST /digital-humans` | 开始生成：形象 + 音色 + 配音内容（≤50 字） |
| `GET /digital-humans?status=&page=` · `GET /digital-humans/:id` | 我的数字人列表 / 详情 |
| `DELETE /digital-humans/:id` | 删除 |

形象来源三选一，提交时传：
- 上传照片：`portrait_source=UPLOAD` + `image_url`（直传后的地址）
- 公共数字人：`portrait_source=PUBLIC` + `public_id`
- AI定制：`portrait_source=AI` + `portrait_id` + `image_index`（选中第几张，0 起）

数字人状态：`0` 配音中、`1` 视频生成中、`2` 已完成、`3` 失败。生成前必须已同意使用约定，否则返回 1007。
**本版没有「我的音色」**（录音复刻），只有系统音色。

## 三、AI图文笔记

| 接口 | 说明 |
|---|---|
| `GET /notes/options` | 笔记类型（含各类型上传区文案）、素材上限、套图默认值、编辑图片上限 |
| `POST /notes/plan` | 开始构思：类型、主题、素材（≤3）、数字人（选填）、套图数量 |
| `GET /notes/:id` | 详情（构思中、生成中都轮询这个） |
| `POST /notes/:id/replan` | 规划页「重新生成」，可带新的套图数量 |
| `POST /notes/:id/generate` | 确认生成，可带改过的标题、正文、话题、套图数量；生成失败后再调一次即重试 |
| `GET /notes?status=&page=` | 列表 |
| `PUT /notes/:id` | 编辑：标题、正文、话题、图片（保留原图 + 新增直传图） |
| `DELETE /notes/:id` | 删除 |

笔记状态：`0` 构思中、`1` 待确认（进规划页）、`2` 构思失败、`3` 生成中、`4` 已完成、`5` 生成失败。

两点注意：
- 规划页的**套图数量只能改少**。改多要点「重新生成」重新构思，否则返回 1001。
- 图上的排版文字是构思时定的，**改标题不会改图上的字**。

## 四、AI海报 / AI长图 / 动作迁移 / 数字人口播

四者共用一组接口，用 `kind` 区分：`poster`、`longImage`、`actionTransfer`、`voiceover`。

| 接口 | 说明 |
|---|---|
| `GET /image-creations/options?kind=` | 表单配置：海报类型与风格、比例、数量上限、模板是否必选；动作迁移、数字人口播的模型列表；数字人口播的时长 `durations` 与默认值 |
| `GET /image-creations/templates?kind=&poster_type=` | 模板列表（海报可按类型筛）；`kind=voiceover` 返回数字人口播的**预设背景** |
| `GET /action-videos` | 动作视频：`presets` 预置 + `mine` 我导入的 |
| `POST /action-videos` | 导入动作视频（直传后回传 `url`，可带 `cover_url`），自动命名「我的动作N」 |
| `DELETE /action-videos/:id` | 删除我导入的 |
| `POST /image-creations` | 提交生成 |
| `GET /image-creations/:id` | 详情（生成中轮询这个） |
| `GET /image-creations?kind=&status=&page=` | 列表 |
| `DELETE /image-creations/:id` | 删除 |

提交参数按类型：

```jsonc
// 海报：模板必选
{"kind":"poster","content":"海报信息","template_id":12,"poster_type":"festival",
 "poster_style":"flat","size":"3:4","image_count":2,"material_urls":["…"]}

// 长图：模板必选（最新对接文档 76 的 template_url 必填），比例固定 1:4
{"kind":"longImage","content":"长图内容","template_id":15,"material_urls":["…"],"image_count":1}

// 动作迁移：人物图来源同数字人；动作视频二选一；模型必选
{"kind":"actionTransfer","portrait_source":"UPLOAD","image_url":"…",
 "action_video_id":34,            // 或 "my_action_video_id": 7
 "model":"doubao-seedance-2-0-260128"}   // 或 "lite"

// 数字人口播：文案必填（字数不限）；人物图来源同数字人；背景三选一；其余不传用默认
{"kind":"voiceover","content":"欢迎光临我们的小店……",
 "portrait_source":"PUBLIC","public_id":5,           // 或 UPLOAD + image_url / AI + portrait_id + image_index
 "background_source":"PRESET","background_id":21,    // AI（默认，智能匹配）/ PRESET + background_id / UPLOAD + background_url
 "model":"seedance2.5",                              // 默认；或 "lite"（XQ Video 1.0 Lite）
 "duration":15,                                      // 15（默认）/ 30
 "size":"9:16"}                                      // 9:16（默认）/ 16:9
```

数字人口播一步出片（执行面 92），**没有「规划剧本」这一步**。按页面逐步对接的完整说明见 [`api-voiceover.md`](api-voiceover.md)。成品视频在 `image_urls[0]`，详情里另有 `input_bg_url`、`duration`。
视频生成较慢（云端常见 2–10 分钟），轮询间隔建议 5–10 秒；超过 30 分钟未出结果按失败处理。
「AI帮写」用 `POST /copywriting/ai-write`，`scene=voiceover`；「AI文案」用 `GET /copywriting?keyword=` 选一条，把正文填进 `content`。

状态：`0` 生成中、`1` 已完成（**含部分成功**，`image_urls` 只返回成功的那几张）、`2` 全部失败。
一次生成多张时，后端是每张一个任务，所以可能出现部分成功。

## 五、产物地址

生成成功后返回的图片 / 视频地址都在我们自己的公有桶上（`https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/aigc-life/...`），
长期有效，可以直接展示、下载、保存。AI 执行面自己返回的是会过期的临时地址，后端已经统一转存，前端不会拿到那种地址。

## 六、待办（会影响前端的）

- **登录**：现在用 `X-User-Id`，接正式登录后改成 token。
- **扣点数**：各生成动作标的 100 点、笔记构思 10 点，目前**不会真的扣**，也不会因为点数不足而失败。
- **素材库**：生成的成品已经写进现有素材库，App 的素材库页面由现有系统提供。
