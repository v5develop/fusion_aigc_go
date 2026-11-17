package repo

import (
	"context"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
)

var _ core.FeedbackStore = (*Repo)(nil)

// UpsertFeedback 按 (user_id, job_id, seg_index) 覆盖写。created=true 表示这是第一次反馈（MySQL ON DUPLICATE：新插入影响 1 行，改写影响 2 行）。
func (r *Repo) UpsertFeedback(ctx context.Context, f *model.Feedback) (bool, error) {
	reasons := "null"
	if len(f.Reasons) > 0 {
		reasons = string(f.Reasons)
	}
	res, err := r.db.ExecContext(ctx, "INSERT INTO `afv_replicate_feedback` (`user_id`, `job_id`, `seg_index`, `rating`, `reasons`, `note`, `continuity`) "+
		"VALUES (?, ?, ?, ?, CAST(? AS JSON), ?, ?) ON DUPLICATE KEY UPDATE `rating` = VALUES(`rating`), `reasons` = VALUES(`reasons`), "+
		"`note` = VALUES(`note`), `continuity` = VALUES(`continuity`), `updated_at` = NOW(3)",
		f.UserID, f.JobID, f.SegIndex, f.Rating, reasons, f.Note, f.Continuity)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// GetFeedback 取本人对某任务某段的反馈；没有返回 nil。
func (r *Repo) GetFeedback(ctx context.Context, userID, jobID int64, segIndex int) (*model.Feedback, error) {
	var f model.Feedback
	return noRows(&f, r.db.NewSelect().Model(&f).Where("fb.user_id = ? AND fb.job_id = ? AND fb.seg_index = ?", userID, jobID, segIndex).Limit(1).Scan(ctx))
}
