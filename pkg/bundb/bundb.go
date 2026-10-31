// Package bundb 新模块统一的 bun 数据层入口（设计依据：docs/design/xiaoqi-2026-10-30/00-需求分流与设计方案.md §9.5）。
//
// 使用边界：
//   - 仅新模块使用；老模块继续用 goctl 生成的 model，不迁移。
//   - bun 只用于新表（afv_studio_*、afv_asset_ext 等）；读 Java 共享表可以只读映射。
//   - 写 Java 共享表（计费 / 任务中心 / 资产）仍走现有 pkg（billing、taskcenter、asset），不在 bun 里重写。
//   - 计费、对账、定点审计类查询用 sqlc（pkg/auditdb），不走本包。
//
// 连接池与 go-zero sqlx 共用（SqlConn.RawDB），不会另开第二个池；
// 但 bun 路径绕过了 sqlx 的慢日志 / 熔断 / metrics，所以统一挂 QueryHook 补上日志。
package bundb

import (
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DefaultSlowThreshold 慢查询阈值，与 go-zero sqlx 默认值一致。
const DefaultSlowThreshold = 500 * time.Millisecond

type options struct {
	slowThreshold time.Duration
	logSQL        bool
}

// Option 配置 New 的可选项。
type Option func(*options)

// WithSlowThreshold 设置慢查询阈值，d <= 0 时忽略。默认 500ms。
func WithSlowThreshold(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.slowThreshold = d
		}
	}
}

// WithLogSQL 为 true 时逐条输出 debug 日志；默认 false，只记慢查询和错误。
func WithLogSQL(on bool) Option {
	return func(o *options) { o.logSQL = on }
}

func buildOptions(opts []Option) options {
	o := options{slowThreshold: DefaultSlowThreshold}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}

// New 复用 go-zero 的连接池创建 *bun.DB（MySQL 方言），并挂上日志 QueryHook。
func New(conn sqlx.SqlConn, opts ...Option) (*bun.DB, error) {
	raw, err := conn.RawDB()
	if err != nil {
		return nil, err
	}
	o := buildOptions(opts)
	db := bun.NewDB(raw, mysqldialect.New())
	db.AddQueryHook(newHook(o.slowThreshold, o.logSQL))
	return db, nil
}
