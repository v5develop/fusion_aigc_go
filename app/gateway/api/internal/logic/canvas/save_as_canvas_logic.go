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

type SaveAsCanvasLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 另存为：复制已保存的快照（运行态剥离）
func NewSaveAsCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveAsCanvasLogic {
	return &SaveAsCanvasLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SaveAsCanvasLogic) SaveAsCanvas(req *types.CanvasSaveAsReq) (resp *types.CanvasDetail, err error) {
	out, err := l.svcCtx.CanvasRpc.SaveAsCanvas(rpcCtx(l.ctx), &canvas_client.SaveAsCanvasReq{UserId: ctxdata.UserID(l.ctx), SrcId: req.Id, Title: req.Title})
	if err != nil {
		return nil, err
	}
	return toDetail(out), nil
}
