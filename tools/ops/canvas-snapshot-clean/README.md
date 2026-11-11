# canvas-snapshot-clean

清理画布快照里被 `pkg/jsoncase` 中间件补出来的下划线副本。

## 背景

9bfa445 之前，jsoncase 改写请求体时会递归下钻自由格式字段（`snapshot`），把快照节点 `data` 里白名单内的 camel 键补出下划线副本并随保存落库，例如：

```json
{"runId": "run_new", "run_id": "run_old", "requestId": "r_9", "request_id": "r_5"}
```

快照内部字段以前端原样的 camelCase 为准（`docs/api/api-canvas.md` 1.4）。服务端保存时只剥离 camel 的运行态键（`runId` / `requestId` …），所以下划线副本一直留在库里、不再更新，值可能已经陈旧。9bfa445 修好中间件后不会再产生新的副本，这个工具负责清理存量。

涉及的表：`afv_canvas.snapshot`（画布）、`afv_canvas_template.snapshot`（官方 / 用户模板）。

## 规则

遍历整棵 JSON 树（节点 `data`、嵌套对象、数组里的对象都算）。对每个对象，下划线键只有**同时**满足下面三条时才删除：

1. 键名在 jsoncase 白名单里（`pkg/jsoncase/known_gen.go` 的 `knownSnake`，通过 `jsoncase.IsKnownSnake` 读，与中间件同一份）；
2. 同一对象里还有它的 camel 孪生键（`jsoncase.ToCamel`，与中间件同一函数）；
3. 这个 camel 键经 `jsoncase.ToSnake` 正好变回该键名（`image_2k`、`_id`、`a__b` 这种反推不回去的组合不可能是中间件补的）。

- 没有 camel 孪生的下划线键一律保留，保留的始终是 camel 键。
- 快照里的下划线键都不是权威：服务端只读快照里的 camel 键（`runId`、`sourceHandle` 等），工具节点的 `params` 是运行时由前端从节点 camel 字段现拼、随 run 请求提交的，不从快照读；前端节点 `data` 一律 camelCase。所以 camel 孪生在时删下划线那份是安全的。
- 值和 camel 不一样也照删（camel 是前端在维护的那份），报告里会标「值不同」，方便抽查。
- 副本本身是对象、里面还有副本时只报外层一条；camel 值里的嵌套副本照常删。

## 用法

```bash
# 1) dry-run（默认）：只读，打印每行要删的 JSON 路径，以及值是否不同
go run ./tools/ops/canvas-snapshot-clean -dsn 'user:pass@tcp(host:3306)/fusion_aigc'

# 只看某段时间保存过的、只扫画布、先抽 500 行
go run ./tools/ops/canvas-snapshot-clean -dsn '…' -tables canvas -since '2026-10-25 00:00:00' -limit 500

# 2) 真写：先备份再写回
go run ./tools/ops/canvas-snapshot-clean -dsn '…' -apply -backup /data/backup/canvas-snap-20261111.jsonl

# 3) 再跑一次 dry-run，确认 would_fix=0（conflict 的行此时会被重新扫到）
```

也可以 `go build -ldflags="-s -w" -o canvas-snapshot-clean ./tools/ops/canvas-snapshot-clean` 后拷到能连库的机器上跑。DSN 也可以放在环境变量 `CANVAS_CLEAN_DSN` 里，避免出现在 shell 历史中。

| 参数 | 默认 | 说明 |
|---|---|---|
| `-dsn` | `$CANVAS_CLEAN_DSN` | MySQL DSN（不需要 `parseTime`） |
| `-tables` | `canvas,template` | 要扫的表：`canvas` = `afv_canvas`，`template` = `afv_canvas_template` |
| `-batch` | `200` | 每批读多少行（按主键 keyset 分页：`id > 上一批最后一个 id ORDER BY id LIMIT n`） |
| `-limit` | `0` | 每张表最多扫多少行，0 表示不限 |
| `-since` | 空 | 只扫 `updated_at >= since` 的行，格式 `2006-01-02 15:04:05` |
| `-include-deleted` | `false` | 逻辑删除的行也一起清理 |
| `-apply` | `false` | 真写库；不加就是 dry-run |
| `-backup` | `./canvas-snapshot-backup-<时间>.jsonl` | apply 时的备份文件，必须是不存在的新文件 |
| `-bump-revision` | `false` | 清理画布时顺带更换 `revision`（见下文「正在编辑的画布」） |

