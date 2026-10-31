package bundb

import (
	"context"
	"errors"
	"sort"

	"github.com/uptrace/bun"
)

// UpdateIf 条件更新（CAS）：按 where 追加条件执行 UPDATE，RowsAffected==1 才返回 ok=true。
// 依赖 MySQL 默认的 RowsAffected =「实际变更行数」语义：值没变的 UPDATE 返回 0。
// 所以 DSN **不得**加 clientFoundRows=true（那会改成「匹配行数」，同值更新也返回 1，CAS 就会误判抢到）。
// 另外 set 里应含会变的列（如 status / updated_at），否则「抢到但值未变」也会返回 false。
// 保留现有 `UPDATE … WHERE id=? AND status=?` 的语义：ok=false 且 err=nil 表示没抢到（状态已变）。
// model 为 bun 模型指针（用于确定表名），set 为 列名->值（按列名排序，保证 SQL 稳定），where 必须传且应含主键与状态条件。
func UpdateIf(ctx context.Context, db bun.IDB, model any, set map[string]any, where func(*bun.UpdateQuery) *bun.UpdateQuery) (bool, error) {
	if where == nil {
		return false, errors.New("bundb: UpdateIf 必须提供 where 条件")
	}
	if len(set) == 0 {
		return false, errors.New("bundb: UpdateIf 必须提供要更新的列")
	}
	q := db.NewUpdate().Model(model)
	q = applySet(q, set)
	res, err := where(q).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// TransitStatus 状态迁移：UPDATE table SET status=?, updated_at=NOW(3), extra... WHERE id=? AND status=?。
// 返回 true 表示抢到迁移；false 表示当前状态不是 from（或记录不存在）。
// extra 为同时更新的其他列（列名->值），不得包含 status / updated_at。
func TransitStatus(ctx context.Context, db bun.IDB, table string, id int64, from, to int, extra map[string]any) (bool, error) {
	if table == "" {
		return false, errors.New("bundb: TransitStatus 缺少表名")
	}
	q := db.NewUpdate().Table(table).
		Set("? = ?", bun.Ident("status"), to).
		Set("? = NOW(3)", bun.Ident("updated_at"))
	q = applySet(q, extra)
	res, err := q.Where("? = ?", bun.Ident("id"), id).
		Where("? = ?", bun.Ident("status"), from).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func applySet(q *bun.UpdateQuery, set map[string]any) *bun.UpdateQuery {
	cols := make([]string, 0, len(set))
	for c := range set {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, c := range cols {
		q = q.Set("? = ?", bun.Ident(c), set[c])
	}
	return q
}
