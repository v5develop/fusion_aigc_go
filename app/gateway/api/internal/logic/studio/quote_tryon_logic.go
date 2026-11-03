// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type QuoteTryonLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 商拍换装提交前报价（只读，不冻结）：请求体同提交，返回本次提交预计冻结的点数
func NewQuoteTryonLogic(ctx context.Context, svcCtx *svc.ServiceContext) *QuoteTryonLogic {
	return &QuoteTryonLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *QuoteTryonLogic) QuoteTryon(req *types.StudioTryonQuoteReq) (resp *types.StudioQuoteResp, err error) {
	out, err := l.svcCtx.StudioRpc.Quote(l.ctx, &studio_client.QuoteReq{
		UserId: ctxdata.UserID(l.ctx), Kind: "tryon", Model: req.Model, Resolution: req.Resolution,
		AspectRatio: req.AspectRatio, TaskTotal: int32(len(req.ModelImageUrls)) * max(req.CountPerModel, 0),
	})
	if err != nil {
		return nil, err
	}
	return toQuoteResp(out), nil
}
