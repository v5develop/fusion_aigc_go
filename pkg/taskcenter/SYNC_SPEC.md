# taskcenter 同步规格（现行行为 + 新实现对照）

本文档是「源表 → 任务中心」同步的行为规格，既是 legacy（`legacy.go`，INSERT … SELECT）的现状记录，也是新实现
（`sync_new.go`，普通 SELECT + Go 拼装 + 批量 upsert）必须逐列等价的验收标准。真实库对拍见
`parity_mysql_test.go`，锁行为见 `lock_mysql_test.go`，两者受 `TASKCENTER_PARITY_DSN` 控制（见文末）。
改同步语义前先改本文档，再改 `upsertUpdateClause` / `buildTarget` / legacy 三处。

## 1. 对象与入口

| 对象 | 说明 |
|---|---|
| `Source` | 一张源表（一类记录）的同步配置。`Table/Prefix/Kind/EntryCode/Type/CreateTimeCol/UpdateTimeCol/Trace` 是字面量，其余字段是 **SQL 表达式**（写源表列名，MySQL 求值） |
| `Syncer` | 一个服务里的若干 Source；`Sync(ctx)` 同步一轮，返回所有源失败的合并错误；`y.mu` 串行化整轮 |
| `AsyncSync.Kick()` | 请求路径异步触发：立即返回；同一时刻只跑一轮，跑的期间再 Kick 只记 dirty，本轮结束补跑一轮；每轮失败最多再试 2 次（退避 0.5s / 1s），超时 30s，仍失败只记日志（后台扫描每轮 Sync 兜底）；panic 被 recover |
| 目标表 | `afv_creation_task`（Java 拥有，列见 §3）、`afv_creation_task_text`（正文，1:1）、读 `afv_exec_submission`（提交快照）与 `afv_billing_reservation`（预留单，只读） |
| 调用方 | 各服务 ServiceContext：`taskcenter.New(conn, 10*time.Minute, sources...)`；轮询器每轮 `Sync`，请求路径 `AsyncSync.Kick()`。公开 API 在新实现里不变 |

### Source 字段

| 字段 | 含义 |
|---|---|
| `Table` | 源表名（SQL 里别名 `s`） |
| `Prefix` | task_id / origin_ref 前缀（img、vid、cw、tyo…） |
| `Kind` | 非空 → 筛选 `kind = '<Kind>'`（笔记 / 数字人为空：不筛） |
| `EntryCode` | 写入 `entry_code`，同计费 offer 的 entry_code |
| `Type` | 写入 `type`（image / video / copywriting…；超时口径依赖它，勿写别的） |
| `Include` | 哪些行进任务中心（如 `status IN (2,3,4)`），空 = 全部；与 `Kind` 一起是静态筛选 |
| `Status` | 映射成任务中心状态的表达式（0排队 1处理中 2完成 3失败 4取消 5部分成功） |
| `Result` | 单个成品地址 → `result_url` |
| `Results` | 成品地址数组（JSON）→ `result_urls`，视频类留空 |
| `Cover` | 封面 → `cover_url`；**空 = 用 `Result`** |
| `Fail` | 失败原因 → `error_msg` |
| `Prompt` | 仅在 `RequestPairs` 为空时用：`request_params` 只含 `prompt` |
| `RequestPairs` | 入参快照的 JSON 键值对；非空 → `JSON_OBJECT('entryCode','<EntryCode>', <RequestPairs>)`，空 → `JSON_OBJECT('entryCode','<EntryCode>','prompt',<Prompt|NULL>)` |
| `ExecTaskID` | 执行方任务号表达式 → `execution_task_id`；同时是查提交快照的键 |
| `BillingRequest` | 计费请求号表达式（须与 `billing.Charge.RequestID` 一致）；空 = 不同步计费字段 |
| `ResultText` | 文本产物正文表达式；非空 = 同步写扩展表（媒体类留空） |
| `Trace` | 派生任务轨迹的组件名（见 trace.go，不属于本规格；两种实现下行为一致，仍由 y.mu 之外的异步 goroutine 跑） |
| `CreateTimeCol/UpdateTimeCol` | 源表时间列名，空 = `create_time/update_time`；bun 新模块配 `created_at/updated_at` |

## 2. 派生规则（每个源表行 → 目标行）

`id` 为源表主键，`s.xxx` 为源表列。两种实现的结果必须逐列相同。

