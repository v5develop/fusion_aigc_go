# AI文案服务

AIGC 本地生活 · AI文案模块后端。Go + go-zero，数据落在现有系统的 `fusion_aigc` 库，新表 `afv_` 前缀。

- 设计方案：`~/Documents/echo/ai-copywriting-design.md`
- 建表脚本：`deploy/sql/V1.0.106__ai_copywriting.sql`（只新增 `afv_copywriting`、`afv_copywriting_option`）

## 服务

| 服务 | 端口 | 职责 |
|---|---|---|
| `copywriting-rpc` | 9201 | 文案生成与增删改查、选项字典、生成历史 |
| `gateway-api` | 8888 | App 接口 `/api/v1/copywriting/*` |
| `admin-api` | 8889 | 运营后台 `/admin/v1/copywriting/*` |

模型不写在配置里：每次生成时按 `afv_system_config.ai_writing_model_id` → `afv_ai_model.code` →
`afv_ai_provider`（地址与密钥）解析，当前为 `qwen3-vl-plus`（阿里云百炼）。换模型改配置值即可。

## 本地运行

```bash
docker compose -f deploy/docker-compose.yml up -d etcd   # 依赖 etcd；mysql 用现有容器
make db          # 建两张新表 + 18 条选项种子数据
make run-all     # 编译并后台启动，日志在 logs/
deploy/scripts/e2e.sh   # 端到端测试（41 项，会真实调用模型）
make stop-all
```

开发阶段身份走请求头：App 用 `X-User-Id`，后台用 `X-Admin-Id`；接入现有用户 / Admin 体系后替换中间件即可。

## 本版范围

做：三条创作路径（AI帮写 / 爆款仿写 / 自主撰写）、AI帮写与AI优化、重新生成、列表搜索、详情编辑删除、后台选项维护、
提交幂等、内容审核（关键词）、提示词模板落库可配。

**不做：文案详情页的「已生成的内容」和「生成历史」**。相关代码已写好但本版不交付，
接口 `GET /copywriting/:id/derivatives` 前端暂不接入，源码中以 `TODO 本版不交付` 标注：
`copywriting.proto`、`derivatives_logic.go`（rpc 与网关各一处）、`common.go` 的 `assetTypesOf`、
`shared_model.go` 的 `ListDerivatives`。启用前需确认各标签对应的 `afv_asset.type`，
以及下游模块是否把 `afv_creation_task.external_ref` 写成文案 id。

## 自带机制

- **幂等**：生成类接口必须带 `X-Request-Id`，落在 `afv_copywriting.request_id`，
  唯一键 `uk_user_request(user_id, request_id)`；重复提交返回同一条，连点不会重复扣费。
- **内容审核**：入参与模型出参都过 `pkg/moderation`。当前为关键词过滤，词库在
  `afv_copywriting_option` 的 `BLOCKWORD` 类型（code 即词本身），运营可维护；
  `Moderation.Mode: off` 可关闭。现有系统的文本审核走网易易盾，凭证不在库里，
  接入后替换 `moderation.Checker` 实现并按约定写 `afv_moderation_log` 即可。
- **提示词**：模板在 `afv_copywriting_option` 的 `PROMPT` 类型（5 条：两种生成的系统提示词、
  主题补写、卖点优化、输出格式要求），运营可改；读不到时回落到 `prompt.go` 里的内置文本。
- **卡单回收**：生成跑在进程内协程里，服务重启会留下「生成中」的记录。`StuckRecovery`
  在启动时扫一遍、之后每分钟扫一次，超过两倍生成超时仍未结束的标记为失败，前端不会一直轮询。
- **并发控制**：按 `afv_ai_model.max_concurrency`（当前 5）限制同时打给模型的请求数，
  超出的排队等待，不会把压力直接怼到百炼。

## 待办（代码里都有 TODO，`grep -rn TODO app pkg`）

| 事项 | 卡在哪 | 代码位置 |
|---|---|---|
| **计费**：预扣 100 点 → 成功结算 / 失败退回 | 100 点配在 `afv_model_pricing` 还是 `charge_component.base_points`；`action_type` 用 TEXT_CHAT 还是新增 TEXT_GEN | `generate_logic.go`，开关 `Billing.Enabled` |
| **用户鉴权**：接 `afv_app_user` | token 怎么验 | `gateway/.../auth_middleware.go` |
| **后台鉴权**：接 `afv_admin_*` | 登录态与权限校验方式 | `admin/.../adminauth_middleware.go` |
| **创作任务**：写 `afv_creation_task` | `entry_code` / `feature_code` 命名确认（`mode` 已定为 0） | `generate_logic.go`，开关 `Task.Enabled` |
| **店铺档案**：店铺信息拼进提示词 | 该模块接口 | `generate_logic.go` |
| **审核接易盾** + 写 `afv_moderation_log` | 易盾凭证；provider 取值与 task_id 约定 | `pkg/moderation`，开关 `Moderation.LogToDb` |
| **生成历史 / 已生成的内容** | 本版不交付 | 见上节 |

这些关着不影响功能：用户能正常生成、编辑、管理文案，只是**不扣点数**、任务和成本不进现有统计、鉴权仍是开发模式。

## 开发

```bash
make gen-rpc     # 改了 app/copywriting/rpc/copywriting.proto
make gen-api     # 改了 gateway.api / admin.api
make gen-model   # 改了建表 SQL（只重生成 *_gen.go）
make test
```
