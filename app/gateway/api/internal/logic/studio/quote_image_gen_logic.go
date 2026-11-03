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

type QuoteImageGenLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 图片生成提交前报价（只读，不冻结）
func NewQuoteImageGenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *QuoteImageGenLogic {
	return &QuoteImageGenLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *QuoteImageGenLogic) QuoteImageGen(req *types.StudioImageGenQuoteReq) (resp *types.StudioQuoteResp, err error) {
	out, err := l.svcCtx.StudioRpc.Quote(l.ctx, &studio_client.QuoteReq{
		UserId: ctxdata.UserID(l.ctx), Kind: "imagegen", Model: req.Model, Resolution: req.Resolution,
		AspectRatio: req.AspectRatio, TaskTotal: req.Count,
	})
	if err != nil {
		return nil, err
	}
	return toQuoteResp(out), nil
}
