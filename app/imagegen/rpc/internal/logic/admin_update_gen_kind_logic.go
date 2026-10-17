package logic

import (
	"context"

	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminUpdateGenKindLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminUpdateGenKindLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminUpdateGenKindLogic {
	return &AdminUpdateGenKindLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminUpdateGenKind 改一个类型的模板必选开关。只允许已在种子里的类型（海报 / 长图 / 动作迁移）。
func (l *AdminUpdateGenKindLogic) AdminUpdateGenKind(in *pb.UpdateGenKindReq) (*pb.GenKindItem, error) {
	kinds, err := l.svcCtx.GenOpts.ListGenKinds(l.ctx)
	if err != nil {
		return nil, err
	}
	var target = -1
	for i, k := range kinds {
		if k.KindCode == in.Kind {
			target = i
			break
		}
	}
	if target < 0 {
		return nil, xerr.Param("不支持的类型")
	}
	row := kinds[target]
	row.TemplateRequired = in.TemplateRequired
	if err := l.svcCtx.GenOpts.SaveGenKind(l.ctx, row); err != nil {
		return nil, err
	}
	invalidateGenOptions()
	return genKindItem(row), nil
}
