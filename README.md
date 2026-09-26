# AIGC 本地生活 · 后端

Go + go-zero，数据落在现有系统的 `fusion_aigc` 库，新表 `afv_` 前缀，现有表只读、不改结构。
目前有四个服务模块：**AI文案（含店铺档案）**、**AI数字人**、**AI图文笔记**、**AI海报/长图/动作迁移/数字人口播**。

| 模块 | 设计方案 | 建表脚本 |
|---|---|---|
| AI文案 | `~/Documents/echo/ai-copywriting-design.md` | `deploy/sql/V1.0.106__ai_copywriting.sql` |
| 店铺档案 | `~/Documents/echo/shop-profile-design.md` | `deploy/sql/V1.0.114__shop_profile.sql`、`V1.0.115__note_shop_profile.sql` |
| AI数字人 | `~/Documents/echo/digital-human-design.md` | `deploy/sql/V1.0.108__ai_digital_human.sql` |
| AI图文笔记 | `~/Documents/echo/image-text-note-design.md` | `deploy/sql/V1.0.109__ai_image_text_note.sql` |
| 探店视频/带货短剧 | `~/Documents/echo/explore-shop-short-drama-design.md` | `deploy/sql/V1.0.116__ai_video_creation.sql` |
| AI海报/长图/动作迁移/数字人口播 | `~/Documents/echo/image-video-creation-design.md` | `deploy/sql/V1.0.111__ai_poster_long_image.sql`、`V1.0.112__ai_action_transfer.sql`、`V1.0.113__ai_voiceover.sql` |

## 服务

| 服务 | 端口 | 职责 |
|---|---|---|
| `copywriting-rpc` | 9201 | 文案生成与增删改查、选项字典、AI帮写（含数字人配音内容）、**店铺档案**（各模块通过 rpc 取拼好的文本） |
| `digitalhuman-rpc` | 9202 | 使用约定、公共数字人（只读）、AI 定制形象、系统音色、数字人生成；内置结果轮询器 |
| `note-rpc` | 9203 | 图文笔记：构思（执行面 74）→ 规划 → 出图（75）、编辑；内置结果轮询器 |
| `imagegen-rpc` | 9204 | 海报（78）、长图（76）、动作迁移（9/86）、数字人口播（92/91）、探店视频（72→73/81）、带货短剧（87→88/89）：模板与动作视频、提交、轮询；内置结果轮询器 |
| `gateway-api` | 8888 | App 接口 `/api/v1/*`，直传凭证；本地存储模式下提供 `/files` 与代传上传 |
| `admin-api` | 8889 | 运营后台 `/admin/v1/*` |
| `mock-union` | 5056 | **仅本地**：模拟联盟用户服务的两个接口，登录联调用 |
| `mock-aigc` | 5055 | **仅本地**：模拟 AIGC 执行面（9/70/71/72/73/74/75/76/78/85/87/88/92），正式地址提供前用它联调 |

模型都不写在配置里，每次调用时从库里解析（地址与密钥在 `afv_ai_provider`）：

| 用途 | 配置项 | 当前模型 |
|---|---|---|
| 文案生成、AI帮写 | `afv_system_config.ai_writing_model_id` | `qwen3-vl-plus` |
| 数字人配音（TTS） | `afv_system_config.ai_tts_model_id` | `qwen-audio-3.1-tts-flash`（`model_type=4` 语音合成） |

## 运维脚本

```bash
# 批量导入运营数据（公共数字人 / 海报模板 / 长图模板 / 预置动作视频），按 url 去重、可重复执行
go run ./tools/seed -dsn '<mysql dsn>' -kind posterTemplate -csv poster.csv -dry
# 批量生成系统音色试听（默认只补没有试听的；--force 全部重来）
ADMIN=http://<host>:8889/admin/v1 deploy/scripts/gen-voice-previews.sh
```

## 文档

- **部署**：一键部署见 `deploy/server/README.md`（`make package` → 服务器 `bash install.sh`）；手工部署细节见 `deploy/README.md`
- **给前端的接口文档**：`docs/api-app.md`（数字人口播单独一份：`docs/api-voiceover.md`）

