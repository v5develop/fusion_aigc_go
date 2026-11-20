# 画布智能体工具（Function Call / MCP 双形态）设计

> 2026-11-13（2026-11-14 补：工具节点开放 §6.1、生成前报价 §6.2）· 分支 feat/studio · 相关：`app/canvas`（canvas-rpc）、`docs/design/xiaoqi-2026-10-30/04-infinite-canvas.md`、
> 前端 `fusion-aigc-web/src/views/canvas/editor/{graph.ts,types.ts,placement.ts,useCanvasEditor.ts}`。

## 1. 目标与分层

让智能体（我们自己的 Agent、第三方 MCP 客户端、OpenAI/Anthropic 风格的 Function Call 循环）直接操作**调用者自己的**画布：
建画布、读结构、增删改节点、移动缩放、连线断线、批量布局、触发图片 / 视频生成并回写、查询生成状态。

**同一份工具定义，两种暴露形式**：

```
            ┌──────────── app/canvas/agenttool（唯一工具清单：名称 / 说明 / 入参出参 JSON Schema / 注解 / 批量上限）
            │                 ├─ Render: OpenAI tools / Anthropic tools / MCP tools/list   ← 三种格式都从这一份渲染
            │                 ├─ Validate: 入参按 Schema 校验（canvas-rpc 执行前调用）
            │                 └─ MCPServer: JSON-RPC 2.0 / Streamable HTTP（initialize / ping / tools/list / tools/call）
gateway-api │  POST /api/v1/canvas/mcp                 MCP（Streamable HTTP，单次 JSON 响应，无会话状态）
            │  GET  /api/v1/canvas/agent/tools         Function Call 定义（?format=openai|anthropic|mcp）
            │  POST /api/v1/canvas/agent/call          Function Call 执行（接受 {name, arguments, call_id} 或 OpenAI tool_call 原样）
            │      └─ Auth（会话令牌）→ CanvasBearer → 频率配额（userquota）→ CanvasRpc.AgentCall
            │  POST /internal/v1/canvas/agent/call     服务间入口（Java 智能体，HMAC，§12）→ 同一个 Caller → CanvasRpc.AgentCall
            │  GET  /internal/v1/canvas/agent/tools    服务间工具定义（HMAC）
canvas-rpc  │  rpc AgentCall(user_id, tool, arguments, call_id, client, agent) → 校验 → 幂等 → 执行 → 审计
            └─ 执行器只复用现有能力：快照解析 / ValidateDoc / 乐观锁 Save / RunNode（含计费冻结）/ GetRun / ListRuns / CancelRun / SelectCandidate / Catalog
```

执行器放在 canvas-rpc 内部（直接操作**存储态快照**，不经 GET 回填后的快照再整段写回），网关只做协议适配与鉴权；
Java 侧智能体的 canvas_* 工具执行器走**服务间 HMAC 入口**（§12），不转发用户令牌（异步轮次在 worker 里跑，令牌会过期）。

## 2. 工具清单

| 工具 | 类别 | 只读 | 破坏性 | 计费 | 说明 |
|---|---|---|---|---|---|
| `canvas_list` | 画布 | ✔ | | | 列出我的画布（分页，≤50/页） |
| `canvas_create` | 画布 | | | | 新建空画布 |
| `canvas_get` | 画布 | ✔ | | | 完整结构：节点（类型、位置、尺寸、data，含运行态回填）、连线、revision；可按 node_ids 过滤 |
| `canvas_add_nodes` | 节点 | | | | 批量加节点（≤50），可带 `ref` 供同批连线引用；未给坐标自动找空位 |
| `canvas_update_nodes` | 节点 | | | | 批量改节点内容（标题 / 文本 / 图片地址 / 模型参数 / data 合并补丁，≤50） |
| `canvas_delete_nodes` | 节点 | | ✔ | | 批量删节点（≤100），相关连线一并删除 |
| `canvas_move_nodes` | 布局 | | | | 绝对坐标或相对位移（≤200） |
| `canvas_resize_nodes` | 布局 | | | | 改宽高或按比例缩放（≤200，80–4000px） |
| `canvas_connect` | 连线 | | | | 批量连线（≤50）；目标口可省略自动推断；单槽口（提示词 / 首帧 / 尾帧 / 服装 / 结果图 / 工具输入）自动替换旧线 |
| `canvas_disconnect` | 连线 | | ✔ | | 按连线 id 或（source, target, target_handle）断开 |
| `canvas_auto_layout` | 布局 | | | | 分层（DAG 最长路径）自动排版，LR / TB，可限定节点子集；分组框不参与 |
| `canvas_batch_edit` | 批量 | | ✔ | | 混合操作原子执行（≤100 步）：add/update/delete/move/resize/connect/disconnect/layout，一次保存一个 revision |
| `canvas_catalog` | 生成 | ✔ | | | 可用模型、比例、分辨率、时长、张数上限、参考图上限与价目 |
| `canvas_quote_node` | 生成 | ✔ | | | 生成前报价（只读）：对某个节点执行会扣多少点、计价依据、由谁计费、是否需要网页登录令牌、预计耗时（§6.2） |
| `canvas_run_node` | 生成 | | | ✔ | 对图片 / 视频模型节点**与工具节点**（裁剪 / 放大 / 扩图 / 局部重绘 / 反推 / 试穿）发起执行：按节点 data + 已保存连线组装请求，走 RunNode，计费按节点原链路（§6.1） |
| `canvas_get_run` | 生成 | ✔ | | | 查询一次生成：状态、进度、候选、视频、错误；`wait_seconds`≤25 可等待终态 |
| `canvas_list_runs` | 生成 | ✔ | | | 画布 / 节点的生成记录（≤50） |
| `canvas_cancel_run` | 生成 | | ✔ | 退点 | 取消排队 / 运行中的生成（按现有规则退点） |
| `canvas_select_candidate` | 生成 | | | | 选中某次成功生成的第 i 张候选（节点展示与下游引用以它为准） |

> 前端不保存 viewport（`index.vue` 载入时居中），因此「移动与缩放」指节点的位置与尺寸（`position` / `data.width,height`），不提供视口工具。

### 2.1 节点类型映射（与前端工厂一致）

| 工具 `type` | 快照 `type` / `data.kind` | 默认 data | 默认尺寸 | id 前缀 |
|---|---|---|---|---|
| `text` | text / text | title 文字, text, mentions [] | 320×220 | `t_` |
| `image` | image / input | title 图片, image | 240×260 | `n_` |
| `result_image` | image / result（有 `image-in` 口） | title 结果图 | 240×260 | `n_` |
| `video` | video / video-input | title 视频, video | 338×380 | `v_` |
| `image_model` | model / model | model, modelId, quality medium, outputFormat jpeg, count 1, cfg 0.95, ratio 1:1, size 1K, prompt, maxImageRefs | 318×440 | `m_` |
| `video_model` | model / video | 同上 + duration 4s, sound true, ratio 16:9, size 720p | 338×480 | `m_` |
| `tool` | tool / tool（`tool`: crop/upscale/outpaint/mask/i2p/tryon） | title、tool、sourceNodeId、工具默认参数 | 340×320（tryon 380×460） | `tool_` |
| `group` | module / module（isGroup，zIndex -1，不能连线） | title 分组 | 760×420 | `mod_` |

指定 `model` 时按 canvas-rpc 模型表（`Config.FindModel`，含别名）补 `modelId / maxImageRefs`，并校验比例 / 分辨率 / 时长 / 张数是否在该模型档内。

### 2.2 连线口（与 `validate.go` 同一套规则）

源口：text→`text`，image→`image`，video→`video`，图片模型 / 工具→`image-out`（反推 i2p→`text`），视频模型→`video`。
目标口：模型 `prompt`/`image`，视频模型另有 `image1`(首帧)/`image2`(尾帧)/`video`；工具 `image`；试穿 `garment`/`models`；结果图 `image-in`。
省略 `target_handle` 时推断：源是文本或反推→`prompt`；目标是工具→`image`；目标是结果图→`image-in`；源是视频模型→`video`；其余→`image`（试穿必须显式）。
保存前整段跑 `ValidateDoc`（类型、上限、成环、dataURL），违规整批拒绝、不落库。

## 3. 入参 / 出参 JSON Schema