| 目标列 | 值 |
|---|---|
| `task_id` | `Ref(Prefix, id)` = `'AiCreation_LF_' + md5("<Prefix>:<id>")`（32 位小写 hex）；legacy 用 MySQL `MD5(CONCAT(...))` 算，新路径用 Go `Ref`，两边靠测试锁死一致 |
| `user_id` | `s.user_id` |
| `type` | `Source.Type` |
| `status` | `Source.Status` 表达式；状态可倒退（如 0→2→0 照搬） |
| `entry_code` | `Source.EntryCode` |
| `request_params` | 见上表 `RequestPairs` / `Prompt` 规则（JSON 列） |
| `execution_task_id` | `Source.ExecTaskID` 表达式；空配置 = NULL；表达式结果为 NULL 也写 NULL（覆盖旧值） |
| `execution_params` | `SELECT input FROM afv_exec_submission WHERE exec_task_id = <ExecTaskID> LIMIT 1`（按 utf8mb4_unicode_ci 比较，大小写不敏感、忽略尾随空格）；没有快照 = NULL；**ODKU 里 NULL 不覆盖旧值**（`COALESCE(VALUES(),列)`）。快照晚于源行最后一次变更才写入时不会补（源行没动就不会被选中），两种实现一致 |
| `billing_status` | 没配 `BillingRequest` → `'NONE'`；配了且无预留单 → `'NONE'`；有预留单按状态映射：HELD→HELD，CONFIRMED→CONFIRMED，PARTIALLY_REFUNDED→PARTIALLY_REFUNDED，REFUNDED→REFUNDED，**CANCELLED→REFUNDED，EXPIRED→REFUNDED**（预扣过期，点数已回余额等同退款），其它→`BILLING_PENDING_REVIEW` |
| `price_points / feature_code / model_id / reservation_no` | 对应预留单的 `price_points / feature_code / model_id / reservation_no`；无预留单 / 没配计费 = NULL |
| 预留单关联 | `biz_type='LOCALLIFE' AND deleted=0 AND biz_id=<键>`，键 = `<user_id>_<BillingRequest>`，长度 > 64 字节时 `LEFT(user_id + '_' + SHA2(<user_id>_<请求号>, 256), 64)`（同 `pkg/billing` 的 bizID）。**同键有多次尝试（uk_biz = biz_type+biz_id+attempt_no）时 legacy 是 `LIMIT 1` 且无 ORDER BY，实测命中 attempt_no 最小者**；新路径 `ORDER BY biz_id, attempt_no, id` 取第一行，对拍已覆盖 |
| `result_url` | `Source.Result`（空配置 = NULL） |
| `result_urls` | `Source.Results`（JSON 列，空配置 = NULL） |
| `cover_url` | `Source.Cover`，空配置则用 `Source.Result` |
| `error_msg` | `Source.Fail`（空配置 = NULL） |
| `is_transferred` | 常量 `1` |
| `deleted` | `s.deleted`；ODKU 里 `GREATEST(目标.deleted, 新值)`：**只会置 1，不会被源表恢复成 0**，也保留 Java 侧置的删除 |
| `create_time / update_time` | 源表的 `CreateTimeCol / UpdateTimeCol`（`update_time` 在 ODKU 里 = 源表更新时间，不是 NOW()） |
| `origin_service` | 常量 `'LOCAL_LIFE'`（Java 侧据此判定只读） |
| `origin_ref` | `<Prefix>:<id>`（Java 后台转发用，md5 不可逆） |

### 不写的列

`orchestration_plan`、`orchestration_topology`、`mode`、`parent_task_id`、`workflow_input` 等一律不写（现有系统的对账 / 驱动 / 恢复任务按它们筛，留空才不会被当成自己的任务重查执行面）。

### ON DUPLICATE KEY UPDATE（唯一键 `uk_task_id`）

更新列与条件的唯一定义是 `upsertUpdateClause`（legacy 与新路径共用）：

```
status, result_url, result_urls, cover_url, error_msg, request_params, execution_task_id,
execution_params = COALESCE(VALUES(execution_params), execution_params),
billing_status, price_points, feature_code, model_id, reservation_no,
deleted = GREATEST(afv_creation_task.deleted, VALUES(deleted)), update_time, origin_service, origin_ref
```

**冲突时不更新**：`user_id / type / entry_code / is_transferred / create_time`（只在首次插入时写）。

### 文本扩展表 `afv_creation_task_text`

配了 `ResultText` 才写；只有 `COALESCE(ResultText,'') <> ''` 的行写：
`(task_id = 同主表, content = ResultText, create_time = 源创建时间, update_time = 源更新时间)`，
ODKU 更新 `content, update_time`（唯一键 `uk_task_id`）。正文后来被清空时**不清扩展表**（保持旧值），两种实现一致。
legacy 里主表与文本表是两条语句（先主后文，文本失败则该源本轮算失败）；新路径同批同事务。

### 提交快照 `afv_exec_submission`（`SubmissionRecorder`，与同步无关的写入）

