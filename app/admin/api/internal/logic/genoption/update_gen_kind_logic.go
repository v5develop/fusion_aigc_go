package genoption

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateGenKindLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 改生成类型的模板必选开关（海报 / 长图 / 动作迁移）
func NewUpdateGenKindLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateGenKindLogic {
	return &UpdateGenKindLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateGenKindLogic) UpdateGenKind(req *types.UpdateGenKindReq) (resp *types.GenKindItem, err error) {
	out, err := l.svcCtx.ImageGenRpc.AdminUpdateGenKind(l.ctx, &imagegen.UpdateGenKindReq{
		Kind: req.Kind, TemplateRequired: req.TemplateRequired,
	})
	if err != nil {
		return nil, err
	}
	return &types.GenKindItem{Kind: out.Kind, TemplateRequired: out.TemplateRequired}, nil
}
