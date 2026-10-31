package repo

import (
	"context"

	"aigc-local-life/app/assetlib/model"

	"github.com/uptrace/bun"
)

// RelationRepo afv_asset_relation。
type RelationRepo struct{ db bun.IDB }

// Add 建关系；已存在（uk_asset_related_type）时不报错，返回 false。
func (r *RelationRepo) Add(ctx context.Context, assetID, relatedID, userID int64, typ string) (bool, error) {
	if err := assertOwned(ctx, r.db, assetID, userID); err != nil {
		return false, err
	}
	n, err := affected(r.db.NewInsert().Ignore().
		Model(&model.Relation{AssetID: assetID, RelatedAssetID: relatedID, RelationType: typ}).Exec(ctx))
	return n > 0, err
}

// Remove 删关系。reference 是双向展示的，两个方向都删；其它类型只删 asset → related 这一条。
// 带归属条件：assetID 必须是 userID 的资产（调用方已校验，这里纵深防御）。
func (r *RelationRepo) Remove(ctx context.Context, assetID, relatedID, userID int64, typ string) (int64, error) {
	q := r.db.NewDelete().Model((*model.Relation)(nil)).Where("relation_type = ?", typ).
		Where("? IN (SELECT id FROM afv_asset WHERE user_id = ? AND deleted = 0)", assetID, userID)
	if typ == model.RelReference {
		q = q.Where("((asset_id = ? AND related_asset_id = ?) OR (asset_id = ? AND related_asset_id = ?))", assetID, relatedID, relatedID, assetID)
	} else {
		q = q.Where("asset_id = ?", assetID).Where("related_asset_id = ?", relatedID)
	}
	return affected(q.Exec(ctx))
}
