package repo

import (
	"context"

	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
)

// ExtRepo afv_asset_ext（主键 asset_id）。
type ExtRepo struct{ db bun.IDB }

// Get 取旁表行；forUpdate 时加行锁（事务内读改写用）。没有返回 nil。
func (r *ExtRepo) Get(ctx context.Context, assetID, userID int64, forUpdate bool) (*model.Ext, error) {
	e := new(model.Ext)
	q := r.db.NewSelect().Model(e).Where("e.asset_id = ?", assetID).Where("e."+ownedAssetSub, userID)
	if forUpdate {
		q = q.For("UPDATE")
	}
	return noRows(e, q.Limit(1).Scan(ctx))
}

// extUpdateCols Upsert 命中主键时覆盖的列（asset_id / user_id / created_at 不变，updated_at 由 ON UPDATE 刷新）。
var extUpdateCols = []string{"kind", "favorite", "license", "metadata", "provenance", "current_version_id"}

// Upsert 整行写入：INSERT … ON DUPLICATE KEY UPDATE（读改写由调用方在事务里完成）。
func (r *ExtRepo) Upsert(ctx context.Context, userID int64, e *model.Ext) error {
	if e.UserID != userID {
		return ErrAssetNotOwned
	}
	if err := assertOwned(ctx, r.db, e.AssetID, userID); err != nil {
		return err
	}
	return bundb.Upsert(ctx, r.db, e, []string{"asset_id"}, extUpdateCols)
}
