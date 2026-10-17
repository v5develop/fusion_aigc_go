// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"

	"github.com/zeromicro/go-zero/core/logx"
)

type TemplatesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模板列表（海报可按类型筛）
func NewTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TemplatesLogic {
	return &TemplatesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *TemplatesLogic) Templates(req *types.ImageGenTemplatesReq) (resp *types.ImageGenTemplatesResp, err error) {
	out, err := l.svcCtx.ImageGenRpc.ListTemplates(l.ctx, &imagegen.ListTemplatesReq{Kind: req.Kind, PosterType: req.PosterType})
	if err != nil {
		return nil, err
	}
	resp = &types.ImageGenTemplatesResp{List: make([]types.ImageGenTemplate, 0, len(out.List))}
	for _, t := range out.List {
		resp.List = append(resp.List, types.ImageGenTemplate{Id: t.Id, Title: t.Title, Url: t.Url})
	}
	return resp, nil
}
