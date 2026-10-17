# App 接口文档

网关地址：`http://<host>:8888`（服务器部署默认 18888），所有接口前缀 `/api/v1/local-life`。

**在线文档（Swagger，可调试）**：`http://<host>:<端口>/swagger`，先点「Authorize」填登录接口返回的 token（或联调用的 `X-User-Id`）。

## 通用约定

**身份**：登录后所有接口都带 `Authorization: Bearer <token>`（token 由下面的登录接口签发）。

联调环境（`Auth.Mode=dev`）没带 token 时仍然认 `X-User-Id: <用户ID>`，方便前端先跑通业务；**线上会关掉**。

### 登录

一共两步，第一步前端自己做：

1. **前端 → 联盟**：`GET {联盟}/api/v1/user/auth/token`，带用户的联盟 token（`Authorization: Bearer <联盟token>`），拿到 `tempToken`。
   它 5 分钟有效、**用一次就失效**，所以每次登录都要重新换一个。
2. **前端 → 我们**：`POST /auth/login`，body `{"temp_token":"<上一步的 tempToken>"}`，换我们的令牌。
   （也可以改放在 `Authorization: Bearer <tempToken>` 请求头里，两种都收。）

| 接口 | 说明 |
|---|---|
| `POST /auth/login` | 临时 token → 我们的令牌；库里没有的用户会自动建档 |
| `GET /auth/me` | 当前登录用户（带我们的令牌） |

```json
// POST /auth/login 返回
{"code":0,"msg":"ok","data":{
  "token":"9f3c1e7a4b8d42f0a1c6e5b7d9084321",   // 之后所有接口带 Authorization: Bearer <token>
  "expires_at":1790755825451,                    // 过期时间（unix 毫秒）
  "refresh_token":"5b2d7f1c8e3a49d6b0f4c2a1e7d93856",
  "refresh_expires_at":1791360625451,
  "user":{"id":2,"nickname":"小林","avatar":"https://…","phone":"130****7881"}
}}
```

**这个 token 和 AIGC 服务是同一套** —— 我们和它共用用户表与登录会话，所以拿它可以**直接调 AIGC 那边的接口**，不用再登录一次：

> 下面这些是现有 Java 服务的接口，路径是 `/api/v1/...`（没有 `local-life`）；我们网关的接口都在 `/api/v1/local-life/...` 下。两边同一个 `/api/v1` 根，只是我们多一段 `local-life`，转发时按 `/api/v1/local-life` 前缀分给我们的 18888。

| 能直接调的 | 用途 |
|---|---|
| `GET /api/v1/points/balance`、`/points/logs` | 点数余额、流水 |
| `GET /api/v1/features/biz-points` | 各业务的点数价目 |
| `GET /api/v1/asset/all`、`/asset/categories` | 素材库（按任务成组、分类 tab、比例筛选） |
| `GET /api/v1/auth/user-info` | 当前用户（含可用点数与创作能力） |
| `POST /api/v1/auth/refresh` | **刷新令牌**（用 `refresh_token`，我们这边不用再实现） |

> `refresh_token` / `refresh_expires_at` 只在共用会话时返回。如果服务端没配（回落到我们自己的 JWT），这两个字段为空，`token` 是 JWT 形状，也不能拿去调 AIGC 的接口。看 `token` 是不是 32 位 hex 就能区分。

- 我们拿这个临时 token 去联盟换用户信息（`GET {联盟}/api/v1/auth/{tempToken}/user`），**库里没有的用户会自动建档**（同时建个人团队，口径和现有 Java 服务一致），有的就直接登录并同步资料（联盟返回空的字段不会覆盖已有资料）。
- `user.id` 是本服务的用户 id，业务数据都按它隔离。手机号是**脱敏**的，联盟那边就不给真实号码。
- 令牌有效期：共用会话时 **access 2 小时 / refresh 7 天**（过期先用 `refresh_token` 调 AIGC 的 `/auth/refresh`，刷不动了再重走上面两步）；回落 JWT 时是 **7 天、没有 refresh**。
  两种情况下过期都返回 `1002`「登录已过期，请重新登录」。

| 登录失败 | 返回 |
|---|---|
| 临时 token 失效/用过了/乱写的 | `1002`「登录已失效，请重新登录」 |
| 账号被冻结 | `1004`「账号已被禁用」 |
| 联盟服务不可用 | `1000`，提示稍后重试 |

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
| 1004 | 账号已被禁用 | 提示并退出登录 |
| 1008 | 点数不足（本版不会出现，计费未接） | 提示充值 |
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
 "dir":"aigc-local-life/image/123/20260921/","base_url":"https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com"}
