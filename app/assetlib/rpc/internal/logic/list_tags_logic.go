package logic

import (
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/pagex"
	"aigc-local-life/pkg/xerr"
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

// 标签字典（默认按使用次数倒序，sort=name 按名字）
func (l *ListTagsLogic) ListTags(in *assetlib.ListTagsReq) (*assetlib.ListTagsResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	switch in.Sort {
	case "", repo.TagSortCount, repo.TagSortName:
	default:
		return nil, xerr.Param("sort 只能是 count / name")
	}
	rows, err := l.svcCtx.Repo.Tag.List(l.ctx, in.UserId, in.Sort)
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
