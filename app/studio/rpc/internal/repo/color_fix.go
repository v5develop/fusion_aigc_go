package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

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
// 带 supersededSrcRid 标记的行（重新生成后才补入素材库的旧成品，见 logic/library_copy.go）不算：子任务的 asset_id
// 与 regen_asset_floor 只认当前成品，旧成品晚入库不能顶替新成品。
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
		Where("JSON_EXTRACT(a.properties, '$."+AssetSupersededKey+"') IS NULL").
		GroupExpr("a.source_task_id").Scan(ctx, &rows)
	for _, row := range rows {
		out[row.SourceTaskID] = row.ID
	}
	return out, err
}

// AssetSupersededKey afv_asset.properties 里标记「成品已换后补入的旧成品」的键（值 = 旧成品的私有源 rid）。
const AssetSupersededKey = "supersededSrcRid"

// AssetIDByURL 同一用户、同一子任务、同一成品地址的资产行 id（含已删除的；没有为 0）。重新生成过的子任务入库查重用：
// 同任务先后有多张成品，按地址查重才不会把别的成品行当成「这一张已入库」。
func (r *TaskRepo) AssetIDByURL(ctx context.Context, userID int64, taskID, url string) (int64, error) {
	var id sql.NullInt64 // MAX 在没有行时是 NULL
	err := r.db.NewSelect().TableExpr("afv_asset AS a").ColumnExpr("MAX(a.id)").
		Where("a.source_task_id = ?", taskID).Where("a.user_id = ?", userID).Where("a.result_url = ?", url).
		Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id.Int64, err
}

// MarkAssetSuperseded 给同一用户、同一子任务、同一成品地址的资产行打「已换成品」标记（properties.supersededSrcRid = srcRid），
// 让它不参与 AssetIDs（子任务 asset_id / regen_asset_floor）。幂等；返回改到的行数。
func (r *TaskRepo) MarkAssetSuperseded(ctx context.Context, userID int64, taskID, url, srcRid string) (int64, error) {
	return affected(r.db.NewUpdate().TableExpr("afv_asset").
		Set("properties = JSON_SET(COALESCE(properties, JSON_OBJECT()), '$."+AssetSupersededKey+"', ?)", srcRid).
		Where("source_task_id = ?", taskID).Where("user_id = ?", userID).Where("result_url = ?", url).
		Where("JSON_EXTRACT(properties, '$." + AssetSupersededKey + "') IS NULL").Exec(ctx))
}
