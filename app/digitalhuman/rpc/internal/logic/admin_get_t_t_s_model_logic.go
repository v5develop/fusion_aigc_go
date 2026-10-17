package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminGetTTSModelLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminGetTTSModelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminGetTTSModelLogic {
	return &AdminGetTTSModelLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 当前配音模型（只读，来自 afv_system_config + afv_ai_model）
func (l *AdminGetTTSModelLogic) AdminGetTTSModel(in *pb.Empty) (*pb.ModelInfo, error) {
	m, err := l.svcCtx.Shared.ResolveTTSModel(l.ctx)
	if err != nil {
		return nil, xerr.New(xerr.NotReady, err.Error())
	}
	return &pb.ModelInfo{Id: m.Id, Name: m.Name, Code: m.Code, Platform: m.Platform}, nil
}
