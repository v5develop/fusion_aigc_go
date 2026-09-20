# AIGC 本地生活 · 后端

Go + go-zero，数据落在现有系统的 `fusion_aigc` 库，新表 `afv_` 前缀，现有表只读、不改结构。
目前有三个模块：**AI文案**、**AI数字人**、**AI图文笔记**。

| 模块 | 设计方案 | 建表脚本 |
|---|---|---|
| AI文案 | `~/Documents/echo/ai-copywriting-design.md` | `deploy/sql/V1.0.106__ai_copywriting.sql` |
| AI数字人 | `~/Documents/echo/digital-human-design.md` | `deploy/sql/V1.0.108__ai_digital_human.sql` |
| AI图文笔记 | `~/Documents/echo/image-text-note-design.md` | `deploy/sql/V1.0.109__ai_image_text_note.sql` |

## 服务

| 服务 | 端口 | 职责 |
|---|---|---|
| `copywriting-rpc` | 9201 | 文案生成与增删改查、选项字典、AI帮写（含数字人配音内容） |
| `digitalhuman-rpc` | 9202 | 使用约定、公共数字人（只读）、AI 定制形象、系统音色、数字人生成；内置结果轮询器 |
| `note-rpc` | 9203 | 图文笔记：构思（执行面 74）→ 规划 → 出图（75）、编辑；内置结果轮询器 |
| `gateway-api` | 8888 | App 接口 `/api/v1/*`，照片上传；本地存储模式下提供 `/files` |
| `admin-api` | 8889 | 运营后台 `/admin/v1/*` |
| `mock-aigc` | 5055 | **仅本地**：模拟 AIGC 执行面（74/75/85），正式地址提供前用它联调 |

模型都不写在配置里，每次调用时从库里解析（地址与密钥在 `afv_ai_provider`）：

| 用途 | 配置项 | 当前模型 |
|---|---|---|
| 文案生成、AI帮写 | `afv_system_config.ai_writing_model_id` | `qwen3-vl-plus` |
| 数字人配音（TTS） | `afv_system_config.ai_tts_model_id` | `qwen-audio-3.1-tts-flash`（`model_type=4` 语音合成） |

## 本地运行

```bash
docker compose -f deploy/docker-compose.yml up -d etcd   # 依赖 etcd；mysql 用现有容器
make db          # 建新表 + 种子数据
make run-all     # 编译并后台启动（含 mock-aigc），日志在 logs/
deploy/scripts/e2e.sh               # AI文案端到端测试（49 项，真实调用模型）
deploy/scripts/e2e-digitalhuman.sh  # AI数字人端到端测试（69 项，执行面走 mock，TTS 真实调用；会临时插入并清理模板素材测试数据）
deploy/scripts/e2e-note.sh          # AI图文笔记端到端测试（48 项，执行面走 mock）
make stop-all
```

开发阶段身份走请求头：App 用 `X-User-Id`，后台用 `X-Admin-Id`；接入现有用户 / Admin 体系后替换中间件即可。
`ai_tts_model_id` 这一行配置 `make db` 不会插，需在各环境手动加（见建表脚本头部注释）。

## AI数字人

形象（上传照片 / 公共数字人 / AI 定制）+ 系统音色 + 配音内容（≤50 字）→ 口播视频，保存为「我的数字人」。

