package bundb

import (
	"context"

	"github.com/uptrace/bun"
)

// RunInTx 在事务里执行 fn：fn 返回 nil 提交，返回 error 或 panic 回滚。是 bun.DB.RunInTx 的薄封装。
func RunInTx(ctx context.Context, db *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error {
	return db.RunInTx(ctx, nil, fn)
}
