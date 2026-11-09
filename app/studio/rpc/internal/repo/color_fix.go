package repo

import (
	"context"
	"encoding/json"

	"aigc-local-life/app/studio/model"

	"github.com/uptrace/bun"
)

// SetColorFix 写子任务的校色参数（JSON）。只改本人、未删除、成功的那一条；返回是否命中。
func (r *TaskRepo) SetColorFix(ctx context.Context, id, userID int64, params json.RawMessage) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableTask).
		Set("color_fix_params = ?", string(params)).
		Where("id = ?", id).Where("user_id = ?", userID).Where("deleted = 0").
		Where("status = ?", model.TaskSucceeded).Exec(ctx))
	return n > 0, err
}

// assetLink afv_asset 按 source_task_id 聚合的一行。
type assetLink struct {
	SourceTaskID string `bun:"source_task_id"`
	ID           int64  `bun:"id"`
}

// AssetIDs 子任务号 → 素材库资产 id（afv_asset，Java 表只读）。
//
// 成品入库时 source_task_id = 子任务号（logic.saveAsset），表上有 idx_asset_source_task；一次 IN 查询取齐，不逐条查。
// 归属口径与 assetlib 的 owned() 一致（本人、未删除、非草稿），这样返回的 id 直接能调
// POST /api/v1/asset-library/assets/{id}/versions；同一任务有多条（运营重做后再入库）取最新一条。
func (r *TaskRepo) AssetIDs(ctx context.Context, userID int64, taskIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	if userID <= 0 || len(taskIDs) == 0 {
		return out, nil
	}
	var rows []assetLink
	err := r.db.NewSelect().TableExpr("afv_asset AS a").
		ColumnExpr("a.source_task_id").ColumnExpr("MAX(a.id) AS id").
		Where("a.source_task_id IN (?)", bun.In(taskIDs)).
		Where("a.user_id = ?", userID).Where("a.deleted = 0").Where("(a.status IS NULL OR a.status <> 0)").
		GroupExpr("a.source_task_id").Scan(ctx, &rows)
	for _, row := range rows {
		out[row.SourceTaskID] = row.ID
	}
	return out, err
}
