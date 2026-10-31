package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CatalogLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCatalogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CatalogLogic {
	return &CatalogLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 目录 ----
func (l *CatalogLogic) Catalog(in *canvas.CatalogReq) (*canvas.CatalogResp, error) {
	return newCore(l.svcCtx).Catalog(l.ctx, in)
}
