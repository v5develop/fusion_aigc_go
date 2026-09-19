// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"
	"aigc-life/app/copywriting/rpc/copywriting"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetModelLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 当前写作模型（只读）
func NewGetModelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetModelLogic {
	return &GetModelLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetModelLogic) GetModel() (resp *types.ModelResp, err error) {
	m, err := l.svcCtx.CopywritingRpc.AdminGetModel(l.ctx, &copywriting.Empty{})
	if err != nil {
		return nil, err
	}
	return &types.ModelResp{Id: m.Id, Name: m.Name, Code: m.Code, Platform: m.Platform}, nil
}
