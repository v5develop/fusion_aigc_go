package logic

import (
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/xerr"
	"context"
	"slices"
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
	entryCodes, err := parseEntryCodes(in.EntryCodes)
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
	tag := strings.TrimSpace(in.Tag)
	if in.Untagged && (tag != "" || strings.Trim(in.Tags, " ,") != "") {
		return nil, xerr.Param("untagged 与 tag / tags 不能同时使用")
	}
	// tags 逗号分隔、全部包含；tag 是单个标签名，整串精确匹配、不按逗号拆（存量含逗号的标签只能这样筛）
	tags, err := parseNameList("tags", in.Tags, l.svcCtx.Config.Limits.TagNameLen)
	if err != nil {
		return nil, err
	}
	if tag != "" && !slices.Contains(tags, tag) {
		tags = append(tags, tag)
	}
	sysTags, err := parseNameList("sys_tags", in.SysTags, asset.SysTagMaxLen)
	if err != nil {
		return nil, err
	}
	limit, offset := listPage(in.Page, in.PageSize)
	f := repo.Filter{UserID: in.UserId, Kind: in.Kind, KindGroup: in.KindGroup, Source: in.Source, Media: in.Media,
		AspectRatio: ratio, EntryCodes: entryCodes, Tags: tags, SysTags: sysTags, Untagged: in.Untagged, FavoriteOnly: in.FavoriteOnly, Q: q}
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