权威定义在 `app/canvas/agenttool/tools.go`（`GET /api/v1/canvas/agent/tools?format=mcp` 原样输出）。所有入参 `additionalProperties:false`，
camelCase 键会先归一为 snake_case（兼容网关 jsoncase 与习惯驼峰的模型）。以下为摘要（`?` = 可选）。

公共片段：

```jsonc
"canvas_id":     {"type":"string","pattern":"^cv_[0-9a-f]{16}$"}
"base_revision": {"type":"string","maxLength":32,"description":"给了就严格乐观锁：与服务端不符直接返回 CANVAS_CONFLICT；不给则在最新版本上应用并自动重试 3 次"}
// 所有写操作的出参都含：
"MutationOut":   {"canvas_id":"string","revision":"string","node_count":"integer","changed_node_ids":["node_id"]}  // changed_node_ids ≤200，与画布事件同口径
```

| 工具 | 入参 | 出参 |
|---|---|---|
| canvas_list | `{page?:int≥1, page_size?:int 1–50}` | `{total, canvases:[{canvas_id,title,revision,node_count,cover_url,updated_at}]}` |
| canvas_create | `{title?:string≤100}` | `{canvas_id,title,revision,node_count}` |
| canvas_get | `{canvas_id, node_ids?:string[]≤200, include_runtime?:bool=true}` | `{canvas_id,title,revision,node_count,nodes:[Node],edges:[Edge]}` |
| canvas_add_nodes | `{canvas_id, base_revision?, nodes:[NodeSpec]1–50}` | `MutationOut + {nodes:[{ref?,node_id,type,x,y,width,height}]}` |
| canvas_update_nodes | `{canvas_id, base_revision?, updates:[NodePatch]1–50}` | `MutationOut + {updated:[node_id]}` |
| canvas_delete_nodes | `{canvas_id, base_revision?, node_ids:string[]1–100}` | `MutationOut + {deleted_nodes:[id], deleted_edges:[id]}` |
| canvas_move_nodes | `{canvas_id, base_revision?, moves:[{node_id, x?, y?, dx?, dy?}]1–200}` | `MutationOut + {nodes:[{node_id,x,y}]}` |
| canvas_resize_nodes | `{canvas_id, base_revision?, sizes:[{node_id, width?, height?, scale?:0.1–10}]1–200}` | `MutationOut + {nodes:[{node_id,width,height}]}` |
| canvas_connect | `{canvas_id, base_revision?, edges:[EdgeSpec]1–50}` | `MutationOut + {edges:[Edge], replaced:[edge_id]}` |
| canvas_disconnect | `{canvas_id, base_revision?, edge_ids?:string[]≤100, pairs?:[{source,target,target_handle?}]≤100}` | `MutationOut + {removed:[edge_id]}` |
| canvas_auto_layout | `{canvas_id, base_revision?, node_ids?:string[]≤500, direction?:"LR"\|"TB", gap_x?:int 20–800, gap_y?:int 20–800, origin_x?, origin_y?}` | `MutationOut + {nodes:[{node_id,x,y}]}` |
| canvas_batch_edit | `{canvas_id, base_revision?, ops:[{op:enum, args:object}]1–100}`，`args` 形状 = 对应单工具的单个元素（layout 的 args = auto_layout 入参去掉 canvas_id） | `MutationOut + {results:[object], refs:{ref:node_id}}` |
| canvas_catalog | `{kind?:"image"\|"video"}` | `{models:[{model,model_id,label,kind,ratios,sizes,qualities,durations,sound,count_max,max_image_refs,prices:[{size,duration,points}]}]}` |
| canvas_quote_node | `{canvas_id, node_id, model?, prompt?, ratio?, size?, quality?, output_format?, count?, duration?, sound?, params?:object}`（覆盖项与 run_node 相同） | `{canvas_id,node_id,node_type,tool?,kind,available,reason?,reason_code?,billing:canvas\|java\|studio\|free,price_known,points,max_points,settle_mode,charged,billing_mode?,basis:{model,model_group,size,quality,ratio,count,duration_s,unit_points,price_unit,quantity},requires_login_token,login_available,estimated_seconds,estimate_source,warnings,note?,java_request?:{route,path,body}}` |
| canvas_run_node | `{canvas_id, node_id, run_id?:string≤64, model?, prompt?:≤3000, ratio?, size?, quality?, output_format?:jpeg|png|webp（缺省 jpeg，仅图片）, count?:1–8, duration?, sound?, params?:object, max_points?:int≥0, expected_points?:int≥0}` | `{request_id,run_id,status,credits,max_points,settle_mode,warnings:[],replayed}` |
| canvas_get_run | `{request_id:string(数字), wait_seconds?:int 0–25}` | `Run` |
| canvas_list_runs | `{canvas_id, node_id?, limit?:1–50}` | `{runs:[Run]}` |
| canvas_cancel_run | `{request_id}` | `Run` |
| canvas_select_candidate | `{canvas_id, node_id, run_id, index:int≥0}` | `Run` |

结构体：

```jsonc
NodeSpec  = {ref?:string≤32, type:enum(2.1), title?:≤100, x?, y?, width?:80–4000, height?:80–4000,
             text?:≤5000, image_url?:uri, video_url?:uri, model?, prompt?:≤3000, ratio?, size?, quality?:low|medium|high, output_format?:jpeg|png|webp,
             count?:1–8, duration?, sound?:bool, tool?:crop|upscale|outpaint|mask|i2p|tryon, data?:object}
NodePatch = {node_id, title?, text?, image_url?, video_url?, model?, prompt?, ratio?, size?, quality?, output_format?, count?, duration?, sound?,
             data?:object /* JSON Merge Patch：null 删除键；运行态键（status/candidates/runId…）一律忽略 */}
EdgeSpec  = {source:node_id|ref, target:node_id|ref, target_handle?:enum(prompt,image,video,image1,image2,first,last,garment,models,image-in),
             source_handle?:string, order?:int≥1}
Node      = {node_id,type(2.1 的工具类型),x,y,width,height,title,data:object /* 前端 data 原样 */}
Edge      = {edge_id,source,source_handle,target,target_handle,order}
Run       = {request_id,run_id,canvas_id,node_id,kind,status:submitting|queued|running|succeeded|failed|cancelled,progress,model,
             candidates:[{url,width,height}],selected_index,video_url,text,credits,error,error_code,retryable,library_status,created_at,finished_at}
```

错误统一为工具结果 `is_error=true` + `{error_code, message, retryable, revision?}`（不是协议错误，模型能看到并自我修正）：
`PARAM_INVALID`（Schema / 连线规则 / 模型档）、`NOT_FOUND`（画布或节点不存在**或不属于你**）、`CANVAS_CONFLICT`（带服务端 revision）、
`IDEMPOTENCY_CONFLICT`、`CALL_IN_PROGRESS`、`RATE_LIMITED`、`INSUFFICIENT_POINTS`、`NOT_READY`、`FORBIDDEN`、`FORBIDDEN_SCOPE`（MCP 令牌作用域不含该工具，§13）、`BUDGET_EXCEEDED`（报价超过 `max_points`，带 `quoted_points / max_points`，§6）、`PRICE_UNKNOWN_NEEDS_CONFIRM`（带 `max_points` 但报不出准确点数，§6）、
`MCP_TOKEN_JAVA_TOOL_UNSUPPORTED`（MCP 令牌碰需要用户 Java 登录令牌的操作，§13.2）、`LOGIN_REQUIRED`（服务间入口执行 Java 计费的工具、拿不到该用户的登录令牌，§6.1）、`SERVER_ERROR`。

## 4. 权限与鉴权

* **身份只来自凭证**：网关 `Auth` 中间件解析 `Authorization: Bearer <令牌>`（与前端同一套：32 位 hex 共用会话令牌，或回落 JWT；dev 模式 `X-User-Id`），
  `user_id` 写进 `AgentCallReq.user_id`。工具 Schema **没有** user_id 字段，入参里出现任何未声明键直接拒绝，杜绝参数注入他人身份。
