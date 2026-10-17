// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTTSModelLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 当前配音模型（只读）
func NewGetTTSModelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTTSModelLogic {
	return &GetTTSModelLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetTTSModelLogic) GetTTSModel() (resp *types.ModelResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.AdminGetTTSModel(l.ctx, &digitalhuman.Empty{})
	if err != nil {
		return nil, err
	}
	return &types.ModelResp{Id: out.Id, Name: out.Name, Code: out.Code, Platform: out.Platform}, nil
}
