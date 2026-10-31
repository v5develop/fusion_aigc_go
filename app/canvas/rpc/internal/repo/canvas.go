package repo

import (
	"context"
	"encoding/json"

	"aigc-local-life/app/canvas/model"

	"github.com/uptrace/bun"
)

// CanvasRepo afv_canvas。
type CanvasRepo struct{ db bun.IDB }

// Insert 新建画布（id 由调用方生成）。
func (r *CanvasRepo) Insert(ctx context.Context, c *model.Canvas) error {
	_, err := r.db.NewInsert().Model(c).ExcludeColumn("created_at", "updated_at").Exec(ctx)
	return err
}

// Get 按 id 取（含已删除；调用方校验 user_id 与 deleted）。
func (r *CanvasRepo) Get(ctx context.Context, id string) (*model.Canvas, error) {
	c := new(model.Canvas)
	return noRows(c, r.db.NewSelect().Model(c).Where("c.id = ?", id).Limit(1).Scan(ctx))
}

// GetOwned 取用户自己的、未删除的画布。
func (r *CanvasRepo) GetOwned(ctx context.Context, userID int64, id string) (*model.Canvas, error) {
	c := new(model.Canvas)
	return noRows(c, r.db.NewSelect().Model(c).Where("c.id = ?", id).Where("c.user_id = ?", userID).
		Where("c.deleted = 0").Limit(1).Scan(ctx))
}

// ListByUser 用户的画布（不含快照），按 updated_at 倒序。
func (r *CanvasRepo) ListByUser(ctx context.Context, userID int64, offset, limit int64) ([]*model.Canvas, int64, error) {
	var list []*model.Canvas
	q := r.db.NewSelect().Model(&list).
		Column("id", "user_id", "workspace_id", "title", "revision", "node_count", "cover_rid", "cover_url", "created_at", "updated_at").
		Where("c.user_id = ?", userID).Where("c.deleted = 0").
		OrderExpr("c.updated_at DESC, c.id DESC").Offset(offset).Limit(int64(limit))
	n, err := q.ScanAndCount(ctx)
	return list, int64(n), err
}

// SaveArgs 一次保存。
type SaveArgs struct {
	ID, NewRevision string
	UserID          int64
	Snapshot        json.RawMessage
	NodeCount       int32
	CoverURL        string
	Title           *string // nil=不改
	IfMatch         string  // 非空时 CAS：WHERE revision = IfMatch
}

// Save 写快照（乐观锁）：ok=false 表示 revision 已变（或画布不存在 / 已删除）。
func (r *CanvasRepo) Save(ctx context.Context, a SaveArgs) (bool, error) {
	q := r.db.NewUpdate().Table(TableCanvas).
		Set("snapshot = CAST(? AS JSON)", string(a.Snapshot)).
		Set("revision = ?", a.NewRevision).
		Set("node_count = ?", a.NodeCount).
		Set("cover_url = ?", nullable(a.CoverURL)).
		Set("updated_at = NOW(3)")
	if a.Title != nil {
		q = q.Set("title = ?", *a.Title)
	}
	q = q.Where("id = ?", a.ID).Where("user_id = ?", a.UserID).Where("deleted = 0")
	if a.IfMatch != "" {
		q = q.Where("revision = ?", a.IfMatch)
	}
	n, err := affected(q.Exec(ctx))
	return n == 1, err
}

// SoftDelete 逻辑删除；返回是否删到。
func (r *CanvasRepo) SoftDelete(ctx context.Context, userID int64, id string) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCanvas).Set("deleted = 1").Set("updated_at = NOW(3)").
		Where("id = ?", id).Where("user_id = ?", userID).Where("deleted = 0").Exec(ctx))
	return n == 1, err
}

// Gone 这些画布里已删除（或不存在）的 id（轮询器取消已删画布的排队 run 用）。
func (r *CanvasRepo) Gone(ctx context.Context, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var alive []string
	err := r.db.NewSelect().Table(TableCanvas).Column("id").Where("id IN (?)", bun.In(ids)).Where("deleted = 0").Scan(ctx, &alive)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(alive))
	for _, id := range alive {
		live[id] = true
	}
	for _, id := range ids {
		if !live[id] {
			out[id] = true
		}
	}
	return out, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
