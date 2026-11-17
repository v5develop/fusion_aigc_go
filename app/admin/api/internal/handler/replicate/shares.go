package replicate

// 用户投稿管理（成片分享进示例库，Go_V1.0.58，CONTRACT §T）：
//   GET  /shares?status=&userId=&page=&size=  投稿列表（replicate:view；带成片 / 封面现签预览地址、改法摘要、遮掩用户号、rewardAllowed）
//   GET  /shares/:id                          一条投稿（replicate:view）
//   POST /shares/:id/approve {title?}         通过：用投稿生成一条示例并上线（replicate:sample）
//   POST /shares/:id/reject  {reason}         驳回，原因必填（replicate:sample）
//   POST /shares/:id/reward                   标记「奖励已发放」（replicate:sample；管理端先调 Java 积分调整接口成功后再调；幂等；每人每月最多 3 次）
// 写接口路由级权限在 middleware.replicateRoutePerm（/shares/** 的非 GET = replicate:sample），这里再查一次同一个码（纵深防御）。

import (
	"net/http"
	"strings"

	"aigc-local-life/app/admin/api/internal/svc"
	radmin "aigc-local-life/app/replicate/admin"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest"
)

func registerShares(server *rest.Server, c *svc.ServiceContext, h *handlers) {
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodGet, Path: prefix + "/shares", Handler: h.wrap(h.shareList)},
		rest.Route{Method: http.MethodGet, Path: prefix + "/shares/:id", Handler: h.wrap(h.shareDetail)},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/approve", Handler: h.wrap(needSample(h.shareApprove))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/reject", Handler: h.wrap(needSample(h.shareReject))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/reward", Handler: h.wrap(needSample(h.shareReward))},
	))
}

func (h *handlers) shareList(r *http.Request) (any, error) {
	q := r.URL.Query()
	req := radmin.ShareListReq{Status: strings.TrimSpace(q.Get("status"))}
	var err error
	if req.UserID, err = optInt(q.Get("userId")); err != nil {
		return nil, xerr.Param("userId 不合法")
	}
	page, err := optInt(q.Get("page"))
	if err != nil || page < 0 {
		return nil, xerr.Param("page 不合法")
	}
	size, err := optInt(q.Get("size"))
	if err != nil || size < 0 {
		return nil, xerr.Param("size 不合法")
	}
	req.Page, req.Size = int(page), int(size)
	list, err := h.s.ListShares(r.Context(), req)
	if err != nil {
		return nil, err
	}
	h.signShares(r, list.Items)
	return list, nil
}

// signShares 整页批量签一次成片 / 封面预览地址（签不出的留空，前端显示「预览不可用」）。
func (h *handlers) signShares(r *http.Request, items []radmin.ShareRow) {
	rids := make([]string, 0, len(items)*2)
	for _, it := range items {
		rids = append(rids, it.VideoRid, it.CoverRid)
	}
	urls := h.media.previewURLs(r.Context(), rids)
	for i := range items {
		items[i].VideoURL, items[i].CoverURL = urls[items[i].VideoRid], urls[items[i].CoverRid]
	}
}

func (h *handlers) shareDetail(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	row, err := h.s.GetShare(r.Context(), id)
	if err != nil {
		return nil, err
	}
	rows := []radmin.ShareRow{*row}
	h.signShares(r, rows)
	return &rows[0], nil
}

func (h *handlers) shareApprove(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return h.s.ApproveShare(r.Context(), id, ctxdata.AdminID(r.Context()), body.Title)
}

func (h *handlers) shareReject(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return h.s.RejectShare(r.Context(), id, ctxdata.AdminID(r.Context()), body.Reason)
}

func (h *handlers) shareReward(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	return h.s.MarkRewardGranted(r.Context(), id, ctxdata.AdminID(r.Context()))
}