输出示例：

```
ROW table=afv_canvas id=cv_3f9a… removed=2 differed=1 result=would_fix
  - $.nodes[3].data.request_id  值相同
  - $.nodes[3].data.run_id  值不同（副本已陈旧，保留 runId）
== DRY-RUN（未写库，加 -apply 才写） 扫描 1830 行，有副本 214 行，删除键 431 个（其中值不同 97 个）；clean=1616 would_fix=214
```

`result` 的取值：`clean` 没有副本 / `would_fix` dry-run 下将清理 / `fixed` 已写回 / `conflict` 写回时这一行已经变了，已跳过 / `bad_json` 快照解析不了，没动 / `error` 备份或写库出错。只要有 `error`、`conflict` 或 `bad_json`，退出码就是 1。

## 安全设计

- **默认 dry-run**：不加 `-apply` 只有 SELECT。
- **先备份后写**：每一批先把要改的行的原快照写进备份文件（JSON Lines：`table` / `id` / `revision` / `updated_at` / `snapshot`），`fsync` 成功后才写回这一批；备份写失败（磁盘满等），这一批一行都不写并**中止整次运行**（退出码 1），修好后换一个新备份文件重跑即可。备份文件用 `O_EXCL` 新建，不会覆盖上一次的备份。
- **乐观锁**：画布 `UPDATE … WHERE id=? AND revision=? AND updated_at=?`，模板 `UPDATE … WHERE id=? AND updated_at=?`。`updated_at` 由库按 `DATE_FORMAT(…, '%Y-%m-%d %H:%i:%s.%f')` 读成文本后原样回比，不经过 Go 时区换算。扫描之后被用户保存或删除过的行影响行数为 0，记为 `conflict` 跳过，重跑时按新内容重新判断。
- **不打乱列表顺序**：写回时 `updated_at = updated_at`，不会触发 `ON UPDATE`，画布列表（按 `updated_at` 倒序）不会因为清理而变动。
- **字节语义不变**：只重写确实有副本的行；数字按原文保留（`UseNumber`，大整数不丢精度），不做 HTML 转义（`<` `>` `&` 原样保留）；对象键序由 MySQL JSON 列自行规范化，语义不变。
- **连接字符集固定 utf8mb4**：快照以文本读出、改完再写回，工具会忽略 DSN 里的 `charset=` / `collation=`，强制 `utf8mb4`，避免 emoji 等字符在读出时被替换成 `?` 后写回。
- **幂等**：清理后的快照里已经没有副本，重复执行不会再改动。

## 正在编辑的画布

默认不改 `revision`，用户无感知。代价是：清理时如果有人正开着这张画布，他的编辑器内存里还是带副本的旧快照，下次自动保存会原样存回去（jsoncase 已修复，不会再产生**新的**副本）。对此有两种处理：

- 在低峰期执行，执行完隔一段时间再跑一次（幂等，只会处理被存回去的那几张）；
- 或者加 `-bump-revision`：清理时同时换一个新的 `revision`，正在编辑的人下次保存会碰到版本冲突（4002 `conflict=revision`），前端会提示「载入最新版本 / 覆盖保存」。注意：用户如果选「覆盖保存」，旧快照仍然会被写回去。

## 恢复

备份每行就是一条原快照，按 `table` + `id` 写回即可。例如用 jq 生成 SQL（快照走 base64，避免转义问题），人工审核后再执行：

```bash
jq -r '"UPDATE `\(.table)` SET `snapshot` = CAST(CONVERT(FROM_BASE64(\"\(.snapshot|tojson|@base64)\") USING utf8mb4) AS JSON), `updated_at` = `updated_at` WHERE `id` = \"\(.id)\";"' \
  canvas-snapshot-backup-xxx.jsonl > restore.sql
```

需要 jq ≥ 1.7：更早的版本会把大整数转成浮点，丢失精度。恢复会覆盖用户在清理之后做的保存，只建议在确认清理出错时针对个别行使用。