* **MCP 凭证**：MCP 规范 Streamable HTTP + `Authorization` 头。**推荐用 MCP 访问令牌**（`Bearer mcp_…`，画布页自助生成，带作用域、到期、可吊销，§13）；
  登录令牌仍可用（兼容现状），但随登录态过期、且等同完整账号权限，不建议交给外部客户端。客户端配置示例：
  `{"mcpServers":{"fusion-canvas":{"type":"http","url":"https://<host>/api/v1/canvas/mcp","headers":{"Authorization":"Bearer mcp_<令牌>"}}}}`。
* **防越权**：canvas-rpc 每个工具都以 `(user_id, canvas_id)` 走 `GetOwned`（`WHERE user_id=? AND deleted=0`），run 走 `ownedRun`；
  他人画布 / run 一律返回 `NOT_FOUND`（不区分「不存在」与「无权」，不泄露存在性）。引用解析 `refLookup` 只看本画布的 run。
  图片地址仍过 `CheckDocInlineMedia`（拒 data:/blob:/file:），生成时上游产物按本画布记录解析冻结（沿用 `ref_resolve.go`）。
* **MCP 传输安全**：校验 `Origin`（有则必须在 `CanvasAgent.AllowedOrigins` 内，空列表 = 只允许无 Origin 的服务端客户端），防 DNS rebinding；
  只接受 `POST`（`GET`/`DELETE` 回 405，不开服务器推送流）；请求体 ≤1MB；`MCP-Protocol-Version` 支持 2025-06-18 / 2025-03-26。

## 5. 幂等与并发

* **画布写入 = 语义补丁 + 乐观锁**：执行器读**存储态**快照 → 在内存里应用操作 → `ValidateDoc` → `Save(IfMatch=读到的 revision)`。
  - 不带 `base_revision`：CAS 失败自动重读重放（最多 3 次）。操作是「加这个节点 / 连这条线」的语义补丁，不会覆盖别人已保存的修改。
  - 带 `base_revision`：与服务端不符立即 `CANVAS_CONFLICT`（返回最新 revision），适合「读完再基于读到的内容改」的场景。
  - 每次成功写入产生新 revision（与前端保存同一列、同一格式）。
* **调用幂等**：写 / 生成类工具支持 `call_id`（MCP：HTTP `Idempotency-Key` 头或 `params._meta.idempotency_key`；Function Call：`call_id` 或 OpenAI `tool_call.id`）。
  canvas-rpc 先插一行审计 `idem_key=call_id`（唯一键 `(user_id, idem_key)`）占位，再执行：
  同 key 同参数已成功 → 原样回放结果（`replayed=true`）；同 key 不同参数 → `IDEMPOTENCY_CONFLICT`；仍在执行 → `CALL_IN_PROGRESS`（可重试）；
  失败的调用释放 key（置 NULL），允许用同 key 重试。占位超过 2 分钟未完成视为崩溃残留，可被接管。
  每个占位带 `lease_token`：执行前复核占位仍属于自己（被接管 → 不执行，回 `CALL_IN_PROGRESS`），收尾按 `(id, lease_token, status=执行中)` CAS，
  影响 0 行（已被接管或不存在）打 WARN（`check=agent_call.finish`），旧执行者的结果不进审计 / 回放。
* **生成幂等**：`canvas_run_node` 的 `run_id` 缺省由 `sha256(user_id:call_id)` 派生（`ag_` 前缀），与 RunNode 现有 `uk(user_id, run_id)` 幂等叠加；
  同一节点同一时刻只允许一个活动 run（`uk_active_node`），并发触发返回现有错误。
  受理阶段就失败、没产生计费的 run（`status=failed` + `error_code ∈ {BILLING_REJECTED, SUBMIT_FAILED, PARAM_INVALID, SUBMIT_TIMEOUT}`
  + `billing_status ∈ {none, refunded}`）不挡同 call_id 重试：派生种子追加尝试序号（`user_id:call_id#n`，n=0 与旧口径一致），
  参数（画布 / 节点 / 归一后的请求）相同才换下一个序号真正重跑，参数改过落回原 run_id 由 RunNode 报 `CONFLICT`；
  冻结中 / 已结算 / 待对账 / Java、studio 自行计费的失败照旧回放原 run，不会重复扣点。同一 call_id 最多 10 个序号，超过报 `IDEMPOTENCY_CONFLICT`。
  显式给 `run_id` 时不派生，重放语义由调用方负责。
* **与前端同步**：见 §11（revision 事件 + SSE + 历史快照三方合并）。

## 6. 计费

* `canvas_run_node` **只调用 `core.RunNode`**：准入 `Billing.CheckAccess` → 落库（提交中）→ `holdAndQueue` 冻结（每 run 一张预留单，`canvas-<task_id>`）
  → 提交后端；结算 / 部分退款 / 失败退点 / 超时退点 / UNKNOWN 待对账全部沿用现有轮询器与 `run_guard` 对账。执行器不接触 `pkg/billing`，没有任何绕过路径。
* 模型节点（`image_model` / `video_model`）与工具节点（2026-11-14 起，§6.1）都开放；计费一律走节点原链路，执行器不碰计费。
* `canvas_cancel_run` 走 `CancelRun`（排队 / 运行中按现有规则退点）。出参透出 `credits / max_points / settle_mode`，让智能体知道冻结了多少。
* 余额不足 → `INSUFFICIENT_POINTS`（3003 原码映射），不重试。生成频率另有网关配额（§8）。
* **点数上限 `max_points`**（2026-11-14，`canvas_run_node` 可选参数，MCP / Function Call 同一份定义）：RunNode 在**冻结前**（落库前）用与冻结同源的报价比较
  （canvas 计费：`billing.Quote(c.charge(...))`，与 `holdAndQueue` 冻结同一个 Charge，按量结算取冻结上限；试穿：studio-rpc Quote；裁剪 0）：
  报价 > `max_points` → 不落 run、不冻结，返回 `is_error` + `BUDGET_EXCEEDED`（`quoted_points`、`max_points`）。
  报不出准确点数的 Java 计费工具（放大 / 扩图 / 局部重绘，`canvas_quote_node` 的 `price_known=false`）带 `max_points` 时用调用方给的 `expected_points`（Java 侧估价）比较，
  缺失 → `PRICE_UNKNOWN_NEEDS_CONFIRM`（不重试，先征得用户确认）；Go 计费报价暂时失败也回 `PRICE_UNKNOWN_NEEDS_CONFIRM`（可重试）。
  不带 `max_points` 行为不变；幂等回放 / 去重回放不冻结也不比较；网页手动执行不受影响。实现见 `logic/agent_budget.go`。
* **无人值守去重**（2026-11-14）：服务间入口（`auth_source=svc`）与 MCP 访问令牌（`mcp_token`）调 `canvas_run_node`、且**没有显式 `run_id`** 时，
  按（user、canvas、node、输入指纹）去重：同指纹的 run **仍在途**（提交中 / 排队 / 运行中，不限时间）或 **120 秒内已成功** → 直接返回该 run
  （`replayed=true`，不再冻结点数，`warnings` 注明）。指纹 = 与落库同口径的归一请求 + 每个上游引用当前解析到的实际产物
  （上游节点最新成功 run 的主键，对比候选 run 受理时冻结的 `frozen.refs[*].run_id`；上游重做后下游不再算同一请求；没有冻结记录的旧 run 不参与）。
  兜住 Java 判「结果未知」后模型换 call_id 重复提交；失败 / 取消的 run 不挡重试；登录令牌（网页手动「执行」、用户自己的 Function Call）与显式 `run_id` 不受影响。
  代价：120 秒内确实想用完全相同的输入与上游再生成一次，需换输入或等过窗口。

### 6.1 工具节点（2026-11-14）

`canvas_run_node` 对工具节点按前端 `buildRunRequest` 的 tool 分支组装请求：节点 data → params（`toolParamsOf` 同口径：
crop `crop{shape,x,y,w,h}`、upscale `scale`、outpaint `outWidth/outHeight` 或 `ratio + resolution`、mask `maskPrompt + maskBox → bbox 4 点`、
i2p `language`、tryon `tryonItems / garmentName / fabric / tryonModel / tryonResolution / tryonAspectRatio`），
再叠加 `params`（自由对象，键归一为 snake_case，`null` 删键；`mask_rid` 只能服务端写，拒收）与通用覆盖项
（upscale：`size` → `resolution`；outpaint：`ratio` 清掉宽高、`size` → `resolution`；mask：`prompt` → `mask_prompt`；tryon：`model / size / ratio / prompt`）。
没连图片线时按 `data.sourceNodeId` 取上游图（validate 同样认它）；非试穿工具没有输入图直接 `PARAM_INVALID`（提示用 canvas_connect）。

