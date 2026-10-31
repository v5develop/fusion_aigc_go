package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveCanvasLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveCanvasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveCanvasLogic {
	return &SaveCanvasLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 保存：if_match_revision 不符且非 force → conflict=true（网关转 4002，data 带服务端 revision）
func (l *SaveCanvasLogic) SaveCanvas(in *canvas.SaveCanvasReq) (*canvas.SaveCanvasResp, error) {
	return newCore(l.svcCtx).SaveCanvas(l.ctx, in)
}
