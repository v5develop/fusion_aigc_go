package bundb

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// Upsert MySQL 的 INSERT … ON DUPLICATE KEY UPDATE。
// 注意：这不是 PG 的 ON CONFLICT … EXCLUDED 语法，MySQL 不支持，也不能指定冲突列，
// 冲突由表上的主键 / 唯一索引决定。
//
// conflictCols 仅用于文档与校验（标明命中哪个唯一键），不会出现在 SQL 里；
// 为防止误改唯一键，updateCols 不得与之重叠。
// updateCols 生成 `col = VALUES(col)`，不能为空。
func Upsert(ctx context.Context, db bun.IDB, model any, conflictCols []string, updateCols []string) error {
	if len(updateCols) == 0 {
		return errors.New("bundb: Upsert 必须提供 updateCols")
	}
	skip := make(map[string]struct{}, len(conflictCols))
	for _, c := range conflictCols {
		skip[c] = struct{}{}
	}
	q := db.NewInsert().Model(model).On("DUPLICATE KEY UPDATE")
	for _, c := range updateCols {
		if _, bad := skip[c]; bad {
			return fmt.Errorf("bundb: Upsert 的 updateCols 不能包含冲突列 %q", c)
		}
		q = q.Set("? = VALUES(?)", bun.Ident(c), bun.Ident(c))
	}
	_, err := q.Exec(ctx)
	return err
}