```

2. 用凭证把文件 PUT 到 `https://{host}/{dir}{随机文件名}`，**对象 key 必须在 `dir` 下**，否则 OSS 返回 403。
3. 上传成功后，文件地址是 `{base_url}/{key}`，把它传给后续接口。凭证默认 30 分钟有效。

## 零、店铺档案

内容生产的数据底座：一个账号最多 **20 条**，AI文案、图文笔记、探店视频、带货短剧的表单里都能「关联店铺档案（选填）」，
选中后后端把档案内容拼成一段文本发给模型，前端只传 `shop_profile_id`。

| 接口 | 说明 |
|---|---|
| `GET /shop-profiles?page=&page_size=` | 列表（卡片用 `name` + `selling_points` 摘要） |
| `POST /shop-profiles` | 创建 |
| `GET /shop-profiles/:id` | 详情（编辑页回填） |
| `PUT /shop-profiles/:id` | 保存 |
| `DELETE /shop-profiles/:id` | 删除（已生成的历史内容不受影响） |

字段与上限（**汉字算 2 个字符**，与设计稿一致）：

| 字段 | 必填 | 上限 | 说明 |
|---|---|---|---|
| `name` | ✅ | 20 | 店铺名称（10 个汉字） |
| `open_date` | | 10 | 开店时间，日期选择器选中后按文本传，如 `2025-10-03` |
| `address` | | 60 | 店铺地址 |
| `products` | | 180 | 提供商品/服务 |
| `selling_points` | | 220 | 核心卖点，3~5 条 |
| `self_title` | | 15 | 如何称呼自己，如「老板娘小林」 |
| `target_audience` | | 40 | 主要目标客群 |

超限返回 1001，`msg` 会写明哪个字段、上限多少。满 20 条再创建也返回 1001。

## 一、AI文案

| 接口 | 说明 |
|---|---|
| `GET /copywriting/options` | 选项：内容类型、风格、行业、字数、当前模型、字数上限 |
| `POST /copywriting/ai-write` | AI帮写（同步，几秒返回）。`scene`：`topic` 主题 / `selling_point` 卖点优化 / `dh_script` 数字人配音 / `note_topic` 笔记主题 / `poster_info` 海报信息润色 / `long_image` 长图内容 / `voiceover` 数字人口播文案 / `explore_shop` 探店视频文案 / `short_drama` 带货短剧主题（都不限字数，可传 `max_len` 约束） |
| `POST /copywriting/generate` | 提交生成（`source`：`AI_WRITE` 帮写 / `IMITATE` 仿写），轮询详情 |
| `POST /copywriting/:id/regenerate` | 重新生成，产生新记录 |
| `POST /copywriting` | 自主撰写，直接保存 |
| `GET /copywriting?keyword=&content_type=&status=&page=&page_size=` | 列表（也是各模块「选择AI文案」弹窗的数据源）。`status`：`0` 生成中 / `1` 已完成 / `2` 失败，**不传 = 全部**；弹窗传 `status=1` 只取成功的 |
| `GET /copywriting/:id` | 详情（生成中轮询这个） |
| `PUT /copywriting/:id` | 编辑标题正文 |
| `DELETE /copywriting/:id` | 删除 |

**生成历史**（`GET /copywriting/:id/derivatives?kind=all`）：这条文案被拿去生成过哪些成品，**只返回已完成的**。

- 列表项：`id`（**业务记录 id**，直接打开对应详情接口）、`kind`、`size`（比例，如 `2:3`；笔记与数字人为空）、`cover_url`、`result_url`、`is_video`、`create_time`。
- `kind` 七种：`note` 笔记 / `poster` 海报 / `long_image` 长图 / `voiceover` 口播 / `digital_human` 数字人视频 / `explore_video` 探店 / `promo_drama` 短剧。
- ⚠️ **要有数据，提交时必须带 `copywriting_id`**：`POST /notes/plan`、`POST /image-creations`、`POST /video-creations`、`POST /digital-humans` 都加了这个**选填**字段，从「选择AI文案」进来的就带上那条文案的 id。不带不报错，只是不会出现在生成历史里。


文案状态：`0` 生成中、`1` 已完成、`2` 失败。

## 二、AI数字人