| 工具（`data.tool`） | 后端 | 计费 | 登录态 / 会话令牌（user） | 服务间入口（svc，Java 画布助手） | MCP 令牌（mcp_token） |
|---|---|---|---|---|---|
| `i2p` 反推提示词 | gateway | Go：RunNode 冻结 / 结算（`canvas_tool_i2p`） | ✔ | ✔ | ✔ |
| `crop` 裁剪 | 媒体服务 | 不计费 | ✔ | ✔ | ✔ |
| `tryon` 模特试穿 | studio | studio 按 `user_id` 冻结（不需要用户令牌） | ✔ | ✔ | ✔ |
| `upscale` 高清放大 / `outpaint` 扩图 / `mask` 局部重绘 | Java 兼容工具 | Java（透传用户 Bearer，Java 按其价目扣点） | ✔ 本次登录令牌 | 用该用户**本人**缓存的登录令牌（`Bearers`，网页最近一次请求留下）；缓存没有 → `LOGIN_REQUIRED`「这个操作需要你在网页里登录后再试」；Java 回 401（令牌过期）同样映射为 `LOGIN_REQUIRED` | `MCP_TOKEN_JAVA_TOOL_UNSUPPORTED`（不借用缓存，§13.2），不组请求、不落 run |

* 「不借用缓存 Bearer」只针对 `mcp_token`：外部令牌不能借网页会话代用户调 Java。服务间入口是我们自己的 Java 智能体代用户操作，沿用 §12.1 的既有约定。
* 工具 run 照旧受 §6 无人值守去重、§5 生成幂等（Java、studio 自行计费的失败照旧回放原 run，不重复扣点）与网关生成配额约束。
* Java 工具 run 的 `credits` 为 0（点数在 Java 侧冻结，任务中心可见）；试穿同理由 studio 冻结。

### 6.2 生成前报价 `canvas_quote_node`（只读）

与 `canvas_run_node` **同一份**请求组装与归一（`agentNodeRequest` → `normalizeRunSize` → `buildSpec` → `checkRefs`），不冻结、不提交、不落审计行：

| billing | 报价来源 | 与实际冻结的关系 |
|---|---|---|
| `canvas`（image / video / i2p） | `billing.Service.Quote(c.charge(user, "quote", sp))`——与 `holdAndQueue` 冻结同一个 `Charge`（入口码、模型组选档、分辨率、张数 / 秒数、按请求参数冻结的质量与参考图数） | 报价 = 冻结（pkg/billing 的 Quote 与 Freeze 共用 price + applyFreezeCap）。计费开关 off 时 RunNode 不冻结，报 0；shadow 报价目、`charged=false`。另做准入（`CheckAccess`），没开通 → `available=false`、`reason_code=BILLING_NO_FEATURE` |
| `studio`（tryon） | studio-rpc `Quote`（kind=tryon、模型档、分辨率、比例、张数 = 模特数），与 studio 提交同一套归一与 charge | = studio 提交回的 estimated_points |
| `java`（upscale / outpaint / mask） | 价目只在 Java（creaft v1 能力绑定 + v2 价目），Go 不复刻规则：`price_known=false`，给出 `java_request{route, path, body}`——执行时发给 Java 的**同一份**请求体（`javaToolBody`，报价时蒙版只用 bbox / 笔迹 / dataURL，否则按整图框，不碰媒体服务） | Java 画布助手据 `java_request` 在进程内按提交同源的受理 + 报价算准确点数（见 Java `AgentCanvasToolExecutor.estimateRun`）；MCP / 网页调用方看到 `price_known=false`，以执行后 Java 实扣为准 |
| `free`（crop） | — | 0 点 |

* `available=false` 时 `reason / reason_code` 说明为什么现在不能执行：`PARAM_INVALID`（参数 / 连线不全，与执行时同一句话）、`NOT_READY`、`MCP_TOKEN_JAVA_TOOL_UNSUPPORTED`、`LOGIN_REQUIRED`、`BILLING_NO_FEATURE` / `BILLING_NO_OFFER`、`STUDIO_QUOTE_UNAVAILABLE`。
* `requires_login_token`：Java 工具为 true；`login_available`：此刻能否拿到该用户的登录令牌（user 看本次请求、svc 看缓存、mcp_token 恒 false）。
* `estimated_seconds`：近 7 天同类（kind + 模型组）成功 run「创建 → 完成」耗时中位数（≥5 个样本，进程内缓存 10 分钟；`estimate_source=recent_median`），否则经验值（`default`）。
* 报价是只读工具：不占生成配额（`Generates=false`），`canvas_rw` 作用域的 MCP 令牌也能用。
* 花钱透明：智能体在 `canvas_run_node` 前先 `canvas_quote_node` 并把点数告诉用户；Java 智能体的点数预算（§12 调用方）优先用报价，超出用户所见金额先确认。

## 7. 审计

新表 `afv_canvas_agent_call`（迁移 `deploy/sql/Go_V1.0.38__canvas_agent_call.sql`）：每次**写 / 生成**类调用一行
（只读调用只打日志，避免读放大写库）：`user_id, canvas_id, tool, client(mcp|function|grpc), agent(客户端名), call_id, idem_key, args_hash, args(≤64KB),
status(0 执行中 1 成功 2 失败), error_code, error_msg, revision_before, revision_after, run_request_id, result(≤64KB，回放用), duration_ms, client_ip, created_at, updated_at`。
索引：`uk_user_idem(user_id, idem_key)`、`idx_canvas_time`、`idx_user_time`。审计写失败不影响业务结果（记 ERROR 日志），但幂等占位失败（非唯一键冲突）会拒绝执行。

## 8. 速率与批量上限

| 维度 | 默认 | 位置 |
|---|---|---|
| 工具调用 | 每用户 120 次/分钟、5000 次/天 | 网关 `CanvasAgent.CallsPerMinute/PerDay`（userquota，会话 Redis，跨实例） |
| 生成调用（canvas_run_node） | 每用户 10 次/分钟、300 次/天（另计入上一行） | 网关 `CanvasAgent.GeneratePerMinute/PerDay` |
| 单次批量 | add/update/connect ≤50；delete/disconnect ≤100；move/resize ≤200；batch_edit ≤100 步；layout ≤500 节点 | Schema `maxItems` + 执行器复核 |
| 画布总量 | 节点 ≤ `Limits.MaxNodes`（2000）、快照 ≤2MB、标题 ≤100 字、提示词 ≤3000 字 | 沿用 canvas-rpc 配置 |
| 等待 | `canvas_get_run.wait_seconds` ≤25（每 1.5s 查一次） | 执行器 |
| 请求体 | MCP / call ≤1MB | 网关 |

超限回 `RATE_LIMITED`（retryable，附 `retry_after_seconds`）。

## 9. 测试

* `app/canvas/agenttool`：工具清单唯一性、Schema 自洽（必填项都在 properties）、三种渲染格式、入参校验（缺字段 / 多余字段 / 越界 / 驼峰归一）、MCP 协议（initialize / tools/list / tools/call / 未知方法 / 通知 202 / GET 405 / Origin）。
* canvas-rpc `agent_tool_test.go`：工具节点来源规则（mcp_token → `MCP_TOKEN_JAVA_TOOL_UNSUPPORTED` 且不到 Java；svc 无缓存令牌 → `LOGIN_REQUIRED`；svc 有缓存令牌 → 带该用户 Bearer 提交 Java，提交体 = 报价的 `java_request`）、
  报价 = 冻结（本地 fusion_aigc 真实价目、shadow 计费：图片 2 张 / 视频 4 秒 / 反推工具，`canvas_quote_node.points == canvas_run_node.credits`；无该库时跳过）、工具 data → params 与覆盖。