`RecordSubmission` 按 `exec_task_id` upsert `(task_type, routed_task_type, input, error_msg, updated_at=NOW())`，`input/error_msg` 空串写 NULL。同步只读它的 `input`。

## 3. 选择谓词（哪些源行本轮要同步）

legacy（`Source.where()`）：

```
(<secs>=0 OR s.<update> >= NOW() - INTERVAL <secs> SECOND
   [OR EXISTS (预留单: biz_type='LOCALLIFE' AND deleted=0 AND biz_id=<键> AND update_time >= NOW() - INTERVAL <secs> SECOND)])
AND [kind = '<Kind>'] AND [(<Include>)]
```

- `secs`：该源**还没成功同步过**（进程首轮，或上一轮该源失败）→ 0（全量）；否则 = Syncer 窗口（默认 10 分钟）。
- **OR EXISTS 的存在理由**：预留单状态独立于源行变化（冻结 → 结算 → 过期回收退款），源行没动、只有预留单在窗口内更新时，也要把新的 `billing_status` 同步过去。只有配了 `BillingRequest` 的源才有；它没有 `secs=0` 的短路，且让 `update_time` 索引失效（每轮全扫）。
- 窗口以库时钟 `NOW()` 为准；`update_time >=` 含边界；`deleted` 的源行**仍会被选中**（软删靠 `deleted` 列同步过去）；源表物理删除的行不处理（任务中心行保留）。
- 失败语义：一个源失败只让它自己下一轮全量重来（`synced[i]=false`），不拖累其他源；`Sync` 返回所有失败的 `errors.Join`；成功后 `synced[i]=true`。
- 顺序：按 `Syncer.sources` 顺序逐源同步；每源内部没有显式排序（INSERT … SELECT 按源表读取顺序）。
- 并发：`y.mu` 串行整轮（轮询器与 AsyncSync 并发时不互相等锁 / 死锁）。轨迹派生在 `y.mu` 之外异步。

## 4. 新实现（SyncMode=new）

### 4.1 取数（全部普通 SELECT，RR 下快照读，不加任何锁）

| 步骤 | 语句 | 说明 |
|---|---|---|
| 源行页 | `SELECT s.id, s.user_id, (Status), JSON_OBJECT(...), (ExecTaskID), (Result), (Results), (Cover|Result), (Fail), s.deleted, DATE_FORMAT(create), DATE_FORMAT(update), (计费键), (ResultText) FROM <Table> s WHERE [kind] [Include] AND <游标条件> ORDER BY update, s.id LIMIT <pageRows>` | 表达式仍由 MySQL 求值，语义与 legacy 相同；时间以 `YYYY-MM-DD HH:MM:SS.ffffff` 字符串往返，避免时区 / 精度转换 |
| 预留单 | `... FROM afv_billing_reservation WHERE biz_type='LOCALLIFE' AND deleted=0 AND biz_id IN (?…) ORDER BY biz_id, attempt_no, id` | 每页一次（≤200 键一块），Go 里按 `normKey`（小写、去尾空格，近似 unicode_ci）对键 |
| 提交快照 | `SELECT exec_task_id, input FROM afv_exec_submission WHERE exec_task_id IN (?…)` | 同上 |
| 预留单窗口 | `SELECT biz_id, update_time, id FROM afv_billing_reservation WHERE biz_type='LOCALLIFE' AND deleted=0 AND <游标> ORDER BY update_time, id LIMIT 1000` | **所有带计费的源共用，每轮一次** |

### 4.2 水位与分页（内存，每源一个）

- 状态机：`fresh`（首轮：从头分页读全表，等价 legacy 的 secs=0 全量）→ `catch-up`（一轮读不完：每轮最多 `pageCap`（默认 20）页 × `pageRows`（默认 500）行，下轮从 `(update_time,id)` 游标续读，**不回看**，保证向前推进）→ `steady`（追平：水位 `wm` = 已见最大 update_time，每轮从 `wm - overlap` 起读，`overlap` 默认 = 同步窗口 10 分钟，与 legacy 的窗口覆盖一致，且不依赖本机时钟）。
- 失败：steady 下 `wm` 只在整源成功后推进；catch-up 下游标按页推进（已写成功的页不重写）。
- 摘要去重：回看窗口内同一行每轮都会读到，按 `task_id → 整行摘要（含文本）` 记在内存（上限 5 万，满了清空），**摘要相同不写**；因此稳态下写入量 = 真实变更量。
- **选择内存而非落库**：legacy 的 `synced` 也在内存，进程重启后首轮全量；新实现重启后同样从头分页（只是不锁）。落库要新增状态表，且有「水位比数据新」的跨宕机风险；全量重读成本只是一次分页普通读，upsert 幂等。

