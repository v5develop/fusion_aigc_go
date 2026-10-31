// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package assetlib

import (
	"aigc-local-life/app/assetlib/rpc/assetlib_client"
	"aigc-local-life/pkg/ctxdata"
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAssetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 资产详情：版本 / 相关资产 / 生成结果，各自分页默认 6
func NewGetAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAssetLogic {
	return &GetAssetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetAssetLogic) GetAsset(req *types.AssetLibGetReq) (resp *types.AssetLibDetail, err error) {
	out, err := l.svcCtx.AssetLibRpc.Get(l.ctx, &assetlib_client.GetReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx),
		VersionPage: req.VersionPage, RelatedPage: req.RelatedPage, ResultPage: req.ResultPage, PageSize: req.PageSize})
	if err != nil {
		return nil, err
	}
	resp = &types.AssetLibDetail{Asset: toAsset(out.Asset), Related: toPage(out.Related), GeneratedResults: toPage(out.GeneratedResults),
		Versions: types.AssetLibVersionPage{Items: []types.AssetLibVersion{}}}
	if out.Versions != nil {
		resp.Versions = types.AssetLibVersionPage{Items: toVersions(out.Versions.Items), Total: out.Versions.Total}
	}
	return resp, nil
}