* canvas-rpc `agent_*_test.go`：操作引擎（加节点默认值与 id 前缀、自动找空位、连线推断与单槽替换、删除连带断线、布局分层、batch 原子回滚）、run 请求组装（连线 → references）。
* 端到端 `agent_e2e_test.go`：MCP HTTP → `agenttool.MCPServer` → `core.AgentCall` → 真 MySQL（临时库，建表后删除）：
  创建画布 → 加节点（文本 + 图片模型）→ 连线 → 读取 → 冲突 → 幂等回放 → 越权读取他人画布得 NOT_FOUND。无 MySQL 时跳过（`CANVAS_AGENT_E2E_DSN` 可指定）。

## 10. 未做与后续

1. （服务端已做，§11）实时同步；前端合并逻辑在 web 侧实现。
2. （已做，§13）画布 MCP 访问令牌（作用域、到期、吊销）。按令牌限速已做（§13.x「按令牌限速」）：在按用户配额之外，每个 mcp_ 令牌另有独立的每分钟上限。
3. （已做，§6.1）工具节点对智能体开放；无人值守的确认规则：执行前报价，Java 智能体点数预算超出用户所见金额先确认（Java 侧确认表单）。
4. （已做，§6.2）生成前报价 `canvas_quote_node`。未做：Java 计费工具对 MCP / 网页调用方的准确报价（需要 Java 提供只读报价接口，目前只有 Java 画布助手能在进程内报出）。

## 11. 画布实时同步（P0-8，服务端已实现）

### 11.1 事件

每次画布 **revision 成功变化**，canvas-rpc 在写入之后发布 Redis pub/sub 事件到频道 `canvas:{canvasId}:events`：

| 写路径 | source | actor |
|---|---|---|
| 用户保存 `PUT /api/v1/canvas/canvases/:id`（含 force） | `user` | `user:<userId>` |
| 新建 / 另存为 / 从模板新建（含智能体 `canvas_create`） | `user`（智能体调用时为 `agent`） | 同上 / 同下 |
| 智能体写入（MCP、Function Call、服务间入口；所有 `agentMutate` 写类工具，含 `canvas_batch_edit`） | `agent` | 服务间入口 `svc:<keyId>`；其余 `<client>:<agent>`（如 `mcp:claude-desktop`） |
| 生成状态变化：受理（提交中→排队）、成功、失败 / 超时 / 取消 / 强制失败（`endRun`）、提交中兜底超时、选中候选 | `run` | `run:<requestId>` |

> run 结果**不写快照**（GET 时按 run 回填），所以 `source=run` 的事件 revision 不变（取画布当前 revision），前端收到后只需刷新该节点的 run 状态（`GET /runs/:id` 或重新 GET 画布），不需要合并。
> 运维工具 `tools/ops/canvas-snapshot-clean` 直接改快照、不改 revision，也不发事件（离线清理，不在本口径内）。

载荷（camelCase，SSE 原样下发）：

```jsonc
{
  "canvasId": "cv_0123456789abcdef",
  "revision": "1794600000123-3fa9",      // 变更后的 revision；run 事件为当前 revision
  "source": "user" | "agent" | "run",
  "actor": "user:7001" | "svc:java-2026a" | "mcp:cursor" | "run:12345",
  "changedNodeIds": ["m_1", "t_2"],      // 新增 / 删除 / 内容或位置或尺寸改动 / 连线增删改涉及的两端；≤200
  "truncated": true,                     // 可选：超过 200 被截断 → 前端按整画布刷新
  "at": 1794600000150,                   // 事件时间（unix 毫秒）
  "requestId": "12345", "runStatus": "succeeded",   // 仅 source=run
  "resync": true                         // 仅网关补发的对账事件（见下），此时 source="unknown"、changedNodeIds=[]
}
```

changedNodeIds 由服务端比较**存储态**快照（运行态已剥离）得出：按节点 / 连线 id 规范化 JSON 比较（键序、数字写法不同不算改）。
同一份差异也写进智能体写类工具的出参 `changed_node_ids`（P0-7「工具结果回传 revision 与节点摘要」）。

配置（canvas-rpc，`deploy/etc/canvas-rpc.yaml`，可选）：

```yaml
Events:
  RedisAddr: 127.0.0.1:46379   # 须与网关 Auth.Session 同一台 Redis（pub/sub 不分库，DB 号无关）；留空不发事件
  RedisPass: ""
Limits:
  RevisionKeep: 20             # 快照历史保留个数；<=0 不记历史
```

发布与历史写入都是**尽力而为**（独立 3 秒时限、与请求取消解耦），失败只记 ERROR（`check=canvas.event_publish` / `canvas.revision_record`），不影响保存结果。

### 11.2 网关 SSE 与降级轮询

| 接口 | 说明 |
|---|---|
| `GET /api/v1/canvas/canvases/:id/events` | SSE。Auth（会话令牌）+ 画布归属（canvas-rpc `GetCanvasRevision`，他人 / 不存在一律 4001，不开流） |
| `GET /api/v1/canvas/canvases/:id/revision` | 当前 revision：`{canvas_id, revision, updated_at, node_count, push, poll_interval_ms}`（统一信封，含驼峰别名）。`push=false` 表示网关没有事件推送 |
| `GET /api/v1/canvas/canvases/:id?revision=<rev>` | 历史快照（§11.3） |

> 路径沿用现有画布资源前缀 `/api/v1/canvas/canvases/:id`（计划里写的 `/api/v1/canvas/{id}` 即此资源）。

SSE 帧：

```
event: hello                       ← 首帧（无 id）
data: {"mode":"push","canvasId":"cv_…","revision":"…","updatedAt":…,"heartbeatMs":15000}

id: 1794600000123-3fa9             ← 画布变更（source=user|agent，或 resync）
event: canvas
data: {…Event…}

event: run                         ← 生成状态（无 id：revision 不变，不影响续传点）
data: {…Event…}

: ping                             ← 心跳，每 15 秒

event: gone                        ← 不要重连：data.code = CANVAS_GONE（画布已删除 / 不再属于你）（SESSION_EXPIRED 仅旧版发出）
event: bye                         ← 可重连：data.code = RECONNECT（单连接最长 30 分钟，带 Last-Event-ID 重连，顺带重新校验归属）| REAUTH（连接令牌失效：刷新令牌后重连）
                                     | CONN_SLOT_UNAVAILABLE（连接名额计数 / 续期失败，按 retry 间隔重连）
```

* **续传**：重连时带 `Last-Event-ID`（EventSource 自动带最后一个 `id`）或首连时带 `?since=<本地编辑起点 revision>`；与当前 revision 不同 → hello 之后先补发一条 `resync:true` 的 canvas 事件（id=当前 revision）。相同则不补发。
* **心跳复核登录态**：每次心跳用与网关 Auth 同一套解析（共用会话 / JWT / dev 的 X-User-Id）复核连接时带的凭证仍有效且仍是同一用户；失效发 `bye · REAUTH` 并关闭（连接沿用建连时的令牌，用户一直在用时其它请求早已刷新，这把旧的过期是常态：前端刷新令牌后重连，真的失效时重连 401、刷新失败才提示登录），查询抖动（Redis 错误）不断开。
* **心跳对账**：每次心跳顺带查一次当前 revision，与已推送的不同就补发 resync 事件——兜住 pub/sub 至多一次丢消息、canvas-rpc 没配 `Events.RedisAddr`、本连接缓冲溢出等情况；画布没了发 `gone`。
* **乱序 / 重复**：同 revision 的重复事件、revision 毫秒更早的事件（多实例发布乱序）不推。
* **并发上限**：每用户同时 ≤`CanvasEvents.MaxConnsPerUser`（默认 5，跨实例：会话 Redis 有序集合 `canvas:sse:conns:{uid}`，名额 3×心跳未续期自动失效）；超出回 `4002 · CANVAS_EVENTS_TOO_MANY`（不开流）。每次心跳用 ZADD（不带 XX）续期，被当作残留清掉的名额会加回；计数或续期出错不放行，发 `bye · CONN_SLOT_UNAVAILABLE` 结束。
* **降级**：网关没有会话 Redis 时，SSE 首帧 `{"mode":"poll","pollUrl":"/api/v1/canvas/canvases/:id/revision","pollIntervalMs":5000}` 后立即结束（附 `retry:` 拉长自动重连间隔）；前端改为按间隔轮询 `/revision`。
* **实现要点**：每个网关实例只占一条 Redis 订阅连接（`canvasevent.Hub` 按画布引用计数 SUBSCRIBE/UNSUBSCRIBE 并扇出）；先订阅再发 hello / 对账，订阅之后的事件不会漏。go-zero 只对 `Accept: text/event-stream` 跳过请求超时——EventSource 自动带；用 fetch 自实现时必须带这个头。nginx 需对该路径关闭缓冲（响应已带 `X-Accel-Buffering: no`）。
* 网关配置（`CanvasEvents`，均可选）：`MaxConnsPerUser: 5`、`HeartbeatSeconds: 15`、`MaxLifetimeMinutes: 30`、`PollIntervalMs: 5000`。

