# ai-gateway 能力审计（2026-10-31，分支 feat/atlas-optimization HEAD 28dbcc0）

seed 只能导入空库，线上以 `GET /v1/catalog/model-groups` 实际返回为准，上线前用 `POST /v1/catalog/readiness` 核对（`items[{model_group, operation?}]`，建议 edit 单独传 operation；返回含 enabled=false 的组需过滤）。

## 图片模型组
| code | ops | 输入图上限 | 参数 |
|---|---|---|---|
| img.gpt-image-2 | edit/generate | 16 | aspect_ratio 13 种；resolution 1K/2K/4K；size WxH 互斥；quality auto/low/medium/high（Atlas 部署缺省 low） |
| img.nano-banana-2.1 | edit/generate | 14 | aspect_ratio 14 种；resolution 1K/2K/4K；拒收 size；quality 不下发 |
| img.seedream-5-pro | edit/generate | 10 | **1K 下非 1:1 比例全部被拒；4K 静默降 2K**；quality 不下发 |
| img.qwen-image-3-pro | edit/generate | 3 | 不要作「通用」 |
「通用」= Go 配置决定，建议 img.gpt-image-2。n 实际都是 1。options_schema 为 {}，model_options 原样透传。input 多写字段 422（extra=forbid）。

## chat.completion 多模态
只有 `vlm.qwen-3.8-flash`（推荐）/ `vlm.qwen-3.8-max` 带视觉；`llm.*`（含 deepseek-v4-flash）传图 → route.no_capable_deployment。图片块 `{"type":"image_url","uri":"mr://…|https://…|data:image/…;base64,…","detail"?}`，字段名 **uri**。结果 result.message.content。

## video.generate
prompt(seedance 5–5000)、duration_s（2 系 4–15，2.5 4–30）、resolution 小写 `^[0-9]{3,4}p$`、aspect_ratio、fps(seedance 只 24)、first_frame/last_frame(MediaRef)、reference_images/reference_videos/reference_audios（复数）、generate_audio（缺省 true）。**vid.seedance-2.5 不支持首尾帧**（只有 seedance-2/fast/mini 的百度/孔明部署支持）；首尾帧不能与 reference_* 同用。输出 result.video、cover、last_frame。

