package replicate

// 成片反馈管理（CONTRACT §AB，只读，权限 replicate:view——中间件前缀映射 GET = view，无需另配）：
//   GET /feedback?page&pageSize&rating&reason&continuity&from&to&userId&jobId   反馈列表
//   GET /feedback/summary?from&to                                              满意 / 不满意、各原因、按连贯度对比
// from / to 缺省 = 最近 30 天，最长 180 天；pageSize 默认 20、最大 100（兼容别名 size）。

import (
	"net/http"
	"strings"

	"aigc-local-life/app/admin/api/internal/svc"
	radmin "aigc-local-life/app/replicate/admin"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest"
)

func registerFeedback(server *rest.Server, c *svc.ServiceContext, h *handlers) {
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodGet, Path: prefix + "/feedback", Handler: h.wrap(h.feedbackList)},
		rest.Route{Method: http.MethodGet, Path: prefix + "/feedback/summary", Handler: h.wrap(h.feedbackSummary)},
	))
}

func (h *handlers) feedbackList(r *http.Request) (any, error) {
	q := r.URL.Query()
	req := radmin.FeedbackListReq{Rating: strings.TrimSpace(q.Get("rating")), Reason: strings.TrimSpace(q.Get("reason")),
		Continuity: strings.TrimSpace(q.Get("continuity"))}
	var err error
	if req.UserID, err = optInt(q.Get("userId")); err != nil || req.UserID < 0 {
		return nil, xerr.Param("userId 不合法")
	}
	if req.JobID, err = optInt(q.Get("jobId")); err != nil || req.JobID < 0 {
		return nil, xerr.Param("jobId 不合法")
	}
	page, err := optInt(q.Get("page"))
	if err != nil || page < 0 {
		return nil, xerr.Param("page 不合法")
	}
	raw := q.Get("pageSize")
	if strings.TrimSpace(raw) == "" {
		raw = q.Get("size")
	}
	size, err := optInt(raw)
	if err != nil || size < 0 {
		return nil, xerr.Param("pageSize 不合法")
	}
	req.Page, req.Size = int(min(page, 1<<31)), int(min(size, 1<<31))
	if req.From, err = parseTime(q.Get("from"), false); err != nil {
		return nil, xerr.Param("from 不合法")
	}
	if req.To, err = parseTime(q.Get("to"), true); err != nil {
		return nil, xerr.Param("to 不合法")
	}
	return h.s.ListFeedback(r.Context(), req)
}

func (h *handlers) feedbackSummary(r *http.Request) (any, error) {
	q := r.URL.Query()
	from, err := parseTime(q.Get("from"), false)
	if err != nil {
		return nil, xerr.Param("from 不合法")
	}
	to, err := parseTime(q.Get("to"), true)
	if err != nil {
		return nil, xerr.Param("to 不合法")
	}
	return h.s.FeedbackSummary(r.Context(), from, to)
}
