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

type GetRunLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询一次执行（轮询，1.2s）
func NewGetRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRunLogic {
	return &GetRunLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetRunLogic) GetRun(req *types.CanvasRunPath) (resp *types.CanvasRunResult, err error) {
	out, err := l.svcCtx.CanvasRpc.GetRun(rpcCtx(l.ctx), &canvas_client.RunIdReq{UserId: ctxdata.UserID(l.ctx), Id: req.Id})
	if err != nil {
		return nil, err
	}
	r := toRunResult(out)
	return &r, nil
}
