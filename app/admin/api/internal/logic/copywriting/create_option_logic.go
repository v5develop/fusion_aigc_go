// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateOptionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增选项
func NewCreateOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateOptionLogic {
	return &CreateOptionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateOptionLogic) CreateOption(req *types.SaveOptionReq) (resp *types.Option, err error) {
	out, err := l.svcCtx.CopywritingRpc.AdminSaveOption(l.ctx, fromSaveOption(req))
	if err != nil {
		return nil, err
	}
	return toOption(out), nil
}
