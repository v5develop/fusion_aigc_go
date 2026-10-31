# studio / assetlib 代码审查结论（2026-10-31，独立 opus 审查，只读）

## 必须修（已派发修复）
1. **排队子任务不续期**：flow_task.go:63-68 只对生成中且 Held 的任务 Renew；KindQuick 预留 30 分钟（billing.go:161-167），reclaim.go:75 到期释放。81 任务排队 >30 分钟 → Settle ErrGone → 上游已付费、用户拿不到图。修：每轮 tick 对所有非终态批次的 Held 子任务批量续期（编排阶段也续）。
2. **先 Settle 后 CAS 竞态**：flow_task.go:178-209 vs admin_force_fail_logic.go:64-73 / 超时失败 flow_task.go:240-250。扣费后任务被改失败，Fail 遇 CONFIRMED 报 ErrGone 不退。修：Settle 前先 CAS 抢「交付中」标记；failTask 发现已 CONFIRMED 时走 refund_confirmed。
3. **提交链路**：submit_common.go:229-293/352-374。(a) SetBillingStatusByBatch 失败只记日志 → 全部任务 billing_status=None → 所有退款/续期跳过；(b) 逐张 Hold 用 RPC ctx，超时/断开后善后全失败；(c) 事务提交后冻结完成前崩溃；(d) 失败那张不 Fail。修：批次先落「提交中」状态（Poller 不扫）冻结完再 CAS 排队；每张 Hold 成功立刻写 billing_status；善后用 context.WithoutCancel + 独立超时；失败那张也 Fail。
4. **权限绕过**：adminauth_middleware.go:33-39 用原始 r.URL.Path 前缀匹配，go-zero 路由 path.Clean 后匹配；`/api/v1//studio/admin/...` 或 `/api/v1/local-life/../studio/admin/...` 用 local_life 权限操作 studio/资产库。修：先 path.Clean，前缀按 prefix+"/" 或相等；补测试。
5. **Seedream 计费档位 vs 实际**：submit_common.go:315-323 按用户选档计费，gateway.go:72-87 归一到 2K。修：validate 阶段按 ModelGroup 归一，快照/Charge/genParams 都用归一值并回告警。
6. **重做后批次拉回失败**：admin_retry_task_logic.go:78-86 UpdateStatus 失败只记日志，任务永卡排队、点数冻结到过期。修：失败则任务 CAS 回原状态并 Fail，报错；或 Poller 兜底扫「批次终态但有排队子任务」。
7. **studio-rpc 不跑 Reclaimer**（仅 imagegen.go:48）：漏退/UNKNOWN 全靠 imagegen 进程。修：studio-rpc 也启 Reclaimer（release 是 CAS，多实例安全）；文档写清 UNKNOWN=过期自动退回。

## 建议（部分已派发）
- flow_task.go:44-47 无调用号且超时直接失败：应先用同幂等键再 Submit 拿回 call_id 再 Cancel。
- Cancel 返回 SUCCEEDED 时应走 deliver 不退款（flow_task.go:86-93、admin_force_fail:74-80）。
- 自动重试 sleep 占 semPoll；重开后 CAS 失败应 Cancel 新调用（flow_task.go:106-131）。
- 编排重试计数取 step_log 历史累计，后台重做后首个可重试错误即整批失败（flow_batch.go:141）。
- 取消在冻结未完成时不退款（query_common.go:144-158），随第 3 条一并解决。
- localtask retry/force-fail 入口对 studio 任务只校验 local_life:update；前缀硬编码。
- 多实例：在途标记只在进程内，扩容会重复调 sidecar/gateway → README 写明单实例或改 DB 租约。
- studio_reconcile.sql `billing_status NOT IN (0,4)` 排除了第 3 条造成的行；应按 billing_request_id 非空参与。
- rewrite 调用不带 deadline/max_cost（flow_task.go:382-392）。
- assetlib common.go:92-105 AddVersion 等 checkURL 不做归属校验（外链可存）；tag.go:102-106 Delete 不带 user 条件。
- sidecar /api/reverse/analyze（:3743-3762）无 try/except，异常断连。
- aigateway lenientUnescape 与 Python unquote 对非法转义处理不同（当前碰不到，补注释/测试）。
- bundb CAS 依赖「变更行数」语义，DSN 加 clientFoundRows 会变。

## 测试盲区
pollTask 各分支、deliver（转存失败/ErrGone/CAS 失败）、failTask refund 条件、onTerminal actRetry、orchestrate/failBatch/errAborted、admin retry/force-fail 全文件、submit Hold 中途 ctx 取消、permFor 不规范路径、auditdb 折叠分支。