### 11.3 历史快照（三方合并 base）

* 新表 `afv_canvas_revision`（`deploy/sql/Go_V1.0.40__canvas_revision.sql`，可重复执行）：每次 revision 变化记一行存储态快照（gzip），写入后按画布裁剪到最近 `Limits.RevisionKeep`（默认 20）个；唯一键 `(canvas_id, revision)`。
* `GET /api/v1/canvas/canvases/:id?revision=<rev>`：`rev` 为空或等于当前 → 照旧返回最新；否则返回该历史版本（`revision` / `node_count` / `updated_at` 为该版本的值，快照做与当前相同的运行态回填——base 与 theirs 的运行态键一致，合并时自然抵消）。
  超出保留范围（或从未存在、历史没记上、表未建）→ `4001 · reason=CANVAS_REVISION_NOT_RETAINED`，前端放弃合并、提示载入最新版本。
* 归属先按 `afv_canvas (id, user_id, deleted=0)` 校验，他人拿到 revision 也读不到。
* 前端合并建议（W2 实现）：base = 本地编辑起点 revision 的快照，theirs = 最新快照，mine = 本地；按节点 / 连线 id：只一方改的取改的一方，双方都改且不同 → 冲突节点让用户选。忽略运行态键（status/progress/runId/requestId/candidates/…）。

## 12. 服务间入口签名契约（P0-7，Java 按此实现）

### 12.1 入口

| 方法 | 路径 | 入参 / 出参 |
|---|---|---|
| POST | `/internal/v1/canvas/agent/call` | 请求体 `{"name": "<工具名>", "arguments": {…}, "call_id": "<幂等键，必填，≤64 字符>"}`（也接受 OpenAI tool_call 原样，此时 `id` 即 call_id）；用户 id **只在** `X-Svc-User-Id` 头，参数里不得出现 user_id（Schema 拒绝）。缺 call_id → 400 `CALL_ID_REQUIRED`，超长 → 400 `PARAM_INVALID`；`Idempotency-Key` / `X-Request-Id` 头不在签名里，**一律忽略**，不能顶替 call_id |
| GET | `/internal/v1/canvas/agent/tools?format=openai\|anthropic\|mcp` | 工具定义；`X-Svc-User-Id` 可为 `0` |

* 执行完全复用 `RunAgentCall`（canvas-rpc `AgentCall`）：user 隔离（他人画布 `NOT_FOUND`）、revision 乐观锁、`call_id` 幂等回放、按用户限速（与用户入口同一份配额）、审计（`client=svc`、`agent=svc:<keyId>`）、计费（按节点原链路，§6.1）均不变；画布事件 / 快照历史里 `source=agent`、`actor=svc:<keyId>`。
* 不挂 Auth：用户令牌访问不到（无签名一律 401）；网关不透传任何 Bearer。例外在 canvas-rpc 内部：`canvas_cancel_run` 取消 Java 兼容工具的 run、
  `canvas_run_node` 执行 Java 计费的工具节点（放大 / 扩图 / 局部重绘，§6.1）时，会用 canvas-rpc 内存里缓存的**该用户本人**最近一次请求带来的 Bearer 调 Java
  （`Bearers` 缓存，按 user_id 取；画布 / run 已按 user 校验归属，只能操作本人的）；缓存不存在时：取消按现有规则处理（不代用户调用），执行回 `LOGIN_REQUIRED`。
* `canvas_run_node`（不带显式 `run_id`）走 §6 的无人值守去重：同（user、canvas、node、输入指纹含上游实际产物）仍在途、或 120 秒内已成功的 run 直接回放（`replayed=true`、不再扣点）——Java 重试换了 call_id 也不会重复生成。
* actor 防冒充：只有本入口（审计 `client=svc`）写入的 `agent=svc:<keyId>` 才被认作服务调用；MCP / Function Call 的 `X-Agent-Name` / `User-Agent` 是客户端自报，
  去控制字符、截 48 字，自报的 `svc:` 前缀（不分大小写）转义为 `svc_`，canvas-rpc 生成 actor 时再校验一次。
* 不经 jsoncase 改写：网关在 jsoncase 之前存下原始请求体（`canvasagent.PreserveInternalBody`），验签与解析都用原始字节；响应不补驼峰别名。
* `CanvasInternal.Keys` 为空（或没有一把合格：id 须 `[A-Za-z0-9_.-]{1,48}`、secret ≥32 字节）时入口**不注册**并打 WARN。
* 没有会话 Redis（nonce 存储）时：`Mode=dev|test` 退回进程内 nonce（仅开发）；其他模式（pro / pre / rt）入口**不注册**并打 ERROR（`check=canvas_internal.nonce_store`），因为进程内 nonce 在多实例下防不住重放。
* **部署要求**：网关端口（8888）只对内网开放，公网流量只经入口 nginx；nginx 对公网屏蔽 `/internal/`；Java 的 `app.fusion-go.base-url` 指向网关**内网地址**（不经公网域名）。签名本身已防伪，这几条是纵深防御。

### 12.2 请求头与签名

| 头 | 值 |
|---|---|
| `X-Svc-Key-Id` | 密钥 id |
| `X-Svc-Timestamp` | unix **秒**（十进制）；与网关时间相差 >300 秒拒绝 |
| `X-Svc-Nonce` | 16–64 位 `[A-Za-z0-9_-]`，每次请求新生成（建议 UUID 去横线）；同一 key 下 630 秒（= 2×时间窗 + 30 秒，配置更小时以此为下限）内只能用一次（Redis `SETNX canvas:svc:nonce:{keyId}:{nonce}`） |
| `X-Svc-User-Id` | 代哪个用户操作（十进制正整数；tools 可 `0`） |
| `X-Svc-Signature` | `hex(HMAC-SHA256(secret, stringToSign))`，小写 hex |
| `Content-Type` | `application/json` |

```
stringToSign = METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + NONCE + "\n" + USER_ID + "\n" + hex(sha256(BODY))
```

* `METHOD` 大写；`PATH` 为网关收到的 URL 路径，**不含 query 与域名**（`/internal/v1/canvas/agent/call`）；如有反向代理改写前缀，按网关实际收到的路径签。
* `BODY` 为实际发送的原始字节（签完不要再序列化一次）；GET 无 body 时取空串的 sha256（`e3b0c442…b855`）。
* 网关校验顺序：头齐全 / 格式 → key 存在 → 时间窗 → 签名（常量时间比较）→ 占用 nonce（签名通过才占，伪造请求烧不掉真 nonce）。
* 轮换：网关同时配新旧两把 key → Java 切到新 key → 删旧 key。

### 12.3 示例（固定向量，Go 单测 `svcauth.TestKnownVector` 同值）

```
secret    = 0123456789abcdef0123456789abcdef
method    = POST
path      = /internal/v1/canvas/agent/call
timestamp = 1794600000
nonce     = n0nce-0123456789abcdef
userId    = 42
body      = {"name":"canvas_get","arguments":{"canvas_id":"cv_0123456789abcdef"},"call_id":"turn-1:tool-1"}
sha256(body) = ee7cb04668ae374d208ed575d611849df8cfc9f47ecab211f8b153705edbf77b

stringToSign =
POST
/internal/v1/canvas/agent/call
1794600000
n0nce-0123456789abcdef
42
ee7cb04668ae374d208ed575d611849df8cfc9f47ecab211f8b153705edbf77b

X-Svc-Signature = d0764d169799dae9acef87b4961fe1e6e6fb0a2803fab46640c211915f83991d
```

### 12.4 Java 侧伪代码（配置 `app.fusion-go.base-url` / `key-id` / `secret`）

