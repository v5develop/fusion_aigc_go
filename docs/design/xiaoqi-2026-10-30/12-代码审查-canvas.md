# canvas 模块代码审查结论（2026-10-31，独立 opus 审查，只读）

## 必须修（已派发修复）
1. **run 冻结未完成即可见**：run_ops.go:86-127、repo/run.go:18-21、flow.go:452-473。task_id 未回填时幂等键退化为 `canvas::0`/`canvas-`（run_dispatch.go:139-141）全体共用 → 409；Hold 前被提交 → 点数不足仍白拿结果；Hold 后写库前 ctx 取消 → billing_status=none 免费交付。修：「提交中」状态或插入前生成 task_id/billing_request_id；Hold 完成后一次 CAS 写 billing_status+points+排队；WithoutCancel；兜底 N 分钟仍提交中 → Fail+失败。
2. **canvas-rpc 无 Reclaimer**（canvas.go:43-44）：UNKNOWN/review、Done/Fail 失败全靠 imagegen。修：group.Add(billing.NewReclaimer)，文档写明 review 到期自动退回。
3. **蒙版 SSRF + 解压炸弹**：run_dispatch.go:449-455/485-501/459，mask_url 任意 http、DefaultClient 跟随跳转、image.Decode 无尺寸限制（50000² PNG → 10GB）。修：mask_url 走 media 服务拉取或仅我方桶域名；DecodeConfig 限 ≤4096²；拉入 media 后 request 列只存 rid。
4. **官方模板 element-replace 跑不起来**：seed 里视频模型 n3→n7 image1（首帧口）+ 两张参考图接 image，违反 run_request.go:342-345 首尾帧与参考互斥。修：e2 改 targetHandle video；补「种子快照过 ValidateDoc+buildSpec」测试。
5. **回填只看最近 run 且丢 selectedCandidate**：snapshot.go:191/196-207/253-266，repo/run.go:54-60 MAX(id) 不看状态；selected_index 无处写入。修：状态取最近 run，成品/候选取最近**成功** run；selectedCandidate 不剥离或提供写回接口。
6. **BatchGet 失败整轮跳过超时；Done 结果被忽略**：flow.go:313-329/527-533。gateway 长时间不可用 → 冻结被回收 → 恢复后 CAS 成功交付、Done 失败只记日志、库里 settled 实际未扣。修：BatchGet 失败仍做超时判断；Done 改 Settle 返回错误，ErrGone 写 unsettled/review 并告警。先 CAS 再 Done 顺序保留。

## 建议
- 孤儿上游调用：dispatch Transit 失败、count>1 部分提交失败、排队超时 backend_ref 为空 → 都应 Cancel 已拿到的 call（排队超时可用同幂等键再提交取回 id）。
- tryon/Java 超时与 force-fail 只停止跟踪未 CancelBatch；CancelRun 文案「退回点数」对 Java 不成立。
- ActiveOnNode 先查后插非原子（run_ops.go:70-74）→ 唯一约束或 FOR UPDATE。
- 多实例无租约；BearerCache 按实例。README 写明单实例或 DB 租约。
- ListByStatus 按 updated_at ASC 取 100 且轮询不更新 → 饥饿；无 Bearer 的 Java run 占队头。
- 部分成功 points 整数除法与 settle 不一致。
- 启动自检 Timeouts < Hold 时长；canvas 无 Renew。
- 视频四组同入口同价需产品确认；只有尾帧无首帧应本地拦。
- pollJava 401 不清缓存 token（flow.go:576-589）。
- dataURL 检测：textKeys 任意层级豁免、节点外层/edges/顶层未检查 → 全段扫描。
- 网关 snapshot interface{} 重编码（大整数精度、转义膨胀）→ json.RawMessage 透传并在网关按 2MB 拦；gRPC 4MB 贴近。
- 保存无 if_match 且 force=false 直接覆盖 → 只允许 force。
- ListTemplates 带全量 snapshot ≤200 条会超 4MB → 列表不返回 snapshot。
- workspace_id 未校验归属；蒙版 dataURL 存 request 列（AdminListRuns 100 条全返）。
- 软删画布的排队 run 仍会被提交。
- admin 前缀 path.Clean 修复后补 canvas/prompt-library 前缀测试。
- mock chat 返回 JSON 字符串被当 prompt（仅联调观感）。

## 已核对无问题
video 字段名、首尾帧互斥、vlm uri+sha256、mr://、deadline 不入 hash、labels；越权 user_id 全覆盖；官方模板/提示词写保护；SQL/seed 可重放、第三方 enabled=0。

## 测试盲区
Tick/advanceQueued/pollGateway/pollJava/pollStudio/apply；succeed 分支；endRun 退款条件；RunNode 计费路径；admin 全部；BearerCache/中间件/metadata；maskBox；种子回归；回填「最近失败、上次成功」；网关 2MB/转义/大整数。