| 接口 | 说明 |
|---|---|
| `GET /agreements/aigc` · `POST /agreements/aigc/accept` | 使用约定：是否已同意当前版本 / 同意 |
| `GET /digital-humans/public` | 公共数字人（名字、职业标签、形象图、头像） |
| `GET /digital-humans/options` | AI定制表单：性别、年龄、肤色（带色值）、职业角色、字数上限、**出片模型 `models`** |
| `POST /digital-humans/portraits` | AI定制形象，出 4 张 9:16 图，轮询下面这个 |
| `GET /digital-humans/portraits/:id` | 形象生成状态与结果图 |
| `POST /digital-humans/portraits/:id/regenerate` | 重新生成（新记录） |
| `GET /voices/categories` · `GET /voices?category=` | 系统音色分类与列表（名称、风格、试听地址） |
| `POST /digital-humans` | 开始生成：形象 + 音色 + 配音内容（≤50 字）+ 选填 `model` |
| `GET /digital-humans?status=&page=` · `GET /digital-humans/:id` | 我的数字人列表 / 详情 |
| `DELETE /digital-humans/:id` | 删除 |

形象来源三选一，提交时传：
- 上传照片：`portrait_source=UPLOAD` + `image_url`（直传后的地址）
- 公共数字人：`portrait_source=PUBLIC` + `public_id`
- AI定制：`portrait_source=AI` + `portrait_id` + `image_index`（选中第几张，0 起）

**出片模型**（2026-09-27 新增）：`options` 的 `models` 下发两档，**第一项是默认，前端不要写死**。

| `code` | 名称 | 走哪 | 说明 |
|---|---|---|---|
| `seedance2.5` | seedance 2.5 | 云端 85 | 默认档，配音最长 30 秒 |
| `lite` | XQ Video 1.0 Lite | 本地 45 | 便宜一档，**配音最长 25 秒** |

提交时 `model` 选填，不传按默认档。传了不认识的值直接报 1001 —— 不会默默降级成云端，
免得用户以为选了便宜的却按贵的扣。列表和详情都回 `model`，历史记录能看出当时用的哪档。

⚠️ **lite 的 25 秒是硬限制**：45 超过会自己把音频截断，出来的视频半句话没了。
所以我们在配音合成完就量时长，超了直接失败并提示精简，不提交。
50 字的配音一般 15 秒上下，正常写不会碰到。

数字人状态：`0` 配音中、`1` 视频生成中、`2` 已完成、`3` 失败。生成前必须已同意使用约定，否则返回 1007。
**本版没有「我的音色」**（录音复刻），只有系统音色。

## 三、AI图文笔记

| 接口 | 说明 |
|---|---|
| `GET /notes/options` | 笔记类型（含各类型上传区文案）、素材上限、套图默认值、编辑图片上限 |
| `POST /notes/plan` | 开始构思：类型、主题、素材（≤3）、数字人（选填）、店铺档案 `shop_profile_id`（选填）、套图数量 |
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

## 五、探店视频 / 带货短剧 / 爆款视频复刻

三者共用一组接口，用 `kind` 区分：`exploreShop`、`shortDrama`、`videoRemake`。和其他模块不同，**这三个都是两步**：
先出中间产物（分镜剧本 / 剧情分析）给用户确认，再出成片。

| 接口 | 说明 |
|---|---|
| `GET /video-creations/options?kind=` | 表单配置：模型、时长（15/30）、比例、字数上限、商品图是否必填 |
| `POST /video-creations` | 提交表单，开始构思剧本（探店 72 / 短剧 87）或解析原片（复刻 70） |
| `GET /video-creations/:id` | 详情（构思中、生成中都轮询这个） |
| `POST /video-creations/:id/replan` | 规划剧本页的「重新生成」（复刻是重新解析） |
| `POST /video-creations/:id/generate` | 确认剧本/剧情，开始生成视频（探店 73 / 短剧 88 / 复刻 71） |
| `GET /video-creations?kind=&status=&page=` | 列表 |
| `DELETE /video-creations/:id` | 删除 |

提交参数：

```jsonc
// 探店视频：文案必填；门店图/店内图/道具图、出镜达人、店铺档案都选填
{"kind":"exploreShop","user_text":"西湖边新开的临湖咖啡店，主打手冲",
 "portrait_source":"PUBLIC","public_id":5,        // 出镜达人：同数字人的三种来源，可不传
 "env1_url":"…",                                   // 门店图
 "env2_url":"…",                                   // 店内图
 "prop_url":"…",                                   // 道具图
 "shop_profile_id":3,                              // 关联店铺档案，选填
 "duration":15,"size":"9:16","model":"seedance2.5"}

// 带货短剧：主题必填、商品图必填；卖点、环境图、出镜人物、店铺档案选填
{"kind":"shortDrama","user_text":"上班族早餐反转短剧",
 "selling_points":"便携榨汁杯、30秒出汁、USB充电",
 "prop_url":"…",                                   // 商品图，必填
 "env1_url":"…",                                   // 环境图
 "portrait_source":"AI","portrait_id":12,"image_index":0,
 "duration":30,"size":"9:16"}

// 爆款视频复刻：两种来源二选一
{"kind":"videoRemake","source":"upload","video_url":"…"}          // 上传本地视频（直传后的地址）
{"kind":"videoRemake","source":"link","link":"https://v.douyin.com/…"}  // 粘贴分享链接（见下）
```

