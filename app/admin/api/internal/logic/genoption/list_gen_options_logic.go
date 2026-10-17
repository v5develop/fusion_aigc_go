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

type ListGenOptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 生成选项列表（模型 / 比例 / 时长，含停用项）
func NewListGenOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListGenOptionsLogic {
	return &ListGenOptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListGenOptionsLogic) ListGenOptions() (resp *types.GenOptionListResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.AdminGenOptions(l.ctx, &imagegen.Empty{})
	if err != nil {
		return nil, err
	}
	resp = &types.GenOptionListResp{List: make([]types.GenOptionItem, 0, len(out.List)), Kinds: make([]types.GenKindItem, 0, len(out.Kinds))}
	for _, o := range out.List {
		resp.List = append(resp.List, toGenOptionItem(o))
	}
	for _, k := range out.Kinds {
		resp.Kinds = append(resp.Kinds, types.GenKindItem{Kind: k.Kind, TemplateRequired: k.TemplateRequired})
	}
	return resp, nil
}

func toGenOptionItem(o *imagegen.GenOptionItem) types.GenOptionItem {
	return types.GenOptionItem{
		Id: o.Id, Group: o.Group, Code: o.Code, Label: o.Label,
		SortOrder: o.SortOrder, Enabled: o.Enabled, IsDefault: o.IsDefault,
	}
}
