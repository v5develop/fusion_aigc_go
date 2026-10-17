// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type DerivativesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 生成历史：该文案衍生出的内容
func NewDerivativesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DerivativesLogic {
	return &DerivativesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DerivativesLogic) Derivatives(req *types.DerivativesReq) (resp *types.DerivativesResp, err error) {
	out, err := l.svcCtx.CopywritingRpc.Derivatives(l.ctx, &copywriting.DerivativesReq{
		UserId: ctxdata.UserID(l.ctx), CopywritingId: req.Id, Kind: req.Kind,
		Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.DerivativesResp{Total: out.Total, List: make([]types.Derivative, 0, len(out.List))}
	for _, d := range out.List {
		resp.List = append(resp.List, types.Derivative{
			Id: d.Id, Kind: d.Kind, Size: d.Size, CoverUrl: d.CoverUrl, ResultUrl: d.ResultUrl,
			IsVideo: d.IsVideo, CreateTime: d.CreateTime,
		})
	}
	return resp, nil
}