## 本地运行

```bash
docker compose -f deploy/docker-compose.yml up -d etcd   # 依赖 etcd；mysql 用现有容器
make db          # 建新表 + 种子数据
make run-all     # 编译并后台启动（含 mock-aigc），日志在 logs/
deploy/scripts/e2e.sh               # AI文案端到端测试（49 项，真实调用模型）
deploy/scripts/e2e-digitalhuman.sh  # AI数字人端到端测试（73 项，执行面走 mock，TTS 真实调用；会临时插入并清理模板素材测试数据）
deploy/scripts/e2e-note.sh          # AI图文笔记端到端测试（62 项，执行面走 mock；SKIP_MODEL=1 跳过真实模型）
deploy/scripts/e2e-imagegen.sh      # 海报/长图/动作迁移/数字人口播端到端测试（96 项，执行面走 mock；会临时插入并清理模板素材；SKIP_MODEL=1 跳过 AI帮写）
deploy/scripts/e2e-video.sh         # 探店视频/带货短剧/爆款视频复刻端到端测试（58 项，两步流程走 mock）
deploy/scripts/e2e-login.sh         # App 登录端到端测试（30 项，联盟走本地 mock-union）
deploy/scripts/e2e-login-fusion.sh  # 建档交给现有 Java 服务那条路（9 项，自己起 mock-fusion 和一个临时网关）
deploy/scripts/e2e-login-direct.sh  # 自己验临时 token 那条路（15 项，读本地 Redis + 联盟用户库，真跑）
deploy/scripts/e2e-admin-login.sh   # 运营后台登录端到端测试（20 项，临时插账号跑完删，不碰真实账号）
deploy/scripts/seed-templates.sh    # 造联调用的公共素材（公共数字人/模板/口播背景/动作视频，占位图）
deploy/scripts/e2e-real.sh          # 真实链路回归：对着已部署环境每个模块真实跑一条（会产生费用，见脚本头部）
make stop-all
```

身份：App 用联盟临时 token 换令牌（见 `docs/api-app.md`）。令牌默认发**和 AIGC（Java）服务同一套**的会话
（32 位 hex + 共用 Redis 的 `fusion:token:*`），一个 token 两边都认；没配那台 Redis 时回落到我们自己签的 JWT。
后台用现有账号 `afv_admin_user` 登录（见 `docs/api-admin.md`）。
联调期 `Auth.Mode=dev` 时没带令牌仍认请求头 `X-User-Id` / `X-Admin-Id`，**上线必须改成 `token`**。
`ai_tts_model_id` 这一行配置 `make db` 不会插，需在各环境手动加（见建表脚本头部注释）。

## AI数字人

形象（上传照片 / 公共数字人 / AI 定制）+ 系统音色 + 配音内容（≤50 字）→ 口播视频，保存为「我的数字人」。

