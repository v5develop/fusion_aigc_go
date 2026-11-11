# Go 服务 MySQL 连接池限额

go-zero `sqlx.NewMysql` 默认 `maxOpenConns=maxIdleConns=64`（每个 DSN 每个进程一个池，同 DSN 的多次 `NewMysql` 共用）。
Go 各服务与 Java、AI gateway 共用一个 MySQL，按默认值最坏会超过 `max_connections`，所以各服务启动时用 `pkg/dbpool` 给共享池显式设限。

## 默认值（每个进程）

| 服务 | MaxOpenConns | MaxIdleConns | 备注 |
| --- | --- | --- | --- |
| canvas-rpc | 30 | 10 | Reclaimer 与 bun 共用同一个池 |
| studio-rpc | 30 | 10 | 同上 |
| gateway-api | 20 | 10 | 仅配了 DataSource 才建池 |
| admin-api | 10 | 10 | 仅配了 DataSource 才建池 |
| copywriting-rpc | 10 | 10 | |
| assetlib-rpc | 10 | 10 | |
| imagegen-rpc | 10 | 10 | |
| digitalhuman-rpc | 10 | 10 | |
| note-rpc | 10 | 10 | |
| 合计（每套 Go 进程各 1 个实例） | 140 | | |

连接最长存活 `ConnMaxLifetime` 30 分钟，空闲最长 10 分钟。

## 容量核算

MySQL `max_connections` 须大于：`140 x Go 实例组数 + Java 各角色池之和 + AI gateway 池之和 + 运维/监控余量`。
某服务多副本时按副本数相乘。

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