- 处理结果（2026-10-31，Java 工具取消）：tool_upscale / tool_outpaint / tool_inpaint 在用户取消、force-fail、超时、画布已删时改为调 Java 现有取消接口 `POST /api/v1/creation/{taskId}/cancel` 真实取消、由 Java 退款（javatool.Cancel + logic/java_cancel.go）；取消时 Java 已成功则用户取消 / 超时 / 画布已删按成功交付、force-fail 置失败并注明已扣费；无有效登录态退化为停止跟踪（error_reason 写明 + WARN `check=canvas.java_cancel_degraded` + 计数）；Java 暂不可达时 Poller 重试最多 5 次；提交期间已结束的孤儿 Java 任务同样取消。CancelRun 文案「退回点数 / 只停止跟踪」已更正。
- 处理结果（2026-10-31，Java 任务号找回）：提交暂时失败、backend_ref 未写上的 Java run，首次发送前把原始请求存入 `backend_request`（Go_V1.0.25 新列），取消类场景按同一幂等键逐字节重放找回 taskUid 再取消（logic/java_request.go）；1003 同键内容不同 → ERROR + 停止跟踪；重放网络错误计入取消重试上限。
- 处理结果（2026-10-31，排队修复四项）：
  1. **Seedream 计费档位**：原先按用户传的尺寸（如 4K）冻结 / 结算，发 gateway 时才改成 2K，多收差价。改为 RunNode 受理阶段按模型组归一（`logic/run_request.go` `normalizeImageSize` / `normalizeRunSize`，口径同 studio `pipeline.NormalizeForGroup`，Seedream 1K / 4K / 空 → 2K），在算 request 快照与幂等比对之前写回 `req.Size`，所以快照、`Charge.Resolution`、gateway 请求都是 2K；`buildSpec` 先归一再校验目录档位（4K 不再报不支持）；`run_dispatch.go` 只为兼容旧快照保留同一函数。`RunNodeResp` 新增 `repeated string warnings = 6`（proto 重新生成），网关 `CanvasRunResp.warnings`（gateway.api / types.go / swagger 同步，无告警为 `[]`），幂等重放也回告警。单测 `queue_fix_test.go`：`TestSeedream4KFreezesAs2K`（计价 SQL 收到的分辨率参数为 2K、冻结额 = 2K 单价）、`TestNormalizeImageSize`。
  2. **UNKNOWN 计费镜像**：review 的 run 预留单到期被 Reclaimer 释放后镜像一直是 review。`logic/run_guard.go` 的 `maintain`（每分钟，与 Renew 同轮）新增 `reconcileBilling`：`repo.ListFinalBillingOpen`（终态 + gateway + `billing_status IN (review, held)` + 近 7 天，走 idx_status 范围扫描，每轮 ≤500）→ `billing.Status`（已有只读 SQL，biz_id 折叠同 pkg/billing，未改 pkg/billing）→ `runBillingMirror`：REFUNDED / EXPIRED / CANCELLED → refunded，CONFIRMED / PARTIALLY_REFUNDED → settled，HELD / 无单不动 → `repo.CASBillingStatus` 以原值 CAS 回写。单测 `TestRunBillingMirror`、`TestReconcileBillingMirror`（三种映射、CAS 未抢到、HELD 不动、超长请求号折叠）、`TestReconcileBillingOff`。
  3. **e2e-canvas.sh 严格化**：source `tests/e2e/lib/strict.sh`（`set -Eeuo pipefail`、ERR trap 打印行号与退出码）；cleanup 首行取 `$?`、经 `e2e_rc` 换算、结尾 `exit` 带回原退出码（bash 3.2 未绑定变量后 `$?=0` 也判为 1）；末尾汇总后 `e2e_finish`；断言失败计入 `E2E_FAIL` 并以 1 退出。「变量紧跟全角字符」检查 0 处，新增行均写 `${var}`。临时注入未定义变量与故意失败的断言实测退出码均为 1，已还原。`AIG_PORT` 改为可覆盖，避免与并行的 studio 栈共用 mock-aigateway。
  4. **Java 任务号找回用例竞态**：Poller 可能在取消前做暂时性重提，提交记录条数不固定。两个用例改为断言「首次之后全是同键重放 / 建任务的非 replay 提交恰好 1 次、全程只有一个 tid、该 tid 的提交都用同一幂等键、只被取消一次」，不再依赖条数精确值。
  - 新增 e2e：Seedream 4K 回 warnings、快照 size=2K、重放同样回告警；BILLING 模式下 Seedream 4K 冻结额 = 2K 单价（15，4K 为 20），UNKNOWN 释放后镜像 review → refunded。本地栈两种模式：普通 93 项 / BILLING 119 项，均 0 失败、退出码 0。
