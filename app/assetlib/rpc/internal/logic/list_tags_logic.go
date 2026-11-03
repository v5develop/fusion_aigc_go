package logic

import (
	"aigc-local-life/pkg/pagex"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTagsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTagsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTagsLogic {
	return &ListTagsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 标签字典
func (l *ListTagsLogic) ListTags(in *assetlib.UserReq) (*assetlib.ListTagsResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	rows, err := l.svcCtx.Repo.Tag.List(l.ctx, in.UserId)
	if err != nil {
		return nil, err
	}
	untagged, err := l.svcCtx.Repo.Tag.UntaggedCount(l.ctx, in.UserId)
	if err != nil {
		return nil, err
	}
	out := &assetlib.ListTagsResp{Items: make([]*assetlib.Tag, 0, len(rows)), UntaggedCount: untagged}
	for _, t := range rows {
		out.Items = append(out.Items, &assetlib.Tag{Id: t.ID, Name: t.Name, AssetCount: t.AssetCount, CreatedAt: pagex.Ms(t.CreatedAt)})
	}
	return out, nil
}
