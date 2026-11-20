// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelRunLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消执行：gateway 退回点数并取消上游、studio 取消批次、Java 工具调 Java 取消接口（回包 status 可能是 cancelled / succeeded（取消时已成功则交付）/ running（Java 暂不可达，后台重试取消））
func NewCancelRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelRunLogic {
	return &CancelRunLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CancelRunLogic) CancelRun(req *types.CanvasRunPath) (resp *types.CanvasCancelResp, err error) {
	out, err := l.svcCtx.CanvasRpc.CancelRun(rpcCtx(l.ctx), &canvas_client.RunIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id})
	if err != nil {
		return nil, err
	}
	return &types.CanvasCancelResp{RequestId: out.RequestId, Status: out.Status, CancelState: out.CancelState}, nil
}
