# Go 服务 MySQL 连接池限额

go-zero `sqlx.NewMysql` 默认 `maxOpenConns=maxIdleConns=64`（每个 DSN 每个进程一个池，同 DSN 的多次 `NewMysql` 共用）。
Go 各服务与 Java、AI gateway 共用一个 MySQL，按默认值最坏会超过 `max_connections`，所以各服务启动时用 `pkg/dbpool` 给共享池显式设限。

## 默认值（每个进程）

| 服务 | MaxOpenConns | MaxIdleConns | 备注 |
| --- | --- | --- | --- |
| canvas-rpc | 30 | 10 | Reclaimer 与 bun 共用同一个池 |
| studio-rpc | 30 | 10 | 同上 |
| gateway-api | 48 | 10 | 仅配了 DataSource 才建池；含视频做同款调度器（三车道 32 + HTTP 8 + 后台 7），deploy yaml 同值 |
| admin-api | 10 | 10 | 仅配了 DataSource 才建池 |
| copywriting-rpc | 10 | 10 | |
| assetlib-rpc | 10 | 10 | |
| imagegen-rpc | 10 | 10 | |
| digitalhuman-rpc | 10 | 10 | |
| note-rpc | 10 | 10 | |
| 合计（每套 Go 进程各 1 个实例） | 168 | | |

连接最长存活 `ConnMaxLifetime` 30 分钟，空闲最长 10 分钟。

## 容量核算

MySQL `max_connections` 须大于：`168 x Go 实例组数 + Java 各角色池之和 + AI gateway 池之和 + 运维/监控余量`。
某服务多副本时按副本数相乘。

## gateway-api 与视频做同款调度器

gateway-api 进程内跑视频做同款的后台调度器（`app/replicate/core/scheduler.go`）：job / 分析 / 入素材库三条车道，
默认并发 16 / 8 / 8（`Replicate.EngineConcurrency` / `AnalysisConcurrency` / `DeliveryConcurrency`，库里
`replicate.engine.*` 覆盖）。每个 worker 同一时刻至多占 1 条连接，最坏同时占 32 条。

**硬上限**：车道并发合计 ≤ `MaxOpenConns − 15`（HTTP 请求 8 + 后台 7：3 个取数协程、1 个在途调用批量查 `InFlightCalls`、
2 个租约心跳、1 个任务中心同步 / 灵感推荐协程）。超了（含运行时库里调大）按比例压低（每车道至少 1）并打
`判据=ENGINE_DB_POOL_CAP` 的 WARN，不会把池打满。默认 32 个 worker 需要池 ≥ 47：`deploy/etc/gateway-api.yaml` 显式配了
`DB.MaxOpenConns: 48`；只用代码默认 40 时会压到约 25 个 worker。调大池要重算上面的 `max_connections`。
租约心跳单条 UPDATE 超时 2 秒、失败重试一次，不长时间占连接。

## 覆盖（可选，无必填项）

yaml 里加可选 `DB` 块；字段缺省或 <=0 用服务默认，`MaxIdleConns` 不会超过 `MaxOpenConns`：

```yaml
DB:
  MaxOpenConns: 20
  MaxIdleConns: 10
  ConnMaxLifetimeSeconds: 1800
```

不要用 `${ENV}` 占位（空值会让 go-zero 的 int 解析失败）。

## 观测

启动时每个服务打一条日志：`check=db.pool_limits`，含 service、maxOpenConns、maxIdleConns、connMaxLifetime。
启动时库不可达则打 error 并后台每 5 秒重试，直到设限成功。