```java
byte[] body = objectMapper.writeValueAsBytes(Map.of("name", tool, "arguments", args, "call_id", callId));
String path = "/internal/v1/canvas/agent/call";
String ts = String.valueOf(Instant.now().getEpochSecond());
String nonce = UUID.randomUUID().toString().replace("-", "");          // 32 位
String sts = "POST\n" + path + "\n" + ts + "\n" + nonce + "\n" + userId + "\n" + hex(sha256(body));
Mac mac = Mac.getInstance("HmacSHA256");
mac.init(new SecretKeySpec(secret.getBytes(UTF_8), "HmacSHA256"));
String sig = hex(mac.doFinal(sts.getBytes(UTF_8)));                       // 小写
HttpRequest req = HttpRequest.newBuilder(URI.create(baseUrl + path))
    .header("Content-Type", "application/json")
    .header("X-Svc-Key-Id", keyId).header("X-Svc-Timestamp", ts).header("X-Svc-Nonce", nonce)
    .header("X-Svc-User-Id", String.valueOf(userId)).header("X-Svc-Signature", sig)
    .POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();          // 发送的必须是签名用的同一份字节
// 重试时必须重新生成 nonce / timestamp / 签名（同 nonce 第二次 = 401 SVC_NONCE_REPLAYED）；call_id 保持不变以获得幂等回放
```

### 12.5 响应与状态码（重试契约）

成功（HTTP 200）：统一信封 `{"code":0,"msg":"ok","data":{"name","call_id","is_error","replayed","result","content"}}`。
`content` 为 `result` 的 JSON 文本，可直接作为 tool 消息回填给模型。**工具业务错误也是 HTTP 200**：`is_error=true`、`result={error_code, message, retryable, retry_after_seconds?, revision?}`
（如 `NOT_FOUND`、`CANVAS_CONFLICT`、`RATE_LIMITED`、`INSUFFICIENT_POINTS`）——交给模型自我修正，是否重试看 `retryable`。

协议级错误：体为 `{"code":<业务码>,"reason":"<原因>","msg":"…","message":"…","data":null}`，HTTP 状态码即重试契约：

| HTTP | code · reason | 含义 | Java 处理 |
|---|---|---|---|
| 401 | 2001 · `SVC_AUTH_MISSING` / `SVC_USER_MISSING` / `SVC_KEY_UNKNOWN` / `SVC_TIMESTAMP_EXPIRED` / `SVC_SIGNATURE_INVALID` / `SVC_NONCE_REPLAYED` | 签名被拒 | 不重试；告警查配置 / 时钟（`SVC_NONCE_REPLAYED` 多为重试复用了 nonce） |
| 400 | 1003 · `PARAM_INVALID` | 请求体不是合法 Function Call，或 call_id 超过 64 字符 | 不重试 |
| 400 | 1002 · `CALL_ID_REQUIRED` | body 缺 call_id | 不重试（修调用方） |
| 413 | 1003 · `PARAM_INVALID` | 请求体 >1MB | 不重试 |
| 404 | — | 入口未注册（网关没配 key）或 `CanvasAgent.Enabled=false` | 不重试；视为功能关闭 |
| 503 | 5001 · `SVC_NONCE_STORE_FAILED` | nonce 存储（Redis）不可用 | 可重试（新 nonce） |
| 500 | 5001 · `SERVER_ERROR` / `SVC_GATEWAY_MISCONFIGURED` | 网关内部错误 | 可重试 |

网关日志：每次拒绝打 `WARN … check=canvas_internal.auth reason=<原因>`（含 key id、user id、来源 IP）。

## 13. MCP 访问令牌（2026-11-14，迁移 `deploy/sql/Go_V1.0.42__canvas_mcp_token.sql`）

外部 MCP 客户端专用的长期令牌，替代把登录令牌交给第三方。

### 13.1 形状与存储

* 明文 `mcp_` + 43 位 base64url（32 字节随机，`app/canvas/mcptoken`）；**只在创建响应里出现一次**，网关响应带 `Cache-Control: no-store`。
* 表 `afv_canvas_mcp_token`：只存 `token_hash = sha256(明文)`（唯一键，256 位熵无需加盐 / 慢哈希）与 `token_prefix`（明文前 12 位，列表里便于识别）；
  `name`（≤50 字）、`scope`、`expires_at`（7 / 30 / 90 天，由库按 `NOW(3)` 计算）、`last_used_at / last_used_ip`、`created_ip`、`revoked_at`（吊销 = 置时间，不物理删）。
* 每人最多 **10 个生效中**（未吊销、未过期）的令牌：创建在事务里 `SELECT … FOR UPDATE` 锁住该用户生效中的行再计数，死锁（同一用户并发创建）整笔重试 ≤3 次；超出 `4002 · MCP_TOKEN_LIMIT`。
* 时间换算沿用画布库约定：DB 会话时区须与 DSN `loc` 一致（`expires_at` 由库算、按 `loc` 读回）。

### 13.2 作用域

| scope | 允许 |
|---|---|
| `canvas_rw` | 全部画布读写工具（含 `canvas_cancel_run` 退点、`canvas_catalog`、`canvas_quote_node`、`canvas_get_run`），**不含** `canvas_run_node` |
| `canvas_rw_generate` | 全部工具（含 `canvas_run_node`，会冻结点数） |

范围外的工具在网关 Caller 里拦下（先于配额，不占配额、不到 canvas-rpc）：工具结果 `is_error=true` + `{error_code:"FORBIDDEN_SCOPE"}`（MCP 为 `isError:true`，不是协议错误）。
网关把 `auth_source=mcp_token` 与 `token_scope` 随 `AgentCallReq` 传给 canvas-rpc，canvas-rpc 在执行前**二次校验**（`check=agent_call.token_scope`）。
`canvas_rw` 令牌的 MCP `tools/list` 与 `GET /agent/tools` **不列**生成类工具。登录令牌与服务间入口（§12）没有作用域限制。

**不借用用户 Bearer 缓存**：`auth_source=mcp_token` 时 canvas-rpc 的 `rememberBearer` 恒返回空——既不记，也不退回 `Bearers` 缓存里该用户网页登录留下的令牌
（否则外部令牌可借网页会话代用户调 Java）。需要用户 Java 令牌的操作（取消运行中的 Java 工具 run、执行放大 / 扩图 / 局部重绘工具节点）直接返回
`is_error` + `MCP_TOKEN_JAVA_TOOL_UNSUPPORTED`（「这个操作需要在网页里登录后使用」），run 不变。`auth_source`：`user`（登录令牌，默认）/ `mcp_token` / `svc`（服务间，照旧可用缓存，§12.1；缓存没有时执行 Java 工具回 `LOGIN_REQUIRED`，§6.1）。
不需要 Java 令牌的工具节点（反推 gateway 计费、裁剪不计费、试穿 studio 按 user_id 计费）MCP 令牌照常可用（`canvas_rw_generate` 作用域）。

**账号禁用**：校验时联查 Java 用户表 `afv_app_user`（`id = user_id AND deleted = 0`，判据与 Java `InternalAuthIntrospectController` 同源）：
`status≠1`（0 = 禁用）或用户不存在 / 已删除 → `401 · MCP_TOKEN_USER_DISABLED`。网关正缓存最长 60 秒，**封禁后最迟 60 秒令牌失效**；恢复启用后负缓存最长 30 秒。

### 13.3 鉴权链路（网关 `canvasagent/mcptoken.go`）

* `POST/GET/DELETE /api/v1/canvas/mcp` 与 `GET /api/v1/canvas/agent/tools`、`POST /api/v1/canvas/agent/call` 挂 `agentAuth`：
  - `Authorization: Bearer mcp_…` → 网关算 sha256，查会话 Redis 缓存 `canvas:mcp_token:{hash}`（正缓存 ≤60 秒且不超过令牌剩余有效期；无效 / 已吊销 / 已过期负缓存 30 秒），
    未命中调 canvas-rpc `VerifyMcpToken(token_hash, client_ip)`（明文不出网关），成功时回写 `last_used_at / last_used_ip`（IP 取 `pkg/clientip`，即约每分钟一次）；
    user 与 Grant（token_id、scope）进 context。失败 → HTTP 401 + `WWW-Authenticate: Bearer error="invalid_token"`，reason `MCP_TOKEN_INVALID` / `MCP_TOKEN_EXPIRED`。
  - **不透传 Bearer**：MCP 令牌不是 Java 认的令牌，也不能进 canvas-rpc 的用户 Bearer 缓存（§12.1 的 Java 取消用的是登录令牌）。
  - 其余 → 照旧 `Auth`（登录态）+ `CanvasBearer`。
