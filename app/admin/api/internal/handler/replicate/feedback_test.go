package replicate

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	radmin "aigc-local-life/app/replicate/admin"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/xerr"
)

// fbStub 只实现反馈读取（嵌入 nil 的 Store：别的方法一调就 panic，说明走错了路径）。
type fbStub struct {
	radmin.Store
	filter repo.FeedbackFilter
	off    int
	limit  int
	from   time.Time
	to     time.Time
}

func (f *fbStub) AdminListFeedback(_ context.Context, fl repo.FeedbackFilter, off, limit int) ([]*model.Feedback, int64, error) {
	f.filter, f.off, f.limit = fl, off, limit
	return nil, 0, nil
}
func (f *fbStub) AdminFeedbackSummary(_ context.Context, from, to time.Time) (*repo.FeedbackStats, error) {
	f.from, f.to = from, to
	return &repo.FeedbackStats{}, nil
}
func (f *fbStub) ListJobFeedback(context.Context, int64) ([]*model.Feedback, error) { return nil, nil }

func fbHandlers() (*handlers, *fbStub) {
	st := &fbStub{}
	return &handlers{s: radmin.New(st, nil, nil)}, st
}

func TestFeedbackListParams(t *testing.T) {
	h, st := fbHandlers()
	out, err := h.feedbackList(httptest.NewRequest("GET", prefix+"/feedback?page=2&pageSize=500&rating=down&reason=seam_jump&continuity=smooth&userId=7&jobId=42&from=2026-11-01&to=2026-11-10", nil))
	if err != nil {
		t.Fatal(err)
	}
	l := out.(*radmin.FeedbackList)
	if st.limit != 100 || st.off != 100 || l.Size != 100 || l.Page != 2 {
		t.Fatalf("pageSize 夹到 100：limit=%d off=%d %+v", st.limit, st.off, l)
	}
	if st.filter.Rating != "down" || st.filter.Reason != "seam_jump" || st.filter.Continuity != "smooth" || st.filter.UserID != 7 || st.filter.JobID != 42 {
		t.Fatalf("筛选：%+v", st.filter)
	}
	// 日期形式的 to 取当天结束（次日 0 点，北京时间）
	if want := time.Date(2026, 11, 11, 0, 0, 0, 0, cst); !st.filter.To.Equal(want) || !st.filter.From.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, cst)) {
		t.Fatalf("时间窗：%v ~ %v", st.filter.From, st.filter.To)
	}
	// size 作为 pageSize 别名；没给 pageSize 用别名
	if _, err := h.feedbackList(httptest.NewRequest("GET", prefix+"/feedback?size=15", nil)); err != nil || st.limit != 15 {
		t.Fatalf("size 别名：%d %v", st.limit, err)
	}
}

func TestFeedbackListRejectsBadParams(t *testing.T) {
	h, _ := fbHandlers()
	for _, q := range []string{"rating=x", "reason=nope", "continuity=slow", "userId=abc", "jobId=-1", "page=-1", "pageSize=zz", "from=yesterday", "to=2026-13-40",
		"from=2026-01-01&to=2026-12-31", "from=2026-11-10&to=2026-11-01"} {
		_, err := h.feedbackList(httptest.NewRequest("GET", prefix+"/feedback?"+q, nil))
		if c, _, _ := xerr.Parse(err); err == nil || c != xerr.ParamError {
			t.Errorf("%s 应参数错误：%v", q, err)
		}
	}
}

func TestFeedbackSummaryParams(t *testing.T) {
	h, st := fbHandlers()
	if _, err := h.feedbackSummary(httptest.NewRequest("GET", prefix+"/feedback/summary", nil)); err != nil || st.to.Sub(st.from) != 30*24*time.Hour {
		t.Fatalf("默认最近 30 天：%v %v", st.to.Sub(st.from), err)
	}
	if _, err := h.feedbackSummary(httptest.NewRequest("GET", prefix+"/feedback/summary?from=2026-01-01&to=2026-12-31", nil)); err == nil {
		t.Fatal("超过 180 天应拒绝")
	}
	if _, err := h.feedbackSummary(httptest.NewRequest("GET", prefix+"/feedback/summary?from=bad", nil)); err == nil {
		t.Fatal("from 不合法应拒绝")
	}
}

// 没装配业务（h.s == nil）：wrap 统一回「功能暂未开放」，不 panic。
func TestFeedbackNotReady(t *testing.T) {
	h := &handlers{}
	w := httptest.NewRecorder()
	h.wrap(h.feedbackList)(w, httptest.NewRequest("GET", prefix+"/feedback", nil))
	if w.Code == 200 && w.Body.Len() == 0 {
		t.Fatal("应有错误响应")
	}
}
