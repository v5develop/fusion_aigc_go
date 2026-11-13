// Package auditdb 计费 / 对账 / 定点审计的只读查询（sqlc 生成，设计依据：
// docs/design/xiaoqi-2026-10-30/00-需求分流与设计方案.md §9.5）。
//
// 约定：
//   - 只读。这里的查询只服务运营后台的对账报表、定点审计脚本和测试，写操作一律不走 sqlc。
//   - 禁止业务调用。业务逻辑（studio / billing / taskcenter 等）不得 import 本包，
//     例外：pkg/billing/audit.go（上线前计费配置检查，本身就是审计，不在任何业务请求路径上）；
//     否则同一语义会在 bun 模型与这里各定义一份；一张表可以同时被 bun（业务读写）和本包（审计只读）访问。
//   - 与 bundb 共享连接。NewFromSqlConn 取 go-zero SqlConn 的 RawDB，和 bundb.New 用的是同一个 *sql.DB 连接池，不另开池。
//   - 生成文件（db.go、models.go、querier.go、*.sql.go）不要手改：改 db/audit/query/*.sql 或
//     db/audit/schema/*.sql 后执行 make gen-audit。
package auditdb

import "github.com/zeromicro/go-zero/core/stores/sqlx"

// NewFromSqlConn 复用 go-zero 连接池创建只读查询集。
// sqlc 生成的 New(db DBTX) 占了 New 这个名字，所以这里叫 NewFromSqlConn（与设计方案 §9.5 里 bundb 的命名一致）。
func NewFromSqlConn(conn sqlx.SqlConn) (*Queries, error) {
	raw, err := conn.RawDB()
	if err != nil {
		return nil, err
	}
	return New(raw), nil
}
