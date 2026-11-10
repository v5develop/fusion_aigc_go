package repo

import (
	"context"
	"errors"
	"time"

	"aigc-local-life/app/assetlib/model"

	"github.com/uptrace/bun"
)

// TagRepo afv_asset_tag + afv_asset_tag_link。
type TagRepo struct{ db bun.IDB }

// TagRow 标签 + 关联的有效资产数（未删除、非草稿，与列表口径一致）。
type TagRow struct {
	model.Tag  `bun:",extend"`
	AssetCount int64 `bun:"asset_count,scanonly"`
}

// assetCountExpr 每个标签挂着的未删除资产数。
const assetCountExpr = "(SELECT COUNT(*) FROM afv_asset_tag_link AS l JOIN afv_asset AS a ON a.id = l.asset_id AND a.deleted = 0 AND (a.status IS NULL OR a.status <> 0) WHERE l.tag_id = t.id) AS asset_count"

// List 用户的全部标签（按名字排序）。
func (r *TagRepo) List(ctx context.Context, userID int64) ([]*TagRow, error) {
	var out []*TagRow
	err := r.db.NewSelect().Model(&out).ColumnExpr("t.id, t.user_id, t.name, t.created_at").ColumnExpr(assetCountExpr).
		Where("t.user_id = ?", userID).OrderExpr("t.name ASC, t.id ASC").Scan(ctx)
	return out, err
}

// untaggedCond 没挂本人字典里任何标签（参数：userID）。
const untaggedCond = "NOT EXISTS (SELECT 1 FROM afv_asset_tag_link AS l JOIN afv_asset_tag AS t ON t.id = l.tag_id " +
	"WHERE l.asset_id = a.id AND t.user_id = ?)"

// UntaggedCount 本人没挂任何标签的资产数。归属 / 删除 / 草稿口径同列表（owned）；
// 「挂了标签」只认本人字典里的标签，与列表 tag 筛选、资产卡片 tags（TagsOf）同口径。
func (r *TagRepo) UntaggedCount(ctx context.Context, userID int64) (int64, error) {
	n, err := owned(r.db.NewSelect().TableExpr("afv_asset AS a"), userID).
		Where(untaggedCond, userID).Count(ctx)
	return int64(n), err
}

// AdminList 后台标签分页（新的在前）。
func (r *TagRepo) AdminList(ctx context.Context, userID int64, q string, limit, offset int64) ([]*TagRow, int64, error) {
	filter := func(s *bun.SelectQuery) *bun.SelectQuery {
		if userID > 0 {
			s = s.Where("t.user_id = ?", userID)
		}
		if q != "" {
			s = s.Where("t.name LIKE ?", likeArg(q))
		}
		return s
	}
	cnt, err := filter(r.db.NewSelect().Model((*model.Tag)(nil))).Count(ctx)
	if err != nil || cnt == 0 {
		return nil, int64(cnt), err
	}
	var out []*TagRow
	err = filter(r.db.NewSelect().Model(&out).ColumnExpr("t.id, t.user_id, t.name, t.created_at").ColumnExpr(assetCountExpr)).
		OrderExpr("t.id DESC").Limit(limit).Offset(offset).Scan(ctx)
	return out, int64(cnt), err
}

// Count 用户的标签数。
func (r *TagRepo) Count(ctx context.Context, userID int64) (int64, error) {
	n, err := r.db.NewSelect().Model((*model.Tag)(nil)).Where("t.user_id = ?", userID).Count(ctx)
	return int64(n), err
}

// GetOwned 取本人的标签；不存在 / 不属于本人返回 nil。
func (r *TagRepo) GetOwned(ctx context.Context, id, userID int64) (*model.Tag, error) {
	t := new(model.Tag)
	return noRows(t, r.db.NewSelect().Model(t).Where("t.id = ?", id).Where("t.user_id = ?", userID).Limit(1).Scan(ctx))
}

// ByIDs 本人的标签（ids 里不属于本人的被过滤掉）。
func (r *TagRepo) ByIDs(ctx context.Context, userID int64, ids []int64) ([]*model.Tag, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []*model.Tag
	err := r.db.NewSelect().Model(&out).Where("t.user_id = ?", userID).Where("t.id IN (?)", bun.In(ids)).Scan(ctx)
	return out, err
}

// ByNames 本人按名字取标签（库是 unicode_ci，名字比较不分大小写）。
func (r *TagRepo) ByNames(ctx context.Context, userID int64, names []string) ([]*model.Tag, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var out []*model.Tag
	err := r.db.NewSelect().Model(&out).Where("t.user_id = ?", userID).Where("t.name IN (?)", bun.In(names)).Scan(ctx)
	return out, err
}

// Insert 新建标签，回填 ID；同名撞 uk_user_name 返回 1062（IsDuplicate）。
func (r *TagRepo) Insert(ctx context.Context, t *model.Tag) error {
	_, err := r.db.NewInsert().Model(t).Exec(ctx)
	if err == nil && t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	return err
}

