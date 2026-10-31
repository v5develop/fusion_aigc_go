package logic

import (
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CategoriesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCategoriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CategoriesLogic {
	return &CategoriesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分类条计数：all + 5 个 kind + uncategorized（kind 为空），随 source / media 条件
func (l *CategoriesLogic) Categories(in *assetlib.CategoriesReq) (*assetlib.CategoriesResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	if err := checkSource(in.Source); err != nil {
		return nil, err
	}
	if err := checkMedia(in.Media); err != nil {
		return nil, err
	}
	rows, err := l.svcCtx.Repo.Asset.Categories(l.ctx, repo.Filter{UserID: in.UserId, Source: in.Source, Media: in.Media})
	if err != nil {
		return nil, err
	}
	return &assetlib.CategoriesResp{Items: categoryItems(rows)}, nil
}
