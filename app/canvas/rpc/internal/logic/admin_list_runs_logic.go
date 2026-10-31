package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListRunsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListRunsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListRunsLogic {
	return &AdminListRunsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 运营后台 ----
func (l *AdminListRunsLogic) AdminListRuns(in *canvas.AdminListRunsReq) (*canvas.AdminListRunsResp, error) {
	return newCore(l.svcCtx).AdminListRuns(l.ctx, in)
}
