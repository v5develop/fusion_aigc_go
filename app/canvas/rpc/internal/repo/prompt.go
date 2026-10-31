package repo

import (
	"context"

	"aigc-local-life/app/canvas/model"

	"github.com/uptrace/bun"
)

// PromptRepo afv_prompt_library_item。
type PromptRepo struct{ db bun.IDB }

// PromptFilter 列表筛选。
type PromptFilter struct {
	UserID                    int64
	Vertical, Group, Model, Q string
	Offset, Limit             int64
}

func (f PromptFilter) where(q *bun.SelectQuery) *bun.SelectQuery {
	if f.Group != "" {
		q = q.Where("p.group_key = ?", f.Group)
	}
	if f.Model != "" {
		q = q.Where("JSON_CONTAINS(p.models, JSON_QUOTE(?))", f.Model)
	}
	if f.Q != "" {
		like := "%" + f.Q + "%"
		q = q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("p.title LIKE ?", like).WhereOr("p.prompt LIKE ?", like).WhereOr("CAST(p.tags AS CHAR) LIKE ?", like)
		})
	}
	return q
}

// ListVisible 用户可见的提示词：官方（启用）+ 本人。vertical=mine 只看本人，ecommerce/general 只看官方，空=两者。
// 排序：官方在前按 sort，本人按最近创建在前。
func (r *PromptRepo) ListVisible(ctx context.Context, f PromptFilter) ([]*model.PromptItem, int64, error) {
	var list []*model.PromptItem
	q := r.db.NewSelect().Model(&list).Where("p.deleted = 0")
	switch f.Vertical {
	case model.VerticalMine:
		q = q.Where("p.user_id = ?", f.UserID)
	case "":
		q = q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("p.user_id IS NULL AND p.enabled = 1").WhereOr("p.user_id = ?", f.UserID)
		})
	default:
		q = q.Where("p.user_id IS NULL").Where("p.enabled = 1").Where("p.vertical = ?", f.Vertical)
	}
	q = f.where(q).OrderExpr("(p.user_id IS NULL) DESC, p.sort ASC, p.created_at DESC, p.id DESC").
		Offset(f.Offset).Limit(f.Limit)
	n, err := q.ScanAndCount(ctx)
	return list, int64(n), err
}

// Get 按 id 取（未删除）。
func (r *PromptRepo) Get(ctx context.Context, id int64) (*model.PromptItem, error) {
	p := new(model.PromptItem)
	return noRows(p, r.db.NewSelect().Model(p).Where("p.id = ?", id).Where("p.deleted = 0").Limit(1).Scan(ctx))
}

// CountByUser 用户自己的条数（上限校验）。
func (r *PromptRepo) CountByUser(ctx context.Context, userID int64) (int64, error) {
	n, err := r.db.NewSelect().Model((*model.PromptItem)(nil)).Where("p.user_id = ?", userID).Where("p.deleted = 0").Count(ctx)
	return int64(n), err
}

// Insert 新建。
func (r *PromptRepo) Insert(ctx context.Context, p *model.PromptItem) error {
	_, err := r.db.NewInsert().Model(p).ExcludeColumn("created_at", "updated_at").Exec(ctx)
	return err
}

// UpdateContent 改正文类字段。userID>0 时只改本人的；userID=0 只改官方的（后台）。
func (r *PromptRepo) UpdateContent(ctx context.Context, p *model.PromptItem, userID int64) (bool, error) {
	q := r.db.NewUpdate().Table(TablePrompt).
		Set("title = ?", p.Title).Set("prompt = ?", p.Prompt).
		Set("vertical = ?", p.Vertical).Set("group_key = ?", p.GroupKey).
		Set("models = CAST(? AS JSON)", jsonOrNull(p.Models)).Set("tags = CAST(? AS JSON)", jsonOrNull(p.Tags)).
		Set("source = ?", p.Source).Set("sort = ?", p.Sort).Set("enabled = ?", p.Enabled).
		Set("updated_at = NOW(3)").Where("id = ?", p.ID).Where("deleted = 0")
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	} else {
		q = q.Where("user_id IS NULL")
	}
	n, err := affected(q.Exec(ctx))
	return n == 1, err
}

// SoftDeleteUser 删除本人的。
func (r *PromptRepo) SoftDeleteUser(ctx context.Context, userID, id int64) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TablePrompt).Set("deleted = 1").
		Where("id = ?", id).Where("user_id = ?", userID).Where("deleted = 0").Exec(ctx))
	return n == 1, err
}

// SetEnabled 启停（后台，只对官方）。
func (r *PromptRepo) SetEnabled(ctx context.Context, id int64, enabled bool) (bool, error) {
	v := 0
	if enabled {
		v = 1
	}
	n, err := affected(r.db.NewUpdate().Table(TablePrompt).Set("enabled = ?", v).
		Where("id = ?", id).Where("user_id IS NULL").Where("deleted = 0").Exec(ctx))
	return n == 1, err
}

// AdminList 后台列表：scope official / user / all。
func (r *PromptRepo) AdminList(ctx context.Context, scope string, enabled int32, f PromptFilter) ([]*model.PromptItem, int64, error) {
	var list []*model.PromptItem
	q := r.db.NewSelect().Model(&list).Where("p.deleted = 0")
	switch scope {
	case "user":
		q = q.Where("p.user_id IS NOT NULL")
	case "all":
	default:
		q = q.Where("p.user_id IS NULL")
	}
	if f.Vertical != "" {
		q = q.Where("p.vertical = ?", f.Vertical)
	}
	if enabled >= 0 {
		q = q.Where("p.enabled = ?", enabled)
	}
	n, err := f.where(q).OrderExpr("(p.user_id IS NULL) DESC, p.sort ASC, p.id DESC").
		Offset(f.Offset).Limit(f.Limit).ScanAndCount(ctx)
	return list, int64(n), err
}

func jsonOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
