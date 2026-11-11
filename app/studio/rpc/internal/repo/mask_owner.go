package repo

import (
	"context"
	"database/sql"
	"time"
)

// 区域蒙版的私有签名地址归属反查（POST /api/v1/studio/masks）：媒体服务对象键 = HMAC(rid) 单向派生，
// 拿着签名地址反推不出 rid，只能在「该用户的私有引用成品」里逐个现签比对路径。这里只给候选 rid，比对在 logic 层。

// ListPrivateResultRIDs 该用户未删除、成品仍是私有引用（mr:// 付费待转存 / 转存失败，mrw:// 免费档水印视图）、
// result_rid 属于 class 类别、且创建或完成时间落在窗口内的子任务成品 rid，按主键倒序（最近的先比）、最多 limit 条。
// 已删除的不算：删掉的成品不再替用户付分割费用，与提交侧 OwnsResultRID 同口径。
func (r *TaskRepo) ListPrivateResultRIDs(ctx context.Context, userID int64, class string,
	createdFrom, createdTo, finishedFrom, finishedTo time.Time, limit int) ([]string, error) {
	if userID <= 0 || class == "" {
		return nil, nil
	}
	var out []string
	err := r.db.NewSelect().Table(TableTask).Column("result_rid").
		Where("user_id = ?", userID).Where("deleted = 0").
		Where("(result_url LIKE 'mr://%' OR result_url LIKE 'mrw://%')").
		Where("result_rid LIKE ?", "%-"+class+"-%").
		Where("((created_at >= ? AND created_at < ?) OR (finished_at >= ? AND finished_at < ?))",
			createdFrom, createdTo, finishedFrom, finishedTo).
		OrderExpr("id DESC").Limit(int64(max(1, limit))).Scan(ctx, &out)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return out, nil
}

// OwnedResultURL 该用户未删除子任务（按对外任务号）的成品引用 result_url（mr:// / mrw:// / 公开地址）；
// 不存在、不是本人、已删除、还没有成品都返回 ("", false, nil)。POST /api/v1/studio/masks 带 task_id 时按它精确取图。
func (r *TaskRepo) OwnedResultURL(ctx context.Context, userID int64, taskID string) (string, bool, error) {
	if userID <= 0 || taskID == "" {
		return "", false, nil
	}
	var url sql.NullString
	err := r.db.NewSelect().Table(TableTask).Column("result_url").
		Where("task_id = ?", taskID).Where("user_id = ?", userID).Where("deleted = 0").
		Limit(1).Scan(ctx, &url)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !url.Valid || url.String == "" {
		return "", false, nil
	}
	return url.String, true, nil
}
