package repo

import (
	"context"
	"database/sql"
	"time"

	"aigc-local-life/pkg/usertier"
)

// 免费档水印视图（mrw://{rid}.{ext}）的付费转存：用户付过钱后，把私有原件复制到公有素材库桶，
// 子任务 result_url 与素材库行（afv_asset.result_url / cover_url）改写成公有地址。

// WatermarkRefPrefix 水印视图引用前缀（Java MediaRefs.WATERMARK_PREFIX）。
const WatermarkRefPrefix = "mrw://"

// FreeOriginal 一张待转存的免费档成品。
type FreeOriginal struct {
	ID        int64          `bun:"id"`
	UserID    int64          `bun:"user_id"`
	TaskID    sql.NullString `bun:"task_id"`
	ResultURL string         `bun:"result_url"`
}

// ListPromotableFreeOriginals 主键大于 afterID、result_url 是水印视图、且用户已付过钱（pkg/usertier 同一判据，
// SQL 内联 EXISTS，未付费用户的行不会占住扫描窗口）的子任务，按主键升序。只看未删除（deleted = 0）：
// 用户删掉的批次不该再被复制到公有素材库桶（等于把已删内容公开）；这类任务的素材库行若还留着，保持水印视图，
// 由保留期清理（FreeOriginalRetentionDays）回收原件。走 Go_V1.0.28 的 result_url 前缀索引。
func (r *TaskRepo) ListPromotableFreeOriginals(ctx context.Context, afterID int64, limit int) ([]FreeOriginal, error) {
	var out []FreeOriginal
	err := r.db.NewRaw("SELECT t.id, t.user_id, t.task_id, t.result_url FROM "+TableTask+" AS t "+
		"WHERE t.result_url LIKE 'mrw://%' AND t.deleted = 0 AND t.id > ? AND "+usertier.PaidExistsCond("t.user_id")+
		" ORDER BY t.id LIMIT ?", afterID, max(1, limit)).Scan(ctx, &out)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return out, nil
}

// PromoteFreeOriginal 子任务成品换成公有原图（CAS：result_url 仍是 oldURL 才改）。返回是否改到。
func (r *TaskRepo) PromoteFreeOriginal(ctx context.Context, id int64, oldURL, rid, url string) (bool, error) {
	res, err := r.db.NewUpdate().Table(TableTask).
		Set("result_rid = ?", rid).Set("result_url = ?", url).
		Where("id = ?", id).Where("result_url = ?", oldURL).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PromoteAssetRefs 素材库里该子任务的行：result_url / cover_url 等于 oldURL 的换成 url（studio 入库时两列同值）。
// 按旧值匹配，重复执行无副作用。返回改到的行数。
func (r *TaskRepo) PromoteAssetRefs(ctx context.Context, userID int64, sourceTaskID, oldURL, url string) (int64, error) {
	res, err := r.db.NewRaw("UPDATE afv_asset SET result_url = IF(result_url = ?, ?, result_url), "+
		"cover_url = IF(cover_url = ?, ?, cover_url) "+
		"WHERE source_task_id = ? AND user_id = ? AND (result_url = ? OR cover_url = ?)",
		oldURL, url, oldURL, url, sourceTaskID, userID, oldURL, oldURL).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// OwnsResultRID 该用户是否有一条未删除、成品原件为 rid 的子任务（用来确认「用户拿着签名水印地址当输入」确实是自己的成品）。
func (r *TaskRepo) OwnsResultRID(ctx context.Context, userID int64, rid string) (bool, error) {
	if userID <= 0 || rid == "" {
		return false, nil
	}
	n, err := r.db.NewSelect().Table(TableTask).
		Where("user_id = ?", userID).Where("result_rid = ?", rid).Where("deleted = 0").Limit(1).Count(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListExpiredFreeOriginals 主键大于 afterID、结束时间不晚于 before、仍是水印视图、终态成功、未删除、且用户仍未付费的子任务
// （付费用户的由转存处理，不在这里删）。按主键升序。
func (r *TaskRepo) ListExpiredFreeOriginals(ctx context.Context, before time.Time, afterID int64, limit int) ([]FreeOriginal, error) {
	var out []FreeOriginal
	err := r.db.NewRaw("SELECT t.id, t.user_id, t.task_id, t.result_url FROM "+TableTask+" AS t "+
		"WHERE t.result_url LIKE 'mrw://%' AND t.deleted = 0 AND t.status = 2 AND t.finished_at IS NOT NULL AND t.finished_at <= ? AND t.id > ? AND NOT "+
		usertier.PaidExistsCond("t.user_id")+" ORDER BY t.id LIMIT ?", before, afterID, max(1, limit)).Scan(ctx, &out)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return out, nil
}

// SoftDeleteFreeOriginal 到期清理：子任务逻辑删除（CAS：result_url 仍是 oldURL 且未删除，期间被付费转存改写的不动）。
func (r *TaskRepo) SoftDeleteFreeOriginal(ctx context.Context, id int64, oldURL string) (bool, error) {
	res, err := r.db.NewUpdate().Table(TableTask).Set("deleted = 1").
		Where("id = ?", id).Where("result_url = ?", oldURL).Where("deleted = 0").Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
