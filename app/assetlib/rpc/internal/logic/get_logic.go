package logic

import (
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLogic {
	return &GetLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 详情：资产 + 版本 + 相关资产 + 生成结果（各自分页，默认每页 6）
func (l *GetLogic) Get(in *assetlib.GetReq) (*assetlib.GetResp, error) {
	row, err := mustOwn(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	assets, err := toAssets(l.ctx, l.svcCtx, in.UserId, []*repo.AssetRow{row})
	if err != nil {
		return nil, err
	}
	out := &assetlib.GetResp{Asset: assets[0], Versions: &assetlib.VersionPage{}, Related: &assetlib.AssetPage{}, GeneratedResults: &assetlib.AssetPage{}}

	// 版本（新的在前）
	limit, offset := subPage(in.VersionPage, in.PageSize)
	if out.Versions.Total, err = l.svcCtx.Repo.Version.Count(l.ctx, row.ID, in.UserId); err != nil {
		return nil, err
	}
	if out.Versions.Total > offset {
		vs, err := l.svcCtx.Repo.Version.Page(l.ctx, row.ID, in.UserId, limit, offset)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			out.Versions.Items = append(out.Versions.Items, toVersion(v))
		}
	}
	out.Asset.Versions = out.Versions.Items

	// 相关资产（reference 双向 + 本资产的 generated_from 来源）与生成结果（generated_from 指向本资产），只出本人未删除的
	pages := []struct {
		page int64
		f    repo.Filter
		dst  *assetlib.AssetPage
	}{
		{in.RelatedPage, repo.Filter{UserID: in.UserId, RelatedOf: row.ID}, out.Related},
		{in.ResultPage, repo.Filter{UserID: in.UserId, ResultsOf: row.ID}, out.GeneratedResults},
	}
	for _, p := range pages {
		limit, offset := subPage(p.page, in.PageSize)
		rows, total, err := l.svcCtx.Repo.Asset.List(l.ctx, p.f, limit, offset)
		if err != nil {
			return nil, err
		}
		if p.dst.Items, err = toAssets(l.ctx, l.svcCtx, in.UserId, rows); err != nil {
			return nil, err
		}
		p.dst.Total = total
	}
	for _, a := range out.Related.Items {
		out.Asset.RelatedIds = append(out.Asset.RelatedIds, a.Id)
	}
	return out, nil
}
