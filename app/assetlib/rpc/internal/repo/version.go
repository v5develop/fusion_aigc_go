package repo

import (
	"context"

	"aigc-local-life/app/assetlib/model"

	"github.com/uptrace/bun"
)

// VersionRepo afv_asset_version。
type VersionRepo struct{ db bun.IDB }

// NextNo 下一个版本号：同资产 MAX(version_no)+1（没有版本时为 1）。
// 并发追加撞 uk_asset_version 时由调用方重试。
func (r *VersionRepo) NextNo(ctx context.Context, assetID int64) (int32, error) {
	var n int32
	err := r.db.NewSelect().TableExpr("afv_asset_version").
		ColumnExpr("COALESCE(MAX(version_no), 0) + 1").
		Where("asset_id = ?", assetID).Scan(ctx, &n)
	return n, err
}

// Insert 插入版本，回填 v.ID。
func (r *VersionRepo) Insert(ctx context.Context, v *model.Version) error {
	_, err := r.db.NewInsert().Model(v).Exec(ctx)
	return err
}

// Count 资产的版本数。
func (r *VersionRepo) Count(ctx context.Context, assetID int64) (int64, error) {
	n, err := r.db.NewSelect().Model((*model.Version)(nil)).Where("v.asset_id = ?", assetID).Count(ctx)
	return int64(n), err
}

// Page 版本分页（新的在前）。
func (r *VersionRepo) Page(ctx context.Context, assetID int64, limit, offset int64) ([]*model.Version, error) {
	var out []*model.Version
	err := r.db.NewSelect().Model(&out).Where("v.asset_id = ?", assetID).
		OrderExpr("v.version_no DESC").Limit(limit).Offset(offset).Scan(ctx)
	return out, err
}

// Get 取资产下的某个版本；不属于该资产返回 nil。
func (r *VersionRepo) Get(ctx context.Context, id, assetID int64) (*model.Version, error) {
	v := new(model.Version)
	return noRows(v, r.db.NewSelect().Model(v).Where("v.id = ?", id).Where("v.asset_id = ?", assetID).Limit(1).Scan(ctx))
}