**本版不做「我的音色」**（录音复刻、自定义音色），只做系统音色：28 个百炼
[Qwen-Audio-TTS 精品中文音色](https://help.aliyun.com/zh/model-studio/qwen-audio-tts-voice-list)，
按分类展示（方言、外语暂无对应音色，分类已停用）。App 端只拿我们的 `voice_id`，不接触音色参数值。

生成链路：

| 步骤 | 走哪 | 说明 |
|---|---|---|
| AI 定制形象 | 执行面 **75** | 选项拼 4 条提示词（同一人设、不同服装背景），出 4 张 9:16 图 |
| 开始生成 ① 配音 | **百炼直连**（`mode=0`） | 同步合成 wav → 下载 → 转存（百炼地址是 http 且 24 小时失效） |
| 开始生成 ② 视频 | 执行面 **85** | 形象图 + 配音；固定 9:16 / 720p，时长取配音实际长度（4–30 秒） |

- **轮询器**：`digitalhuman-rpc` 每 5 秒扫一次库里「生成中」的记录，逐个调 `getRes`。状态都在库里，
  服务重启后接着查（已实测）。超时：出图 10 分钟、视频 30 分钟；「配音中」卡住（重启留下的）也会回收。
- **wav 时长**：百炼返回的 wav 头里长度字段是占位值 `0x7FFFFFFF`，按实际字节数计算（`tts.WavDuration`）。
- **存储**：`Storage.Mode=local` 写 `data/files`（仅开发，执行面访问不到）；`oss` 写现有公有桶 `xx-shop-img`，
  配置从 `afv_system_config.storage_public_*` 读，用 OSS V1 签名直接 PUT，不引 SDK。网关与 rpc 的 Storage 配置须一致：
  rpc 只接受本存储签发的上传地址，防止把任意外链交给执行面。
  oss 模式已对真实桶实测（上传 → 公网访问 → 删除）：
  `OSS_REAL=1 OSS_DSN='<dsn>' go test ./pkg/storage -run OSSReal -v`，测试会自行清理。
- **执行面地址**：`digitalhuman-rpc.yaml` 的 `Aigc.Endpoint`，本地指向 mock；置空则出图与生成返回「暂未开放」。
- **公共数字人**：不建表，复用现有模板素材（运营在现有后台「模板」里维护，本服务只读）。
  `afv_public_template_assets` 一行（`title`=名字，`url`=形象图，`extra`=`{"role":"店长","avatarUrl":"可空"}`）
  + `afv_public_template_asset_placement` 一行（`localLife / digitalHuman / publicModel`，排序与上下架），配置项 `PublicModel`。
- 生成类接口需先同意《AIGC功能使用约定》（`afv_user_agreement`，版本号在配置里，改版 +1 需重新同意）。

## AI图文笔记

表单（类型 / 主题 / 素材 ≤3 / 数字人 / 店铺档案 / 套图数量）→ **构思**（执行面 74，出标题、正文、话题和每张图的出图提示词）
→ 规划页（可改标题、正文；套图数量只能改少）→ **出图**（执行面 75）→ 详情、编辑。
与设计稿不同（已和产品确认）：上传素材最多 3 张；套图数量不设上限，以前端为准；编辑图片上限 3 张（生成更多时以生成张数为准）。

- 74 返回的 `script` 整个存下来。确认生成时把 `images`（按 index 取前 N 条，带 index）和 `images_url` **原样**交给 75。
  字段名以真实交互为准，是 `images_url`（对接文档写的是 `image_urls`）。`images_url` 是参考图（数字人形象图 + 上传素材）在 `aigc_temp/` 的副本，不是生成图；
  因为 `reference_materials` 是 `images_url` 从 1 开始的下标。解析逻辑在 `note/.../logic/common.go` 的 `parseScript`，单测用的是真实样例。
- 已知限制：出图提示词里写死了图内文字，规划页改标题不会改图上的字。
- 表单里的数字人 = 我的数字人（须已生成完成），它的形象图作为 74 的 `person_url`；note-rpc 通过 digitalhuman-rpc 校验。
- AI帮写新增场景 `note_topic`（带 `note_type`）。

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
| **计费**：文案、AI 定制形象、数字人生成、笔记出图各 100 点，笔记构思 10 点 → 成功结算 / 失败退回 | 定价与 `action_type` 方案 | `generate_logic.go`（文案）、`flow.go` / `create_logic.go`（数字人） |
| **用户鉴权**：接 `afv_app_user` | token 怎么验 | `gateway/.../auth_middleware.go` |
| **后台鉴权**：接 `afv_admin_*` | 登录态与权限校验方式 | `admin/.../adminauth_middleware.go` |
| **创作任务**：写 `afv_creation_task` | `entry_code` / `feature_code` 命名确认；`mode`：文案与 TTS 为 0，笔记构思 74，出图 75，视频 85 | 文案 `generate_logic.go`；数字人、笔记 `flow.go` |
| **AIGC 执行面正式地址** | 待提供；结果地址是否长期有效（否则需转存）；`aigc_temp/` 参考图保留多久 | `deploy/etc/digitalhuman-rpc.yaml`、`note-rpc.yaml` |
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