**爆款视频复刻**的几点不同：

- 表单配置里另有 `max_video_bytes`（50M）、`max_video_seconds`（180）、`video_formats`（mp4/avi/mkv/mov/flv/wmv）、
  `link_enabled`。**`link_enabled=false` 时「链接查视频」不要放出来**，只显示「上传视频」。
- 提交前后端会探一下体积和格式，超限直接返回 1001，文案就是设计稿那句「仅支持 3 分钟、50M 以内的视频，请重新上传」。
  时长在提交时拿不到，等解析完成（70 返回原片时长）才知道，超过 3 分钟会把这条记录标成失败、给同样的提示。
- 状态 `1`（待确认）时，`script` 是**剧情分析文本**——**纯文本，不用 parse**（探店/短剧的 `script` 才是 JSON 对象）。
  内容通常是 Markdown（`## 一、整体信息` / `## 三、分镜逐镜描述` …），但由模型生成、结构不保证，整段渲染即可。
  `duration` 是原片时长。用户点「复刻视频」即调 `generate`。
- **`generate` 可以带上用户改过的设置**（都选填）：
  `{"prompt":"改过的剧情","model":"lite","size":"16:9"}`。不传就用解析结果和默认值；`size` 不传让成片跟原片走。
  可选的 `model` / `size` 在 `options?kind=videoRemake` 里下发。
- 详情里另有 `source`、`source_url`（归一化后的原链接）、`source_video_url`（待解析的视频地址）、`title`、`cover_url`。
  **`title` / `cover_url` 只有链接入口才有值**（来自解析服务，我们会转存，不会过期）；上传入口是空的，请用文件名兜底。
  ⚠️ 时序：**`title` 提交就有**；`cover_url` 和 `source_video_url` 要等后台把原片转存完（几秒到几十秒，按视频大小），轮询详情时会补上。

**链接查视频**（`source=link`）：支持抖音 / 快手 / 小红书 / B站，**整段分享文案直接粘贴**即可（带口令、表情、话题标签都能认），不用让用户抠链接。
后端分两段：**提交时同步解析**（几秒，拿到标题/直链），**下载转存放后台**（原片最大 50M，同步做会把提交接口拖超时）。

提交时就能拒掉的（返回 `1001`，直接展示 `msg`）：

| 情况 | msg |
|---|---|
| 平台不支持 / 文案里没链接 | 这个链接解析不了，目前支持抖音、快手、小红书、B站的分享链接 |
| 图集作品（没有视频） | 这个作品里没有视频，换一个再试 |
| 解析就拿到时长且超 3 分钟 | 仅支持 3 分钟、50M 以内的视频，请重新上传 |

**后台才暴露的**（提交先成功，随后记录变 `status=4`，看 `fail_reason`）：超过 50M、原片下载不动、转存失败，以及解析时拿不到时长的（比如 B站）等执行面回报时长后才判的超时长。所以结果页照常轮询即可。

**状态**：`0` 构思中 → `1` 待确认（剧本已出）→ `2` 生成中 → `3` 已完成 → `4` 失败。

- 状态 `1` 时 `script` 是剧本 JSON 原文，前端解析后展示分幕：`scenes[]` 每项有 `scene_name`、`start_sec`/`end_sec`、
  `camera`（镜头）、`dialogue`（台词）、`image_ref`（用到哪张图，如 `ENV_01`、`PROP_01+CHAR_01`）。
  **剧本只读**，不满意就调 `replan` 重新生成。
- 失败（`4`）后可以再调 `replan`（重新构思）或 `generate`（重新出片）。
- `model`：`seedance2.5`（默认，云端）或 `lite`（执行面本地 GPU，慢很多）。出视频耗时见第四节的说明，超时 60 分钟。
- 成品在 `video_url`，封面用出镜人物图（没传人物图就用成片地址）。

## 六、产物地址

生成成功后返回的图片 / 视频地址都在我们自己的公有桶上（`https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/aigc-local-life/...`），
长期有效，可以直接展示、下载、保存。AI 执行面自己返回的是会过期的临时地址，后端已经统一转存，前端不会拿到那种地址。

## 七、待办（会影响前端的）

- **登录**：现在用 `X-User-Id`，接正式登录后改成 token。
- **扣点数**：各生成动作标的 100 点、笔记构思 10 点，目前**不会真的扣**，也不会因为点数不足而失败。
- **素材库**：生成的成品已经写进现有素材库，App 的素材库页面由现有系统提供。
