package admin

// 成片反馈管理（CONTRACT §AB，/api/v1/replicate/admin/feedback**）：
//   - 列表 / 汇总 / 任务详情里的 feedback[] 都只读；权限沿用 replicate:view（本权限体系是按动作而非按资源细分：
//     view / refund / force / sample / switch，没有按资源的 :view 码，所以不新增 replicate:feedback:view，也就不需要迁移或种子）。
//   - 时间范围按反馈首次提交时间（created_at）：默认最近 30 天，跨度最大 180 天；列表 pageSize ≤ 100。
//   - note 已在用户提交时过审，原样给运营；用户号给原值（筛选要用）并附遮掩串。

import (
	"context"
	"encoding/json"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/xerr"
)

const (
	feedbackDefaultSpan = 30 * 24 * time.Hour
	feedbackMaxSpan     = 180 * 24 * time.Hour
	feedbackMaxPageSize = 100
)

// FeedbackStore 反馈读取（*repo.Repo 直接满足）。Store 没实现时（测试替身）相关接口回「功能暂未开放」/ 详情不带 feedback。
type FeedbackStore interface {
	AdminListFeedback(ctx context.Context, f repo.FeedbackFilter, offset, limit int) ([]*model.Feedback, int64, error)
	AdminFeedbackSummary(ctx context.Context, from, to time.Time) (*repo.FeedbackStats, error)
	ListJobFeedback(ctx context.Context, jobID int64) ([]*model.Feedback, error)
}

var _ FeedbackStore = (*repo.Repo)(nil)

func (s *Service) feedbackStore() (FeedbackStore, error) {
	fs, ok := s.store.(FeedbackStore)
	if !ok || fs == nil {
		return nil, xerr.New(xerr.NotReady, "功能暂未开放")
	}
	return fs, nil
}

