// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

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

// 新增数字人选项
func NewCreateOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateOptionLogic {
	return &CreateOptionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateOptionLogic) CreateOption(req *types.DhSaveOptionReq) (resp *types.Option, err error) {
	req.Id = 0
	out, err := l.svcCtx.DigitalHumanRpc.AdminSaveOption(l.ctx, fromSaveOption(req))
	if err != nil {
		return nil, err
	}
	return toOption(out), nil
}