* 没有会话 Redis（仅开发）时不缓存，每次查 canvas-rpc。

### 13.4 管理接口（只认登录令牌）

| 方法 | 路径 | 入参 | 出参（统一信封 data，含驼峰别名） |
|---|---|---|---|
| GET | `/api/v1/canvas/mcp-tokens` | — | `{tokens:[Token], active_count, max_active, mcp_path}`（未吊销的，含已过期，新的在前，≤50） |
| POST | `/api/v1/canvas/mcp-tokens` | `{name, expires_in_days: 7\|30\|90, scope: canvas_rw\|canvas_rw_generate}` | `{token: Token, plain_token, mcp_path}` |
| DELETE | `/api/v1/canvas/mcp-tokens/{id}` | — | `{id, revoked:true}`；他人的 / 不存在 → `4001`；重复吊销幂等 |

`Token = {id(string), name, token_prefix, scope, expires_at, last_used_at(0=从未), last_used_ip, created_at, expired}`（时间为 unix 毫秒）。

* 带 `mcp_` 令牌调用管理接口一律 `2005 · FORBIDDEN_SCOPE`（不能用 MCP 令牌管理令牌）；handler 内对带 Grant 的请求再拒一次（纵深防御）。
* **吊销即时生效**：canvas-rpc 置 `revoked_at` 并返回 `token_hash`，网关写 60 秒**墓碑**（负缓存覆盖 `canvas:mcp_token:{hash}`，不只删键）；
  正缓存用 `SET NX` 写入，吊销前已在途、吊销后才返回的校验写不回正缓存。墓碑写失败打 ERROR（`check=mcp_token.invalidate`），最长 60 秒后缓存自然失效。
* canvas-rpc 新增 `CreateMcpToken / ListMcpTokens / RevokeMcpToken / VerifyMcpToken`（proto 只增）。
* 入口受 `CanvasAgent.Enabled` 控制（关闭时 404）。

### 13.5 测试

* `app/canvas/mcptoken`：格式、哈希、前缀、作用域、有效期。
* canvas-rpc `mcp_token_e2e_test.go`（真 MySQL 临时库，结束 DROP）：迁移可重复执行、库里无明文、校验与最后使用回写、他人不可见 / 不可吊销、错误令牌、过期、吊销即失效与幂等、并发创建不超过 10 个、吊销后可再建、他人不受上限影响。
* 网关 `mcptoken_test.go`（miniredis）：MCP 令牌解析与不透传 Bearer、登录令牌兼容、401 与 reason、缓存命中 / 过期 / 不活过令牌、吊销即时失效、MCP 令牌不能管理令牌、吊销他人令牌 NotFound、作用域（Caller 与 MCP tools/call 端到端 `isError` FORBIDDEN_SCOPE）。

### 13.x 按令牌限速（2026-11-14）

在按用户限速（`CallsPerMinute/Day`、`GeneratePerMinute/Day`，同一用户的所有令牌与网页共用）之外，**每个 `mcp_` 令牌独立再计一层每分钟数**：

* 配置 `CanvasAgent.McpTokenCallsPerMinute`（默认 60，全部工具调用）、`CanvasAgent.McpTokenGeneratePerMinute`（默认 10，生成类，另计入总数），`<=0` 不限；只追加字段，旧配置不用改。
* 只对 MCP 令牌生效（登录令牌、服务间入口没有令牌 id，不受此层限制）；按令牌 id 计数，同一用户的另一个令牌互不影响。
* 顺序：作用域（越权不占额度）→ 令牌级（总数 → 生成）→ 用户级（总数 → 生成）。令牌级先扣，一个失控的令牌先被自己的上限挡住，不会把同用户的其它令牌与网页的用户级额度耗光。被拒的调用不到达 canvas-rpc。
* 超出回 `is_error: true`、`error_code: RATE_LIMITED`、`retryable: true`、`retry_after_seconds: 60`，与用户级同形；文案区分为「该令牌的工具调用 / 生成太频繁，请稍后再试」。
* 计数：会话 Redis（`go:quota:canvas_agent:mcp_token:{call|gen}:<令牌id>:m:<分钟>`，多实例共享）；没有 Redis 退回进程内计数并打 ERROR 日志（`check=mcp_token.rate_limit`，多实例各算各的，只适合开发）。Redis 抖动时放行（与 userquota 一致）。

## 14. 快照写入量观测（2026-11-14）

目的：判断「每次保存写一行 `afv_canvas_revision`（gzip，每画布留 20 个）」的写放大是否需要治理。**只观测**：不改写入语义与保留策略；指标 / 日志任何失败都被吞掉（`recover`），不影响保存。实现：`app/canvas/rpc/internal/logic/revision_metrics.go`，接在 `recordRevision` 里。

### 14.1 指标（go-zero `core/metric`，随服务 Prometheus 端点暴露；不带画布 / 用户标签，避免高基数）

| 指标 | 类型 | 标签 | 含义 |
|---|---|---|---|
| `canvas_revision_write_total` | counter | `result`=ok / insert_failed / gzip_failed | 快照历史写入次数与结果 |
| `canvas_revision_raw_bytes` | histogram | `source` | 压缩前（存储态快照）字节数 |
| `canvas_revision_gzip_bytes` | histogram | `source` | 压缩后（入库）字节数 |
| `canvas_revision_pruned_rows_total` | counter | `result`=deleted / failed | 被裁剪删除的行数 / 裁剪失败次数 |
| `canvas_revision_large_total` | counter | `source` | 压缩后超过阈值的写入次数 |

* 桶（字节）：1K、4K、16K、64K、256K、1M、2M（单份上限 2MB）。`source` 只有 user / agent / run 等少数取值。
* 按用户分桶：**未做**（高基数风险）；需要时用下面的 SQL 按 `user_id` 复核，或在指标侧加粗粒度分档（如活跃度档位），不要直接用 user_id 当标签。
* 写放大估算：`rate(canvas_revision_gzip_bytes_sum[1d])`（日写入字节）、`rate(canvas_revision_write_total{result="ok"}[1d])`（日写入次数）；稳态下 `pruned_rows_total` 速率应接近 `write_total` 速率（每画布满 20 后每写一行删一行）。

### 14.2 大快照告警

压缩后 **> 256KB**（`snapshotLargeGzipBytes = 262144`，等于不算）时打 WARN（logx 无 warn 级别，沿用仓库惯例：error 级别输出、消息以 `WARN` 开头）：判据 `check=CANVAS_SNAPSHOT_LARGE`，带 `canvas`、`gzip_bytes`、`raw_bytes`、`threshold`、`source`；同时 `canvas_revision_large_total` +1。该日志每次超阈值的保存都会打一条；若某画布长期偏大且编辑频繁，会较密，可按 canvas id 聚合后再决定是否治理（如降低保留数、差量存储）。

### 14.3 运维 SQL

`tools/ops/canvas-revision-stats/stats.sql`（只读，仅 SELECT，建议在从库执行）：

```bash
mysql -h <host> -u <ro_user> -p fusion_aigc < tools/ops/canvas-revision-stats/stats.sql
```

输出：近 7 天每日行数 / 画布数 / 压缩前后平均与最大 / 压缩比 / 大快照行数；按来源拆分；每画布行数分布；全表占用；单份最大的 Top 20 画布。可改文件头的 `@days`、`@large`。

**口径提醒**：表内每画布只留最近 20 行，所以 SQL 里的「每日行数」只是保留中的行，会低估活跃画布的真实保存次数；真实写入次数与大小分布以 14.1 的指标为准，SQL 用来人工复核大小、压缩比和行数堆积。

### 14.4 测试

`revision_metrics_test.go`：小快照记 raw / gzip 不计 large；阈值边界（等于不算、+1 计 large）；压缩失败只记 raw；写入结果与裁剪行数计数（0 不增、失败单列）。

