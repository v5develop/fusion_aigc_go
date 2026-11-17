package replicate

// 用户投稿管理（成片分享进示例库，Go_V1.0.58，CONTRACT §T）：
//   GET  /shares?status=&userId=&page=&size=  投稿列表（replicate:view；带成片 / 封面现签预览地址、改法摘要、遮掩用户号、rewardAllowed）
//   GET  /shares/:id                          一条投稿（replicate:view）
//   POST /shares/:id/approve {title?}         通过：用投稿生成一条示例并上线（replicate:sample）
//   POST /shares/:id/reject  {reason}         驳回，原因必填（replicate:sample）
//   POST /shares/:id/reward/claim             占位奖励发放（replicate:sample；两阶段第一步：同事务校验开关 / 月上限并 none → granting，返回点数快照；幂等）
//   POST /shares/:id/reward                   标记「奖励已发放」（replicate:sample；Java 积分调整接口成功后再调；granting → granted 无条件，幂等；没占位 409 REWARD_NOT_CLAIMED）
//   POST /shares/:id/reward/release           取消占位（replicate:sample；granting → none，仅 Java 明确没加点时用；幂等）
// 写接口路由级权限在 middleware.replicateRoutePerm（/shares/** 的非 GET = replicate:sample），这里再查一次同一个码（纵深防御）。

import (
	"net/http"
	"strings"

	"aigc-local-life/app/admin/api/internal/svc"
	radmin "aigc-local-life/app/replicate/admin"
	replicatehttp "aigc-local-life/app/replicate/httpapi"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
)

func registerShares(server *rest.Server, c *svc.ServiceContext, h *handlers) {
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodGet, Path: prefix + "/shares", Handler: h.wrap(h.shareList)},
		rest.Route{Method: http.MethodGet, Path: prefix + "/shares/:id", Handler: h.wrap(h.shareDetail)},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/approve", Handler: h.wrap(needSample(h.shareApprove))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/reject", Handler: h.wrap(needSample(h.shareReject))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/reward/claim", Handler: h.wrap(needSample(h.shareRewardClaim))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/reward", Handler: h.wrap(needSample(h.shareReward))},
		rest.Route{Method: http.MethodPost, Path: prefix + "/shares/:id/reward/release", Handler: h.wrap(needSample(h.shareRewardRelease))},
	))
	registerShareRewardSettings(server, c)
}

// ---------------------------------------------------------------- 分享奖励规则（CONTRACT §U，数据库 > 配置文件）
//
// GET    /share-reward-settings  当前规则（replicate:view）
// PUT    /share-reward-settings  {enabled, points, monthlyLimit, reason}（replicate:sample）
// DELETE /share-reward-settings  {reason} 恢复跟随部署配置（replicate:sample）
// 路由级权限同 /shares/**（middleware.replicateRoutePerm），写接口这里再查一次（纵深防御）。

func registerShareRewardSettings(server *rest.Server, c *svc.ServiceContext) {
	st := c.ReplicateShareReward
	wrap := func(fn func(r *http.Request) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if st == nil {
				httpx.ErrorCtx(r.Context(), w, xerr.New(xerr.NotReady, "功能暂未开放"))
				return
			}
			out, err := fn(r)
			if err != nil {
				httpx.ErrorCtx(r.Context(), w, err)
				return
			}
			httpx.OkJsonCtx(r.Context(), w, out)
		}
	}
	path := prefix + "/share-reward-settings"
	server.AddRoutes(rest.WithMiddlewares([]rest.Middleware{c.AdminAuth},
		rest.Route{Method: http.MethodGet, Path: path, Handler: wrap(func(r *http.Request) (any, error) { return st.Get(r.Context()) })},
		rest.Route{Method: http.MethodPut, Path: path, Handler: wrap(needSample(func(r *http.Request) (any, error) {
			var body struct {
				Enabled      *bool  `json:"enabled"`
				Points       *int   `json:"points"`
				MonthlyLimit *int   `json:"monthlyLimit"`
				Reason       string `json:"reason"`
			}
			if err := decode(r, &body); err != nil {
				return nil, err
			}
			if body.Enabled == nil || body.Points == nil || body.MonthlyLimit == nil {
				return nil, xerr.New(xerr.ParamMissing, "缺少 enabled / points / monthlyLimit")
			}
			return st.Set(r.Context(), replicatehttp.ShareRewardReq{AdminID: ctxdata.AdminID(r.Context()), Reason: body.Reason,
				RewardSettings: radmin.RewardSettings{Enabled: *body.Enabled, Points: *body.Points, MonthlyLimit: *body.MonthlyLimit}})
		}))},
		rest.Route{Method: http.MethodDelete, Path: path, Handler: wrap(needSample(func(r *http.Request) (any, error) {
			var body struct {
				Reason string `json:"reason"`
			}
			if err := decode(r, &body); err != nil {
				return nil, err
			}
			return st.Reset(r.Context(), ctxdata.AdminID(r.Context()), body.Reason)
		}))},
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

func (h *handlers) shareRewardClaim(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	return h.s.ClaimReward(r.Context(), id, ctxdata.AdminID(r.Context()))
}

func (h *handlers) shareRewardRelease(r *http.Request) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	return h.s.ReleaseReward(r.Context(), id, ctxdata.AdminID(r.Context()))
}
