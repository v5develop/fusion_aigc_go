package logic

import (
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
	"context"
	"strings"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 用户侧 ----
func (l *ListLogic) List(in *assetlib.ListReq) (*assetlib.ListResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	if err := checkListKind(in.Kind); err != nil {
		return nil, err
	}
	if err := checkKindGroup(in.KindGroup); err != nil {
		return nil, err
	}
	ratio, err := cleanAspectRatio(in.AspectRatio)
	if err != nil {
		return nil, err
	}
	if err := checkSource(in.Source); err != nil {
		return nil, err
	}
	if err := checkMedia(in.Media); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(in.Q)
	if runeLen(q) > 100 {
		return nil, xerr.Param("关键字最多 100 字")
	}
	limit, offset := listPage(in.Page, in.PageSize)
	f := repo.Filter{UserID: in.UserId, Kind: in.Kind, KindGroup: in.KindGroup, Source: in.Source, Media: in.Media,
		AspectRatio: ratio, Tag: strings.TrimSpace(in.Tag), FavoriteOnly: in.FavoriteOnly, Q: q}
	rows, total, err := l.svcCtx.Repo.Asset.List(l.ctx, f, limit, offset)
	if err != nil {
		return nil, err
	}
	items, err := toAssets(l.ctx, l.svcCtx, in.UserId, rows)
	if err != nil {
		return nil, err
	}
	return &assetlib.ListResp{Items: items, Total: total}, nil
}
