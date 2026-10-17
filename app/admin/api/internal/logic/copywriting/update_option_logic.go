// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateOptionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 编辑选项
func NewUpdateOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateOptionLogic {
	return &UpdateOptionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateOptionLogic) UpdateOption(req *types.SaveOptionReq) (resp *types.Option, err error) {
	out, err := l.svcCtx.CopywritingRpc.AdminSaveOption(l.ctx, fromSaveOption(req))
	if err != nil {
		return nil, err
	}
	return toOption(out), nil
}
