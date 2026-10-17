// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package genoption

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateGenOptionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 改生成选项：名称 / 启用 / 排序 / 默认（编码不可改）
func NewUpdateGenOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateGenOptionLogic {
	return &UpdateGenOptionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateGenOptionLogic) UpdateGenOption(req *types.UpdateGenOptionReq) (resp *types.GenOptionItem, err error) {
	out, err := l.svcCtx.ImageGenRpc.AdminUpdateGenOption(l.ctx, &imagegen.UpdateGenOptionReq{
		Id: req.Id, Label: req.Label, SortOrder: req.SortOrder, Enabled: req.Enabled, IsDefault: req.IsDefault,
	})
	if err != nil {
		return nil, err
	}
	item := toGenOptionItem(out)
	return &item, nil
}