## CallRequest
image.edit：images[]（首张主图）、mask?、n、size | aspect_ratio+resolution、quality、output_format；image.generate 用 reference_images[]。MediaRef {uri mr://|https://, sha256?, mime?}，建议带 sha256。https 必须在 input_source_hosts 白名单（管理端 PUT /admin/v1/settings/input-source-hosts，小写裸主机名，需「校验→发布」；草稿未设时回落 AIG_INPUT_SOURCE_HOSTS）且带有效签名、TTL 覆盖调用期；**优先 mr://**。media_purpose intermediate|asset_library。limits.max_cost **CNY 十进制字符串**，软上限（只在新尝试前检查）。labels ≤16 对，值 ≤256。constraints.deadline_at/allowed_deployments/max_attempts/min_outputs；缺省超时 900–1800s（视频 3600s）。cost 看 call.cost.amount_cny。

## Go 侧需修（已派给 Poller/画布两路）
1 Seedream 缺省 2k、4k 降 2k；2 显式传 aspect_ratio；3 ImageEditInput 加 quality（GPT 商拍 high/medium）；4 润色有图用 vlm.qwen-3.8-flash；5 带 deadline_at + max_cost，超时主动 Cancel；6 参考图上限读 catalog feature_limits.max_reference_images；7 参考图优先 mr://+sha256；8 Readiness 支持 operation、补 reason（operation.mismatch / deployments.all_balance_excluded / runtime.unavailable）；9 labels 值 ≤256 校验；10 视频字段 reference_videos/generate_audio/duration_s，首尾帧不用 2.5；11 过滤 enabled=false。

## 2026-10-31 DeepSeek 渠道

ai-gateway 新增 DeepSeek 官方渠道（未上线前已按统一命名原地改名，上一轮的 provider / account / 部署 / 视觉组 / 价格模型旧名全部删除、无兼容别名）。命名沿用 gateway 现有规则：文本部署 `<provider>.<model>` 归 `llm.<model>`，视觉部署 `<provider>.vl.<model>` 归 `vlm.<model>`。DeepSeek Flash 统一叫 `deepseek-flash`（不带 v4，与官方模型名一致），百炼上游模型名 `deepseek-v4.1-flash`；**线上已有的文本组 `llm.deepseek-v4-flash` 在同一次 gateway 发布里直接改名为 `llm.deepseek-flash`，不保留过渡别名**；pro 不动。源头 ai-gateway `scripts/seed/ai.py`、`scripts/seed/legacy_overrides.json`、`contract/model-groups.seed.yaml`；存量库用 `deploy/seed/ai-gateway-seed-diff-deepseek-chat.sql`。

| 类型 | 名称 | 要点 |
|---|---|---|
| provider | `deepseek-chat` | DeepSeek 官方，`openai.chat`，`https://api.deepseek.com/v1`（thinking_style=volcengine，json_schema 降级 json_object） |
| account | `deepseek-chat.main` | 密钥 `api_key`，只经管理端写入 |
| 价格模型 | `pm.deepseek-chat` | CNY / 千 token，单价占位（输入 0.002、输出 0.003），待按官方价核对 |
| 文本部署 | `deepseek-chat.deepseek-flash` | vendor_model `deepseek-flash`；不读图，json / thinking / tools 开 |
| 文本部署 | `deepseek-chat.deepseek-v4-pro` | vendor_model `deepseek-v4-pro`；**不读图** |
| 视觉部署 | `deepseek-chat.vl.deepseek-flash` | vendor_model `deepseek-flash`；读图、json、thinking 开；read 200 s；内联图 ≤ 10 MiB |
| 文本部署（新） | `bailian-chat.deepseek-flash` | 取代线上 `bailian-chat.deepseek-v4-flash`；vendor_model `deepseek-v4.1-flash`；参数同旧部署（不读图，json / thinking / tools 开） |
| 文本部署（线上已有） | `bailian-chat.deepseek-v4-flash` | 移出所有组并**停用**（保留行，便于对照历史账本；运行态与删除等价），确认后可在管理端删除 |
| 视觉部署 | `bailian-chat.vl.deepseek-flash` | 复用 `bailian-chat` / `bailian-chat.main`；vendor_model `deepseek-v4.1-flash`（同百炼文本部署）；读图、json 开，thinking 关；启用（视觉能力按用户资料启用，上线前用 readiness 与一次真实读图调用核对） |
| 视觉组（新） | `vlm.deepseek-flash` | `deepseek-chat.vl.deepseek-flash` tier 0 → `bailian-chat.vl.deepseek-flash` tier 1（weight 均 100） |
| 文本组（新，取代 `llm.deepseek-v4-flash`） | `llm.deepseek-flash` | `deepseek-chat.deepseek-flash` tier 0 → `bailian-chat.deepseek-flash` tier 1 |
| 文本组（线上已有，删除） | `llm.deepseek-v4-flash` | 增量 SQL 删除组与全部成员（运营另加的成员先原样搬进 `llm.deepseek-flash`）；发布后按旧名调用一律被网关拒（模型组不存在） |
| 文本组（已有，组名不改） | `llm.deepseek-v4-pro` | `deepseek-chat.deepseek-v4-pro` tier 0 → `bailian-chat.deepseek-v4-pro` tier 1 |

不建 v4-pro 的视觉组（v4-pro 不读图），也不建单渠道组：排障用调用参数 `constraints.allowed_deployments` 指定单一部署（sidecar 环境变量 `REVERSE_ALLOWED_DEPLOYMENTS`）。

对 Go 的影响：
- 反推 sidecar `tools/prompt-reverse` 默认模型组 `vlm.deepseek-flash`；`check-studio-readiness.sh` 的必需组加入它。
- studio / canvas 的视觉润色与反推仍用 `vlm.qwen-3.8-flash`，不改；上文「只有 `vlm.qwen-3.8-*` 带视觉」现应补上 `vlm.deepseek-flash`（Go 代码未用它）。
- 文本润色组改为 `llm.deepseek-flash`：studio-rpc `Rewrite.ModelGroup` 默认值与 `deploy/etc/studio-rpc.yaml` 已改。发布后官方优先（Java 同组调用一起变），费用计入 DeepSeek 官方账户；`deepseek-chat.main` 缺密钥时官方部署在发出前失败、同一次调用切到百炼，详见 `13-上线手册.md` ① 的「流量变化提示」。
- **切换窗口**：gateway 发布（旧组消失）到 Java 数据切换 SQL（`deploy/sql/ops/java-deepseek-flash-switch-2026-10-31.sql`）执行完、Go studio-rpc 换上新配置之间，Java 润色（rewrite）/ 视频翻译（translator）/ 智能体对话（text）报模型组不存在；Go 润色失败只降级为原提示词。低峰期操作，顺序见 `13-上线手册.md` ①。
- Java 代码里还有写死旧组名的出厂默认（`DefaultModelRoleResolver.LLM_DEEPSEEK_V4_FLASH`、`ModelGroups.LLM_DEEPSEEK_FLASH`），只在绑定 `model_roles` 缺对应角色时才用；切换 SQL 后各绑定都显式配了新组，平时用不到。新建库时 Flyway V1.0.165 / V1.0.173 仍会写旧组名，需要 Java 后续版本改常量与迁移（本次按约定不改 Java 代码）。
