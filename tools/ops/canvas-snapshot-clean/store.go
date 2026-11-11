package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// target 一张带快照的表。
type target struct {
	Name        string // 命令行里的名字：canvas / template
	Table       string
	NumericID   bool // 主键是 bigint（模板）还是字符串（画布 cv_…）
	HasRevision bool // 有 revision 列（画布）：乐观锁同时比 revision 与 updated_at
}

var targets = []target{
	{Name: "canvas", Table: "afv_canvas", HasRevision: true},
	{Name: "template", Table: "afv_canvas_template", NumericID: true},
}

// record 一行快照。UpdatedAt 是库里格式化好的文本（DATE_FORMAT … %f），写回时原样比较，不经 Go 时区换算。
type record struct {
	ID        string
	Revision  string
	UpdatedAt string
	Snapshot  []byte
}

// updatedAtFmt 微秒精度的 updated_at 文本：datetime(3) 的值按它格式化后与列比较是精确相等的。
const updatedAtFmt = "%Y-%m-%d %H:%i:%s.%f"

type scanOpts struct {
	Since          string // updated_at >= since（空 = 不限）
	IncludeDeleted bool
	Batch          int
}

type store struct{ db *sql.DB }

// startCursor keyset 分页的起点：字符串主键从空串开始，数字主键从 0 开始。
func startCursor(t target) string {
	if t.NumericID {
		return "0"
	}
	return ""
}

func cursorArg(t target, c string) (any, error) {
	if !t.NumericID {
		return c, nil
	}
	n, err := strconv.ParseInt(c, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("游标 %q 不是数字: %w", c, err)
	}
	return n, nil
}

// nextBatch 按主键 keyset 取下一批（id > after，id 升序）。
func (s *store) nextBatch(ctx context.Context, t target, after string, o scanOpts) ([]record, error) {
	arg, err := cursorArg(t, after)
	if err != nil {
		return nil, err
	}
	rev := "''"
	if t.HasRevision {
		rev = "`revision`"
	}
	q := "SELECT `id`, " + rev + ", DATE_FORMAT(`updated_at`, '" + updatedAtFmt + "'), CAST(`snapshot` AS CHAR) FROM `" + t.Table +
		"` WHERE `id` > ?"
	args := []any{arg}
	if !o.IncludeDeleted {
		q += " AND `deleted` = 0"
	}
	if o.Since != "" {
		q += " AND `updated_at` >= ?"
		args = append(args, o.Since)
	}
	q += " ORDER BY `id` LIMIT ?"
	args = append(args, o.Batch)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []record
	for rows.Next() {
		var r record
		var snap sql.NullString
		if err := rows.Scan(&r.ID, &r.Revision, &r.UpdatedAt, &snap); err != nil {
			return nil, err
		}
		r.Snapshot = []byte(snap.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// writeBack 乐观锁写回清理后的快照：只在行仍是读到时的版本（画布：revision + updated_at；模板：updated_at）时生效。
// updated_at 显式写回原值，不触发 ON UPDATE（画布列表按 updated_at 排序，清理不该把画布顶到最前）。
// newRevision 非空时顺带换 revision（-bump-revision：让正开着这张画布的编辑器保存时撞版本冲突、重新载入）。
// 返回 false = 行已变（被保存 / 删除），本行跳过。
func (s *store) writeBack(ctx context.Context, t target, r record, snapshot []byte, newRevision string) (bool, error) {
	id, err := cursorArg(t, r.ID)
	if err != nil {
		return false, err
	}
	q := "UPDATE `" + t.Table + "` SET `snapshot` = CAST(? AS JSON), `updated_at` = `updated_at`"
	args := []any{string(snapshot)}
	if t.HasRevision && newRevision != "" {
		q += ", `revision` = ?"
		args = append(args, newRevision)
	}
	q += " WHERE `id` = ?"
	args = append(args, id)
	if t.HasRevision {
		q += " AND `revision` = ?"
		args = append(args, r.Revision)
	}
	q += " AND `updated_at` = ?"
	args = append(args, r.UpdatedAt)
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