// Rename 改名（带归属）。同名冲突返回 1062。
func (r *TagRepo) Rename(ctx context.Context, id, userID int64, name string) error {
	_, err := r.db.NewUpdate().Model((*model.Tag)(nil)).Set("name = ?", name).
		Where("id = ?", id).Where("user_id = ?", userID).Exec(ctx)
	return err
}

// ownedTagSub 本人标签 id 的子查询（写关联表时纵深防御：调用方已校验过归属，这里再兜一层）。
const ownedTagSub = "tag_id IN (SELECT id FROM afv_asset_tag WHERE user_id = ?)"

// ErrTagNotOwned 要关联的标签不全是本人的。
var ErrTagNotOwned = errors.New("repo: 标签不属于该用户")

// checkTagsOwned ids 必须全是 userID 的标签。
func (r *TagRepo) checkTagsOwned(ctx context.Context, userID int64, ids []int64) error {
	uniq := map[int64]bool{}
	for _, id := range ids {
		uniq[id] = true
	}
	if len(uniq) == 0 {
		return nil
	}
	own, err := r.ByIDs(ctx, userID, ids)
	if err != nil {
		return err
	}
	if len(own) != len(uniq) {
		return ErrTagNotOwned
	}
	return nil
}

// Delete 删除标签与它的全部关联（应在事务里调）。关联的删除同样带 user_id 条件。
func (r *TagRepo) Delete(ctx context.Context, id, userID int64) (bool, error) {
	if _, err := r.db.NewDelete().Model((*model.TagLink)(nil)).Where("tag_id = ?", id).Where(ownedTagSub, userID).Exec(ctx); err != nil {
		return false, err
	}
	n, err := affected(r.db.NewDelete().Model((*model.Tag)(nil)).Where("id = ?", id).Where("user_id = ?", userID).Exec(ctx))
	return n > 0, err
}

// UnlinkAssets 解除一批资产与本人标签的全部关联（删除资产时调，应在事务里）。
func (r *TagRepo) UnlinkAssets(ctx context.Context, userID int64, assetIDs []int64) (int64, error) {
	if len(assetIDs) == 0 {
		return 0, nil
	}
	return affected(r.db.NewDelete().Model((*model.TagLink)(nil)).Where("asset_id IN (?)", bun.In(assetIDs)).Where(ownedTagSub, userID).Exec(ctx))
}

// AssetTag 资产上的一个标签。
type AssetTag struct {
	AssetID int64  `bun:"asset_id"`
	TagID   int64  `bun:"tag_id"`
	Name    string `bun:"name"`
}

// TagsOf 一批资产各自的标签（只取本人字典里的；按关联先后）。
func (r *TagRepo) TagsOf(ctx context.Context, userID int64, assetIDs []int64) (map[int64][]AssetTag, error) {
	out := map[int64][]AssetTag{}
	if len(assetIDs) == 0 {
		return out, nil
	}
	var rows []AssetTag
	err := r.db.NewSelect().TableExpr("afv_asset_tag_link AS l").
		Join("JOIN afv_asset_tag AS t ON t.id = l.tag_id").
		ColumnExpr("l.asset_id, l.tag_id, t.name").
		Where("l.asset_id IN (?)", bun.In(assetIDs)).Where("t.user_id = ?", userID).
		OrderExpr("l.created_at ASC, l.tag_id ASC").Scan(ctx, &rows)
	for _, r := range rows {
		out[r.AssetID] = append(out[r.AssetID], r)
	}
	return out, err
}

// ReplaceAssetTags 整组替换某资产的标签（应在事务里调）。只动 userID 名下标签的关联，tagIDs 必须全是本人的。
func (r *TagRepo) ReplaceAssetTags(ctx context.Context, assetID, userID int64, tagIDs []int64) error {
	if err := r.checkTagsOwned(ctx, userID, tagIDs); err != nil {
		return err
	}
	if _, err := r.db.NewDelete().Model((*model.TagLink)(nil)).Where("asset_id = ?", assetID).Where(ownedTagSub, userID).Exec(ctx); err != nil {
		return err
	}
	if len(tagIDs) == 0 {
		return nil
	}
	links := make([]*model.TagLink, 0, len(tagIDs))
	for _, id := range tagIDs {
		links = append(links, &model.TagLink{AssetID: assetID, TagID: id})
	}
	_, err := r.db.NewInsert().Model(&links).Exec(ctx)
	return err
}

// AddLinks 追加关联（已有的跳过，INSERT IGNORE），返回新增条数。
// tagID 必须全是 userID 的标签。
func (r *TagRepo) AddLinks(ctx context.Context, userID int64, links []*model.TagLink) (int64, error) {
	if len(links) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.TagID)
	}
	if err := r.checkTagsOwned(ctx, userID, ids); err != nil {
		return 0, err
	}
	return affected(r.db.NewInsert().Ignore().Model(&links).Exec(ctx))
}
