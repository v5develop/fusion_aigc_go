// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"
	"strings"
	"time"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
)

type AutoColorFixLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 参考图驱动的自动校色（免费，只增 2026-11-12）：按商拍换装子任务（本人、成功）取成品、各单品的服装参考图与逐件蒙版，交给求解服务返回每件单品的五个校色滑杆值（作为该单品图层的初值，不落库，用户点「应用」才保存）；按用户限频（超出 4002 · COLORFIX_AUTO_TOO_FREQUENT）；功能没开 7003 · COLORFIX_AUTO_DISABLED；服务忙 / 超时 6001 · COLORFIX_AUTO_BUSY / COLORFIX_AUTO_TIMEOUT（可重试）
func NewAutoColorFixLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AutoColorFixLogic {
	return &AutoColorFixLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// colorFixAutoCallTimeout 调 studio-rpc AutoColorFix 的超时：rpc 侧要先拿蒙版（最长约 120s）再等一次求解（约 40s），比路由超时（250s）短。
const colorFixAutoCallTimeout = 190 * time.Second

// msgColorFixAutoTooFrequent 超出按用户配额（Config.Studio.ColorFixAuto*）时的提示，码 4002。
const msgColorFixAutoTooFrequent = "自动校色太频繁，请稍后再试"

func (l *AutoColorFixLogic) AutoColorFix(req *types.StudioAutoColorFixReq) (resp *types.StudioAutoColorFixResp, err error) {
	uid := ctxdata.UserID(l.ctx)
	taskID := strings.TrimSpace(req.TaskId)
	if taskID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务号")
	}
	// 先占额度再调 rpc；被拒的不计数。功能没开 / 来源不合法没有求解调用，不该占额度（下面按错误退回）
	if r := l.svcCtx.ColorFixAutoQuota.Take(l.ctx, uid); r != userquota.Allowed {
		l.Infow("studio colorfix auto quota exceeded (check=colorfix_auto_quota)", logx.Field("user_id", uid), logx.Field("window", quotaWindow(r)))
		return nil, xerr.NewReason(xerr.Conflict, xerr.ReasonColorFixAutoTooFrequent, msgColorFixAutoTooFrequent)
	}
	out, err := l.svcCtx.StudioRpc.AutoColorFix(l.ctx, &studio_client.AutoColorFixReq{UserId: uid, TaskId: taskID, Pieces: req.Pieces},
		zrpc.WithCallTimeout(colorFixAutoCallTimeout))
	if err != nil {
		if refundsColorFixQuota(err) {
			l.svcCtx.ColorFixAutoQuota.Refund(l.ctx, uid)
		}
		return nil, err
	}
	resp = &types.StudioAutoColorFixResp{TaskId: out.TaskId, Version: out.Version,
		Pieces: make([]types.StudioAutoColorFixPiece, 0, len(out.Pieces)), Skipped: make([]types.StudioAutoColorFixSkipped, 0, len(out.Skipped))}
	for _, p := range out.Pieces {
		var cp types.StudioColorFixParams
		if p.Params != nil {
			cp = types.StudioColorFixParams{Hue: p.Params.Hue, Saturation: p.Params.Saturation, Value: p.Params.Value,
				Brightness: p.Params.Brightness, Contrast: p.Params.Contrast}
		}
		resp.Pieces = append(resp.Pieces, types.StudioAutoColorFixPiece{Key: p.Key, Params: cp, Score: p.Score})
	}
	for _, s := range out.Skipped {
		resp.Skipped = append(resp.Skipped, types.StudioAutoColorFixSkipped{Key: s.Key, Reason: s.Reason})
	}
	return resp, nil
}

// refundsColorFixQuota 没真正调到求解服务的失败退回额度：参数 / 来源不合法（1002 / 1003 / 4001 任务不存在）、任务未成功（4002）、功能没开（7003）。
// 求解服务忙 / 超时 / 不可用（6001）和一件都解不出（4001 · COLORFIX_AUTO_NO_PIECE）已经占用了求解或分割资源，不退。
func refundsColorFixQuota(err error) bool {
	code, _, ok := xerr.Parse(err)
	if !ok {
		return false
	}
	switch code {
	case xerr.ParamMissing, xerr.ParamError, xerr.NotReady:
		return true
	case xerr.Conflict:
		return true
	case xerr.NotFound:
		return xerr.ReasonOf(err) != xerr.ReasonColorFixAutoNoPiece
	}
	return false
}