**本版不做「我的音色」**（录音复刻、自定义音色），只做系统音色：百炼
[Qwen-Audio-TTS 精品中文音色](https://help.aliyun.com/zh/model-studio/qwen-audio-tts-voice-list)，
按分类展示（方言、外语暂无对应音色，分类已停用）。App 端只拿我们的 `voice_id`，不接触音色参数值。
  入库 28 个，其中 **6 个当前账号合成会报 411，已置为停用**（陆小燕、许如远、易安宁、楚景川、顾小然在官网有列但接口拒绝；
  凌星朗官网已不在列表），可用 22 个。账号开通后把 `status` 改回 1，再跑 `deploy/scripts/gen-voice-previews.sh` 生成试听。

生成链路：

| 步骤 | 走哪 | 说明 |
|---|---|---|
| AI 定制形象 | 执行面 **75** | 选项拼 4 条提示词（同一人设、不同服装背景），出 4 张 9:16 图 |
| 开始生成 ① 配音 | **百炼直连**（`mode=0`） | 同步合成 wav → 下载 → 转存（百炼地址是 http 且 24 小时失效） |
| 开始生成 ② 视频 | 执行面 **85** | 形象图 + 配音；固定 9:16 / 720p，时长取配音实际长度（4–30 秒） |

- **轮询器**：`digitalhuman-rpc` 每 5 秒扫一次库里「生成中」的记录，逐个调 `getRes`。状态都在库里，
  服务重启后接着查（已实测）。超时：出图 10 分钟、视频 30 分钟；「配音中」卡住（重启留下的）也会回收。
- **wav 时长**：百炼返回的 wav 头里长度字段是占位值 `0x7FFFFFFF`，按实际字节数计算（`tts.WavDuration`）。
- **上传：客户端直传 OSS**（与现有系统一致，已和前端确认）。
  `GET /upload/credentials?kind=image|video` 返回 STS 临时凭证，策略限定只能写 `aigc-life/<kind>/<用户>/<日期>/` 下的对象，
  前端直接 PUT 到 OSS，再把地址回传给业务接口。
  **文件大小与类型由前端控制，后端不管**；后端只校验地址属于本存储签发的目录（`storage.Owned`），
  防止把任意外链交给执行面。`POST /upload`（网关代传）保留给本地开发与 e2e，线上不用。
- **产物转存**：AIGC 执行面的产物落在上游**私有桶**（`afv_system_config.storage_private_*`，当前是上海的 `xxlm-pp`），
  返回的是带签名的临时地址、会过期，也不对外可读——现有系统同样要转存（见 `afv_creation_task.is_transferred`）。
  各模块的轮询器拿到结果后先转存到公有桶再落库：同地域走 OSS 服务端复制（CopyObject），
  跨地域（现状：上海 → 杭州）下载再上传；私有桶对象会丢掉过期签名、用我们的密钥重新签。
  转存失败不落库，保持生成中，下一轮重试。已对真实对象实测：
  `OSS_REAL=1 OSS_DSN='<dsn>' go test ./pkg/storage -run TransferReal -v`。
- **存储**：`Storage.Mode=local` 写 `data/files`（仅开发，执行面访问不到，也不支持直传）；
  `oss` 写现有公有桶 `xx-shop-img`，配置从 `afv_system_config.storage_public_*` 读（含 STS 角色），
  OSS V1 签名与 STS AssumeRole 都是自己签，不引 SDK。网关与 rpc 的 Storage 配置须一致。
  已对真实桶实测（代传上传、STS 直传、目录外写入被拒 403、公网访问、删除）：
  `OSS_REAL=1 OSS_DSN='<dsn>' go test ./pkg/storage -run 'OSSReal|STSReal' -v`，测试会自行清理。
- **执行面地址**：`digitalhuman-rpc.yaml` 的 `Aigc.Endpoint`，本地指向 mock；置空则出图与生成返回「暂未开放」。
- **公共数字人**：不建表，复用现有模板素材（运营在现有后台「模板」里维护，本服务只读）。
  `afv_public_template_assets` 一行（`title`=名字，`url`=形象图，`extra`=`{"role":"店长","avatarUrl":"可空"}`）
  + `afv_public_template_asset_placement` 一行（`localLife / digitalHuman / publicModel`，排序与上下架），配置项 `PublicModel`。
- 生成类接口需先同意《AIGC功能使用约定》（`afv_user_agreement`，版本号在配置里，改版 +1 需重新同意）。

## AI海报 / AI长图 / 动作迁移 / 数字人口播

一个服务 `imagegen`、一张表 `afv_image_creation`，用 `kind`（poster / longImage）区分，流程相同：
填内容（≤500 字）→ 选模板 → 生成设置 → 提交 → 轮询 → 存素材库。

- **一张图一个执行面任务**：生成 N 张就提交 N 个任务，结果逐个回填；全部失败记失败，部分成功按完成处理、只返回成功的那几张。
- **模板复用现有模板素材表**：投放位 `localLife / poster|longImage / template`，海报按筛选维度 `posterType` 打标签
  （社媒/营销/节日/喜庆/邀请函/招聘），运营在现有后台维护。模板地址下单时快照，下架不影响已生成的记录。
- 海报模板必选、可选风格与比例、模型本版固定 `max`；长图模板也必选（最新对接文档 76 的 `template_url` 必填）、比例固定 `1:4`、不传 model（76 文档里没有这个参数）。
- 上传素材两边都最多 3 张（设计稿长图写的是 10 张，与产品确认后改为 3）。
- AI帮写新增场景 `poster_info`（海报信息润色）、`long_image`（长图内容）。
- **动作迁移**：人物图（上传 / 公共数字人 / AI定制形象，经 `digitalhuman-rpc` 的 `ResolvePortrait` 解析）
  + 动作视频（预置来自模板素材表 `localLife/actionTransfer/actionVideo`，或用户导入存 `afv_action_video`）
  → 执行面 9；只有选 lite 留在本地 9，其余都路由到 86。成品视频进素材库，封面用人物图。
- **数字人口播**：口播文案（字数不限）+ 人物图（同动作迁移）+ 背景（AI智能匹配不传 / 预设背景来自模板素材表
  `localLife/voiceover/background` / 用户导入）→ 执行面 **92** 一步出片（lite 路由到本地 91），时长 15/30 秒、比例 9:16/16:9、
  分辨率固定 720p。剧本由执行面内部生成，设计稿已去掉「规划剧本」页。超时 40 分钟（实测本地 GPU 出 15 秒视频约 14 分钟）。成品进素材库「数字人口播」分类，
  封面用人物图。AI帮写新增场景 `voiceover`（字数不限）。生成完不跳转，后端只返回数据，由前端处理。

## 探店视频 / 带货短剧 / 爆款视频复刻

同在 `imagegen` 服务，另一张表 `afv_video_creation`，用 `kind`（exploreShop / shortDrama）区分。和其他模块不同，**这两个是两步**：

```
填表单 → 出剧本（72 / 87）→ 待确认（规划剧本页，只读，可「重新生成」）
      → 确认出片（73 / 88；缺省 model 走云端 81 / 89）→ 转存 → 素材库
```

- 状态：0 构思中 / 1 待确认 / 2 生成中 / 3 已完成 / 4 失败。剧本原文存 `plan_script`，出片时**原样回传**给执行面。
- 探店：文案必填，门店图/店内图/道具图对应 `ENV_01`/`ENV_02`/`PROP_01`，出镜达人复用数字人的三种来源。
- 短剧：主题必填、**商品图必填**（87 的要求，设计稿同步改成必填），另有商品卖点、环境图、出镜人物。
- 两者都能关联店铺档案，构思时通过 rpc 取拼好的文本塞进 `shop_profile`。
- 超时：构思 10 分钟、出片 40 分钟。AI帮写新增场景 `explore_shop`、`short_drama`。
- **爆款视频复刻**（`videoRemake`，70 解析 → 71 复刻）：两种来源——上传本地视频，或粘贴抖音/快手/小红书/bilibili 分享链接。
  源视频限制 50M、3 分钟（设计稿文案），提交前用 HEAD / Range 探体积与格式，时长等 70 返回再判。
  **链接解析（分享链接 → 可下载地址）是同事的接口，还没提供**；`VideoRemake.LinkResolver` 留空时这个入口不开放，
  代码里的 TODO（`create_video_logic.go`）写了接口到位后要补的三步：解析 → 探体积格式 → 边下边数并转存。
  第一步的产物是剧情文本（不是分镜剧本），成品归到素材库现有的「视频复刻」分类。

## AI图文笔记

表单（类型 / 主题 / 素材 ≤3 / 数字人 / 店铺档案 / 套图数量）→ **构思**（执行面 74，出标题、正文、话题和每张图的出图提示词）
→ 规划页（可改标题、正文；套图数量只能改少）→ **出图**（执行面 75）→ 详情、编辑。
与设计稿不同（已和产品确认）：上传素材最多 3 张；套图数量不设上限，以前端为准；编辑图片上限 3 张（生成更多时以生成张数为准）。

- 74 返回的 `script` 整个存下来。确认生成时把 `images`（按 index 取前 N 条，带 index）和 `images_url` **原样**交给 75。
  75 的参考图字段按最新对接文档是 `image_urls`（内容来自 74 返回的 `images_url`）。它们是参考图（数字人形象图 + 上传素材）在执行面临时目录 `aigc_temp/` 的副本，不是生成图，
  **构思完成时先转存到我们的目录**再存 script（临时目录会被清理）；
  因为 `reference_materials` 是 `images_url` 从 1 开始的下标。解析逻辑在 `note/.../logic/common.go` 的 `parseScript`，单测用的是真实样例。
- 已知限制：出图提示词里写死了图内文字，规划页改标题不会改图上的字。
- 表单里的数字人 = 我的数字人（须已生成完成），它的形象图作为 74 的 `person_url`；note-rpc 通过 digitalhuman-rpc 校验。
- AI帮写新增场景 `note_topic`（带 `note_type`）。

## 店铺档案

一个账号最多 20 条（`afv_shop_profile`），7 个字段：店铺名称（必填）、开店时间、地址、提供商品/服务、核心卖点、如何称呼自己、主要目标客群。
字数上限按设计稿便签，**汉字算 2 个字符**（`charLen`）。放在 `copywriting` 服务里：AI文案直接用，其他模块通过 rpc `ProfileText` 取拼好的文本，
塞进执行面的 `shop_profile` 参数。开店时间存文本（前端日期选择器选中后回传），拼提示词时若能解析成日期会顺带算出「已开店 N 年 M 个月」。
删除是逻辑删除，已生成的历史内容不受影响（提示词在生成时就拼好了）。

目前已接入：AI文案（AI帮写/爆款仿写）、图文笔记（74）。探店视频、带货短剧做的时候一起带上。

## 登录（联盟 token → 本服务令牌）

App 带着联盟（xxlm）的 token 调 `POST /api/v1/auth/login`，网关做三件事：

1. 调联盟 `GET /api/v1/user/auth/token` 换临时 token（对方 Redis 存 5 分钟、**用一次就失效**）；
2. 调联盟 `GET /api/v1/auth/{临时token}/user` 拿用户信息（手机号是脱敏的）；
3. 按联盟用户 id 查 `afv_app_user`，有就登录、没有就建档，然后签发我们自己的令牌（HS256 JWT，默认 7 天）。

建档沿用现有数据的约定：`username=union_<联盟用户ID>`、`login_source=UNION`、`data_source=2`、密码是随机串的 BCrypt，
这样同一个人在现有系统和本服务里是**同一行**。每次登录会把昵称、头像、手机号同步过来（联盟是权威源）。

- 中间件优先认 `Authorization: Bearer <我们的令牌>`；`Auth.Mode=dev` 时没带令牌才退回 `X-User-Id`，**线上设成 `token`**。
- 令牌密钥 `Auth.Jwt.Secret` 由部署脚本按环境生成写进配置（`deploy.env` 的 `JWT_SECRET`，留空自动生成）。
- 不做 refresh token：联盟 token 在 App 手里，过期后再登录一次即可。
- 本地联调有 `tools/mock-union`（:5056）模拟联盟这两个接口，`make run-all` 会启动。

## AI文案

做：三条创作路径（AI帮写 / 爆款仿写 / 自主撰写）、AI帮写与AI优化、重新生成、列表搜索、详情编辑删除、后台选项维护、
提交幂等、内容审核（关键词）、提示词模板落库可配。AI帮写新增场景 `dh_script`（数字人配音内容，默认 50 字以内）。

**不做：文案详情页的「已生成的内容」和「生成历史」**。相关代码已写好但本版不交付，
接口 `GET /copywriting/:id/derivatives` 前端暂不接入，源码中以 `TODO 本版不交付` 标注：
`copywriting.proto`、`derivatives_logic.go`（rpc 与网关各一处）、`common.go` 的 `assetTypesOf`、
`shared_model.go` 的 `ListDerivatives`。启用前需确认各标签对应的 `afv_asset.type`，
以及下游模块是否把 `afv_creation_task.external_ref` 写成文案 id。

## 自带机制（两个模块共用）

- **幂等**：生成类接口必须带 `X-Request-Id`，唯一键 `uk_user_request(user_id, request_id)`；
  重复提交返回同一条，连点不会重复扣费。
- **内容审核**：入参与模型出参都过 `pkg/moderation`。当前为关键词过滤，词库在
  `afv_copywriting_option` 的 `BLOCKWORD` 类型（两个模块共用），运营可维护；`Moderation.Mode: off` 可关闭。
  现有系统的文本审核走网易易盾，凭证不在库里，接入后替换 `moderation.Checker` 实现即可。
- **提示词**：模板在各自选项表的 `PROMPT` 类型里，运营可改；读不到时回落到代码内置文本。
- **卡单回收**：文案由 `StuckRecovery`、数字人由 `Poller` 负责，前端不会一直轮询。
- **并发控制**：按 `afv_ai_model.max_concurrency` 限制同时打给模型的请求数（文案与 TTS 各自计数）。
- **网络重试**：调百炼（文案、TTS）遇到连接/握手这类网络抖动自动重试一次。
- **素材库**：生成成功后写进现有 `afv_asset`（开关 `Asset.Enabled`）。笔记每张图一行、用 `source_task_id` 成组（同现有套图）；
  数字人定制形象是一行主资产 + 4 条 `afv_asset_item`；口播视频一行。用户删除或编辑时，素材库同步删除 / 重写。
  聚合分类与入口归属见 `deploy/sql/V1.0.110__asset_category_localLife.sql`（只插数据，可重复执行）。

## 待办（代码里都有 TODO，`grep -rn TODO app pkg`）

| 事项 | 卡在哪 | 代码位置 |
|---|---|---|
| **计费**：文案、AI 定制形象、数字人生成、笔记出图、海报/长图各 100 点，笔记构思 10 点 → 成功结算 / 失败退回 | 定价与 `action_type` 方案 | `generate_logic.go`（文案）、`flow.go` / `create_logic.go`（数字人） |
| **用户鉴权**：上线前把 `Auth.Mode` 改成 `token`（关掉 X-User-Id 兜底） | 联调期先留着 dev | `gateway-api.yaml` |
| **后台鉴权**：接 `afv_admin_*` | 登录态与权限校验方式 | `admin/.../adminauth_middleware.go` |
| **创作任务**：写 `afv_creation_task` | `entry_code` / `feature_code` 命名确认；`mode`：文案与 TTS 为 0，笔记构思 74，出图 75，长图 76，海报 78，视频 85，动作迁移 9，数字人口播 92 | 各模块的 `flow.go` |
| **AIGC 执行面正式地址** | 没有正式地址，本期只对 mock 联调 | 各服务 yaml 的 `Aigc.Endpoint` |
| **链接解析接口**：爆款视频复刻的「链接查视频」 | 同事的接口还没给；到位后填 `VideoRemake.LinkResolver`，并补 create_video_logic.go 里 TODO 写的三步 | `imagegen-rpc.yaml`、`create_video_logic.go` |
| **6 个系统音色合成报 411** | 阿里云账号是否开通（已先停用，可用 22 个） | `afv_digital_human_voice.status` |
| **店铺档案**：店铺信息拼进文案提示词 | 该模块接口 | `generate_logic.go` |
| **审核接易盾** + 写 `afv_moderation_log` | 易盾凭证；provider 取值与 task_id 约定 | `pkg/moderation` |
| **生成历史 / 已生成的内容**（文案） | 本版不交付 | 见上节 |

这些关着不影响功能跑通，只是**不扣点数**、任务和成本不进现有统计、鉴权仍是开发模式。

## 开发

```bash
make gen-rpc     # 改了 app/*/rpc/*.proto
make gen-api     # 改了 gateway.api / admin.api
make gen-model   # 改了建表 SQL（只重生成 *_gen.go）
make test
```
