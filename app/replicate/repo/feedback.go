package repo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// ---------------------------------------------------------------- 后台：反馈检索与汇总（CONTRACT §AB）

// FeedbackFilter 后台反馈检索条件（零值 = 不限）。时间按 created_at（首次反馈时间，有索引 idx_time），左闭右开。
type FeedbackFilter struct {
	Rating     string // up / down
	Reason     string // 原因枚举之一；只匹配 reasons JSON 数组包含它的行（调用方已校验枚举，这里仍按参数绑定）
	Continuity string // fast / smooth
	UserID     int64
	JobID      int64
	From, To   *time.Time
}

// AdminListFeedback 后台反馈列表（id 倒序）。
func (r *Repo) AdminListFeedback(ctx context.Context, f FeedbackFilter, offset, limit int) ([]*model.Feedback, int64, error) {
	var out []*model.Feedback
	q := r.db.NewSelect().Model(&out)
	if f.Rating != "" {
		q = q.Where("fb.rating = ?", f.Rating)
	}
	if f.Reason != "" {
		q = q.Where("JSON_CONTAINS(fb.reasons, ?)", strconv.Quote(f.Reason)) // 枚举码只含 [a-z_]，Quote 即合法 JSON 字符串
	}
	if f.Continuity != "" {
		q = q.Where("fb.continuity = ?", f.Continuity)
	}
	if f.UserID > 0 {
		q = q.Where("fb.user_id = ?", f.UserID)
	}
	if f.JobID > 0 {
		q = q.Where("fb.job_id = ?", f.JobID)
	}
	if f.From != nil {
		q = q.Where("fb.created_at >= ?", msExpr(*f.From))
	}
	if f.To != nil {
		q = q.Where("fb.created_at < ?", msExpr(*f.To))
	}
	n, err := q.OrderExpr("fb.id DESC").Offset(int64(offset)).Limit(int64(limit)).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return out, int64(n), nil
}

// ListJobFeedback 一个任务的全部反馈（所有用户、所有段；按段号、id 升序）。一个任务一个属主、每段最多一行，行数很小。
func (r *Repo) ListJobFeedback(ctx context.Context, jobID int64) ([]*model.Feedback, error) {
	var out []*model.Feedback
	err := r.db.NewSelect().Model(&out).Where("fb.job_id = ?", jobID).OrderExpr("fb.seg_index ASC, fb.id ASC").Limit(200).Scan(ctx)
	return out, err
}

// FeedbackStats 反馈汇总（单行聚合结果）。
type FeedbackStats struct {
	Total, Up, Down int64
	// Reasons 各原因的命中行数（只统计「不满意」行；一行多个原因各计一次），顺序同 core.FeedbackReasons
	Reasons []ReasonStat
	// UpSmooth 等：按连贯度拆开的满意 / 不满意行数
	UpSmooth, DownSmooth, UpFast, DownFast int64
}

// ReasonStat 一个原因的命中行数，及其中连贯 / 更快两种模式各占多少。
type ReasonStat struct {
	Reason       string
	Count        int64
	Smooth, Fast int64
}

// AdminFeedbackSummary [from, to) 内的反馈汇总：一条 SQL 全部在库里算完（条件聚合，没有行数上限、不会截断）。
//
// 原因怎么聚合：reasons 是 JSON 数组、原因枚举固定且很少（core.FeedbackReasons），所以对每个原因用
// SUM(JSON_CONTAINS(reasons, '"code"')) 做条件聚合——MySQL 5.7+ 都能跑，不依赖 JSON_TABLE（8.0.4+），也不需要拉行回应用层再算。
// 以后新增原因码只要进 core.FeedbackReasons，这里自动带上。拼进 SQL 的只有枚举常量与时间毫秒数，没有用户输入。
func (r *Repo) AdminFeedbackSummary(ctx context.Context, from, to time.Time) (*FeedbackStats, error) {
	var b strings.Builder
	b.WriteString("SELECT COUNT(*), COALESCE(SUM(rating = 'up'), 0), COALESCE(SUM(rating = 'down'), 0), " +
		"COALESCE(SUM(rating = 'up' AND continuity = 'smooth'), 0), COALESCE(SUM(rating = 'down' AND continuity = 'smooth'), 0), " +
		"COALESCE(SUM(rating = 'up' AND continuity = 'fast'), 0), COALESCE(SUM(rating = 'down' AND continuity = 'fast'), 0)")
	for _, rs := range core.FeedbackReasons {
		hit := fmt.Sprintf("rating = 'down' AND JSON_CONTAINS(reasons, '\"%s\"')", rs.Code)
		fmt.Fprintf(&b, ", COALESCE(SUM(%s), 0), COALESCE(SUM(%s AND continuity = 'smooth'), 0), COALESCE(SUM(%s AND continuity = 'fast'), 0)", hit, hit, hit)
	}
	fmt.Fprintf(&b, " FROM `%s` WHERE created_at >= FROM_UNIXTIME(%d / 1000) AND created_at < FROM_UNIXTIME(%d / 1000)", model.TableFeedback, from.UnixMilli(), to.UnixMilli())
	st := &FeedbackStats{Reasons: make([]ReasonStat, len(core.FeedbackReasons))}
	dest := []any{&st.Total, &st.Up, &st.Down, &st.UpSmooth, &st.DownSmooth, &st.UpFast, &st.DownFast}
	for i, rs := range core.FeedbackReasons {
		st.Reasons[i].Reason = rs.Code
		dest = append(dest, &st.Reasons[i].Count, &st.Reasons[i].Smooth, &st.Reasons[i].Fast)
	}
	if err := r.db.QueryRowContext(ctx, b.String()).Scan(dest...); err != nil {
		return nil, err
	}
	return st, nil
}
