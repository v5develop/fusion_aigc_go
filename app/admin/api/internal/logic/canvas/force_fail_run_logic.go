// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type ForceFailRunLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 强制失败未结束的 run（gateway 后端退款并取消上游）
func NewForceFailRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ForceFailRunLogic {
	return &ForceFailRunLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ForceFailRunLogic) ForceFailRun(req *types.CanvasAdminForceFailReq) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.CanvasRpc.AdminForceFailRun(l.ctx, &canvas_client.AdminRunReq{Id: req.Id, Reason: req.Reason, AdminId: ctxdata.AdminID(l.ctx)}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