### 4.3 预留单驱动（对应 OR EXISTS）

每轮取一次预留单窗口（`biz_type=LOCALLIFE`，从 `rcur - overlap` 起；首轮从「库当前时间 - overlap」起，首轮各源全量读已经按键现读预留单）。
对每个带计费的源：窗口里变过的 biz_id，**本轮源行页已处理过的（covered）跳过**，已处理过 `(源, biz_id, update_time)` 的跳过（`seenResv`，上限 5 万），其余用 `(<计费键表达式>) IN (…)` 反查源行（保持 Kind/Include 筛选）重新同步；没有对应源行（别的服务的预留单）的也记进 `seenResv`，免得每轮白查。预留单游标只在**本轮所有源都成功**后推进。

### 4.4 写入

多行 `INSERT INTO afv_creation_task (…) VALUES (…),(…) ON DUPLICATE KEY UPDATE <upsertUpdateClause>`，每批（默认 100 行）一个短事务，同事务内追加文本表多行 upsert；批内按 `task_id` 排序（统一加锁顺序）；遇 1213（死锁）/ 1205（锁等待超时）整批退避重试（50ms / 150ms / 400ms + 抖动，最多 3 次，日志 `判据=TASKCENTER_UPSERT_RETRY`）。新路径从不对 `afv_billing_reservation`、`afv_exec_submission`、源表加锁。

## 5. SyncMode 与上线

`Syncer.WithMode` 或环境变量 `TASKCENTER_SYNC_MODE`（`legacy` | `new` | `shadow`，缺省 `legacy`，非法值按 legacy 并记错误日志）。所有使用 taskcenter 的服务统一生效。

| 模式 | 行为 |
|---|---|
| `legacy` | 旧 INSERT … SELECT（默认，线上现状，零行为变化） |
| `shadow` | **旧路径写**（锁行为与 legacy 一致）；每源每轮旧路径写完后，新路径**只读只算不写**，对窗口内的行（每源每轮最多一页）读回 `afv_creation_task` / 文本表逐列比对。**没有双写**。目标行 `update_time` 与新算的不同（旧路径尚未追上源行）→ 计入 `stale` 跳过；首次不一致只登记，**下一轮仍不一致**才报 `判据=TASKCENTER_SHADOW_DIFF`（含 source、task_id、逐列 want/got）；`ShadowStats()` 可读计数 |
| `new` | 只走新路径 |

推荐上线顺序：

1. 发版（默认 legacy，无变化）。
2. 灰度实例 `TASKCENTER_SYNC_MODE=shadow`，观察 ≥ 1 天：日志无 `TASKCENTER_SHADOW_DIFF`（有则按 task_id 排查：多半是新增 Source 表达式在 SELECT 里与 INSERT … SELECT 里的类型差异）。shadow 期间锁行为仍是旧的。
3. 切 `new`：先单实例，观察计费确认 / 退款 UPDATE 的锁等待（`innodb_trx` / `data_lock_waits`）与 `TASKCENTER_UPSERT_RETRY` 频率；再全量。回滚 = 把变量改回 `legacy` 重启，两条路径写同一份目标行，随时可切。
4. 稳定后删除 `legacy.go`、`shadow.go` 与 `ModeLegacy/ModeShadow`，`upsertUpdateClause` 保留。

## 6. 对拍与测试

- `TestParityLegacyVsNew`（真实 MySQL，需 `TASKCENTER_PARITY_DSN`，如 `root:123456@tcp(127.0.0.1:43306)/`）：自建一次性库 `taskcenter_parity_<随机>_{old,new}`（用完 DROP），四个源（图片 / 文案 / studio / 笔记，覆盖 Kind、Include、JSON 取数、`created_at/updated_at`、执行任务号、计费、文本产物），多轮夹具（全状态流转、各预留状态、只动预留单、软删、超长键 SHA-256、多次尝试、预留单软删、快照大小写、快照缺失、Java 侧改目标行、状态倒退、正文清空、物理删除），两库跑 legacy / new 后逐列比对（排除自增 id）；再用 shadow 验证零误报并能报出被篡改的列。
- `TestLockBehaviourLegacyVsNew`：阻塞实验（另一会话持有预留单行 / 源表行写锁，会话级 `innodb_lock_wait_timeout=1`）+ `performance_schema.data_locks` 锁清单。
- 单测（无库）：模式解析、计费映射、`buildTarget`、upsert 语句与 legacy 共用同一更新子句、分页 / catch-up / 摘要去重 / 失败不推进水位、1213 / 1205 重试与排序、取数语句不含任何锁子句。
