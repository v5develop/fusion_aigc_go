package logic

import (
	"context"

	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminGetModelLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminGetModelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminGetModelLogic {
	return &AdminGetModelLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 当前写作模型（只读，来自 afv_system_config + afv_ai_model）
// 当前写作模型（只读，来自 afv_system_config + afv_ai_model）
func (l *AdminGetModelLogic) AdminGetModel(in *pb.Empty) (*pb.ModelInfo, error) {
	w, err := l.svcCtx.Shared.ResolveWritingModel(l.ctx)
	if err != nil {
		return nil, xerr.New(xerr.ServerError, err.Error())
	}
	return &pb.ModelInfo{Id: w.Id, Name: w.Name, Code: w.Code, Platform: w.Platform}, nil
}