// FeedbackRow 一条反馈（列表与任务详情共用）。
type FeedbackRow struct {
	ID           int64     `json:"id"`
	UserID       int64     `json:"userId"`
	UserIDMasked string    `json:"userIdMasked"`
	JobID        int64     `json:"jobId"`
	SegIndex     int       `json:"segIndex"` // 0 = 整条成片
	Rating       string    `json:"rating"`
	Reasons      []string  `json:"reasons"`
	Note         string    `json:"note"`
	Continuity   string    `json:"continuity"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func feedbackRow(f *model.Feedback) FeedbackRow {
	reasons := []string{}
	if len(f.Reasons) > 0 {
		_ = json.Unmarshal(f.Reasons, &reasons)
		if reasons == nil { // JSON null
			reasons = []string{}
		}
	}
	return FeedbackRow{ID: f.ID, UserID: f.UserID, UserIDMasked: maskUserID(f.UserID), JobID: f.JobID, SegIndex: f.SegIndex, Rating: f.Rating,
		Reasons: reasons, Note: f.Note, Continuity: f.Continuity, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt}
}

// FeedbackListReq 反馈检索。From / To 为空走默认窗口。
type FeedbackListReq struct {
	Rating, Reason, Continuity string
	UserID, JobID              int64
	From, To                   *time.Time
	Page, Size                 int
}

// FeedbackList 反馈列表。
type FeedbackList struct {
	Items []FeedbackRow `json:"items"`
	Total int64         `json:"total"`
	Page  int           `json:"page"`
	// Size 实际生效的每页条数（pageSize 超过 100 被夹到 100）
	Size int `json:"pageSize"`
	// From / To 实际生效的时间窗口（左闭右开）
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// feedbackWindow 校验并补全时间窗口：都没给 = 最近 30 天；只给 from → 到现在；只给 to → 往前 30 天；from 必须早于 to，跨度 ≤ 180 天。
func (s *Service) feedbackWindow(from, to *time.Time) (time.Time, time.Time, error) {
	var f, t time.Time
	switch {
	case from == nil && to == nil:
		t = s.clock()
		f = t.Add(-feedbackDefaultSpan)
	case from == nil:
		t = *to
		f = t.Add(-feedbackDefaultSpan)
	case to == nil:
		f, t = *from, s.clock()
	default:
		f, t = *from, *to
	}
	if !f.Before(t) {
		return f, t, xerr.Param("from 必须早于 to")
	}
	if t.Sub(f) > feedbackMaxSpan {
		return f, t, xerr.Param("时间范围最长 180 天，请缩小范围")
	}
	return f, t, nil
}

func validFeedbackRating(r string) bool {
	return r == "" || r == core.FeedbackRatingUp || r == core.FeedbackRatingDown
}

func validFeedbackContinuity(c string) bool {
	return c == "" || c == core.ContinuityFast || c == core.ContinuitySmooth
}

func validFeedbackReasonCode(c string) bool {
	if c == "" {
		return true
	}
	for _, r := range core.FeedbackReasons {
		if r.Code == c {
			return true
		}
	}
	return false
}

// ListFeedback 反馈列表（id 倒序；page 默认 1，pageSize 默认 20、最大 100）。
func (s *Service) ListFeedback(ctx context.Context, req FeedbackListReq) (*FeedbackList, error) {
	fs, err := s.feedbackStore()
	if err != nil {
		return nil, err
	}
	if !validFeedbackRating(req.Rating) {
		return nil, xerr.Param("rating 只能是 up / down")
	}
	if !validFeedbackReasonCode(req.Reason) {
		return nil, xerr.Param("reason 不认识：" + req.Reason)
	}
	if !validFeedbackContinuity(req.Continuity) {
		return nil, xerr.Param("continuity 只能是 fast / smooth")
	}
	from, to, err := s.feedbackWindow(req.From, req.To)
	if err != nil {
		return nil, err
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.Size <= 0 {
		req.Size = 20
	}
	req.Size = min(req.Size, feedbackMaxPageSize)
	rows, total, err := fs.AdminListFeedback(ctx, repo.FeedbackFilter{Rating: req.Rating, Reason: req.Reason, Continuity: req.Continuity,
		UserID: req.UserID, JobID: req.JobID, From: &from, To: &to}, (req.Page-1)*req.Size, req.Size)
	if err != nil {
		return nil, err
	}
	out := &FeedbackList{Items: make([]FeedbackRow, 0, len(rows)), Total: total, Page: req.Page, Size: req.Size, From: from, To: to}
	for _, f := range rows {
		out.Items = append(out.Items, feedbackRow(f))
	}
	return out, nil
}

// ReasonCount 一个原因的命中数及按连贯度的拆分。
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
	Smooth int64  `json:"smooth"`
	Fast   int64  `json:"fast"`
}

// ContinuityCount 一种连贯度下的满意 / 不满意数。
type ContinuityCount struct {
	Continuity string `json:"continuity"`
	Up         int64  `json:"up"`
	Down       int64  `json:"down"`
}

// FeedbackSummary 反馈汇总。byReason 只统计「不满意」行，一行多个原因各计一次，所以各项之和可能大于 down；固定给全部 6 个原因（没有也给 0）。
type FeedbackSummary struct {
	Total        int64             `json:"total"`
	Up           int64             `json:"up"`
	Down         int64             `json:"down"`
	ByReason     []ReasonCount     `json:"byReason"`
	ByContinuity []ContinuityCount `json:"byContinuity"`
	From         time.Time         `json:"from"`
	To           time.Time         `json:"to"`
}

// FeedbackSummary 汇总（默认最近 30 天，最长 180 天）。
func (s *Service) FeedbackSummary(ctx context.Context, from, to *time.Time) (*FeedbackSummary, error) {
	fs, err := s.feedbackStore()
	if err != nil {
		return nil, err
	}
	f, t, err := s.feedbackWindow(from, to)
	if err != nil {
		return nil, err
	}
	st, err := fs.AdminFeedbackSummary(ctx, f, t)
	if err != nil {
		return nil, err
	}
	out := &FeedbackSummary{Total: st.Total, Up: st.Up, Down: st.Down, From: f, To: t, ByReason: make([]ReasonCount, 0, len(st.Reasons)),
		ByContinuity: []ContinuityCount{{Continuity: core.ContinuityFast, Up: st.UpFast, Down: st.DownFast},
			{Continuity: core.ContinuitySmooth, Up: st.UpSmooth, Down: st.DownSmooth}}}
	for _, r := range st.Reasons {
		out.ByReason = append(out.ByReason, ReasonCount{Reason: r.Reason, Count: r.Count, Smooth: r.Smooth, Fast: r.Fast})
	}
	return out, nil
}

// ---- 任务详情用 ----

// PlanBrief 任务详情里的方案摘要（只增字段）。
type PlanBrief struct {
	// SubjectText 按人点名的逐行说明（与用户端方案卡 changes[].subjectText 同口径），没点名不出现
	SubjectText []string `json:"subjectText,omitempty"`
}

// AnalysisGetter 读分析行（*repo.Repo 满足；Store 没实现就不出按人说明）。
type AnalysisGetter interface {
	GetAnalysis(ctx context.Context, id int64) (*model.Analysis, error)
}

// planBrief 由方案 + 分析算出方案摘要；读不到 / 解析不了都当作没有（辅助信息不拖垮详情）。
func (s *Service) planBrief(ctx context.Context, p *model.Plan) *PlanBrief {
	ag, ok := s.store.(AnalysisGetter)
	if !ok || p == nil {
		return nil
	}
	a, err := ag.GetAnalysis(ctx, p.AnalysisID)
	if err != nil || a == nil {
		return nil
	}
	an, err := s.resolveAnalysis(ctx, a)
	if err != nil || an == nil {
		return nil
	}
	var ann core.Annotations
	if json.Unmarshal(p.Annotations, &ann) != nil {
		return nil
	}
	if t := core.SubjectTexts(an, ann); len(t) > 0 {
		return &PlanBrief{SubjectText: t}
	}
	return nil
}
