// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ResolveCopywritingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 人工处理卡住的文案：仅限生成中且点数已扣（预留单已确认/部分退回）的记录；标记失败并记录处理说明，不自动退款，返回预留单状态供核对
func NewResolveCopywritingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ResolveCopywritingLogic {
	return &ResolveCopywritingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ResolveCopywritingLogic) ResolveCopywriting(req *types.ResolveCopywritingReq) (resp *types.ResolveCopywritingResp, err error) {
	operator := ctxdata.AdminID(l.ctx)
	out, err := l.svcCtx.CopywritingRpc.AdminResolveStuckCopywriting(l.ctx, &copywriting.AdminResolveReq{Id: req.Id, Note: req.Note})
	if err != nil {
		l.Errorw("admin resolve stuck copywriting failed", logx.Field("id", req.Id),
			logx.Field("operator", operator), logx.Field("err", err.Error()))
		return nil, err
	}
	l.Infow("admin resolved stuck copywriting", logx.Field("id", req.Id), logx.Field("operator", operator),
		logx.Field("reservation_status", out.ReservationStatus), logx.Field("refunded_points", out.RefundedPoints),
		logx.Field("note", req.Note))
	return &types.ResolveCopywritingResp{ReservationStatus: out.ReservationStatus, RefundedPoints: out.RefundedPoints}, nil
}
