package repo

import (
	"context"

	"aigc-local-life/app/canvas/model"

	"github.com/uptrace/bun"
)

// TemplateRepo afv_canvas_template。
type TemplateRepo struct{ db bun.IDB }

// ListVisible 用户可见的模板：official = 上架的官方；user = 本人的；all = 两者。按 sort、id。
func (r *TemplateRepo) ListVisible(ctx context.Context, userID int64, scope string) ([]*model.Template, error) {
	var list []*model.Template
	q := r.db.NewSelect().Model(&list).Where("t.deleted = 0")
	switch scope {
	case model.ScopeUser:
		q = q.Where("t.scope = 'user'").Where("t.user_id = ?", userID)
	case "all":
		q = q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("t.scope = 'official' AND t.enabled = 1").WhereOr("t.scope = 'user' AND t.user_id = ?", userID)
		})
	default:
		q = q.Where("t.scope = 'official'").Where("t.enabled = 1")
	}
	err := q.OrderExpr("t.scope ASC, t.sort ASC, t.id DESC").Limit(200).Scan(ctx)
	return list, err
}

// Get 按 id 取（含已删除 / 下架，调用方判断）。
func (r *TemplateRepo) Get(ctx context.Context, id int64) (*model.Template, error) {
	t := new(model.Template)
	return noRows(t, r.db.NewSelect().Model(t).Where("t.id = ?", id).Limit(1).Scan(ctx))
}

// Insert 新建模板。
func (r *TemplateRepo) Insert(ctx context.Context, t *model.Template) error {
	_, err := r.db.NewInsert().Model(t).ExcludeColumn("created_at", "updated_at").Exec(ctx)
	return err
}

// UpdateOfficial 更新官方模板内容。
func (r *TemplateRepo) UpdateOfficial(ctx context.Context, t *model.Template) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableTemplate).
		Set("title = ?", t.Title).Set("description = ?", t.Description).
		Set("snapshot = CAST(? AS JSON)", string(t.Snapshot)).Set("cover_url = ?", t.CoverURL).
		Set("sort = ?", t.Sort).Set("enabled = ?", t.Enabled).
		Where("id = ?", t.ID).Where("scope = 'official'").Where("deleted = 0").Exec(ctx))
	return n == 1, err
}

// SetEnabled 上下架。
func (r *TemplateRepo) SetEnabled(ctx context.Context, id int64, enabled bool) (bool, error) {
	v := 0
	if enabled {
		v = 1
	}
	n, err := affected(r.db.NewUpdate().Table(TableTemplate).Set("enabled = ?", v).
		Where("id = ?", id).Where("deleted = 0").Exec(ctx))
	return n == 1, err
}

// SoftDeleteUser 删除用户自己的模板。
func (r *TemplateRepo) SoftDeleteUser(ctx context.Context, userID, id int64) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableTemplate).Set("deleted = 1").
		Where("id = ?", id).Where("scope = 'user'").Where("user_id = ?", userID).Where("deleted = 0").Exec(ctx))
	return n == 1, err
}

// AdminList 后台模板列表。
func (r *TemplateRepo) AdminList(ctx context.Context, scope string, enabled int32, q string, offset, limit int64) ([]*model.Template, int64, error) {
	var list []*model.Template
	s := r.db.NewSelect().Model(&list).Where("t.deleted = 0")
	if scope != "" {
		s = s.Where("t.scope = ?", scope)
	}
	if enabled >= 0 {
		s = s.Where("t.enabled = ?", enabled)
	}
	if q != "" {
		s = s.Where("t.title LIKE ?", "%"+q+"%")
	}
	n, err := s.OrderExpr("t.scope ASC, t.sort ASC, t.id DESC").Offset(offset).Limit(int64(limit)).ScanAndCount(ctx)
	return list, int64(n), err
}
