// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type KindStatsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分类统计：按 kind × 来源计数
func NewKindStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *KindStatsLogic {
	return &KindStatsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *KindStatsLogic) KindStats(req *types.AssetLibKindStatsReq) (resp *types.AssetLibKindStatsResp, err error) {
	out, err := l.svcCtx.AssetLibRpc.AdminKindStats(l.ctx, &assetlib_client.AdminKindStatsReq{UserId: req.UserId, Media: req.Media})
	if err != nil {
		return nil, err
	}
	resp = &types.AssetLibKindStatsResp{Rows: []types.AssetLibKindStat{}, Kinds: []types.AssetLibCountItem{}, Sources: []types.AssetLibCountItem{}}
	for _, r := range out.Rows {
		resp.Rows = append(resp.Rows, types.AssetLibKindStat{Kind: r.Kind, Source: r.Source, Count: r.Count})
	}
	for _, c := range out.Kinds {
		resp.Kinds = append(resp.Kinds, types.AssetLibCountItem{Key: c.Key, Label: c.Label, Count: c.Count})
	}
	for _, c := range out.Sources {
		resp.Sources = append(resp.Sources, types.AssetLibCountItem{Key: c.Key, Label: c.Label, Count: c.Count})
	}
	return resp, nil

}
