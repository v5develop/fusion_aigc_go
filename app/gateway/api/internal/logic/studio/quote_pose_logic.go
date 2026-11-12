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

type QuotePoseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 姿态复刻提交前报价（只读，不冻结）
func NewQuotePoseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *QuotePoseLogic {
	return &QuotePoseLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *QuotePoseLogic) QuotePose(req *types.StudioPoseQuoteReq) (resp *types.StudioQuoteResp, err error) {
	in := &studio_client.QuoteReq{
		UserId: ctxdata.UserID(l.ctx), Kind: "pose", Model: req.Model, Resolution: req.Resolution,
		AspectRatio: req.AspectRatio, TaskTotal: int32(len(req.PoseImageUrls)),
	}
	if n := int32(len(req.GarmentImageUrls)); n > 0 { // 带服装参考图才告诉 studio 张数（按请求参数冻结的载体用）；不带的报价与旧版一致
		in.RefImageCount = &n
	}
	out, err := l.svcCtx.StudioRpc.Quote(l.ctx, in)
	if err != nil {
		return nil, err
	}
	return toQuoteResp(out), nil
}
