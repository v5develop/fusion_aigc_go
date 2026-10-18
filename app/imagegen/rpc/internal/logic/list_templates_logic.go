package logic

import (
	"context"

	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTemplatesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTemplatesLogic {
	return &ListTemplatesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 模板列表（复用现有模板素材表，运营在现有后台维护）
func (l *ListTemplatesLogic) ListTemplates(in *pb.ListTemplatesReq) (*pb.TemplateList, error) {
	cfg, err := kindConf(l.svcCtx, in.Kind)
	if err != nil {
		return nil, err
	}
	list, err := l.svcCtx.Shared.ListTemplates(l.ctx, templateSlot(cfg), templateTypeFilter(in.Kind), in.PosterType)
	if err != nil {
		return nil, err
	}
	resp := &pb.TemplateList{List: make([]*pb.Template, 0, len(list))}
	for _, t := range list {
		resp.List = append(resp.List, &pb.Template{Id: t.Id, Title: t.Title, Url: t.Url})
	}
	return resp, nil
}
