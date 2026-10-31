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

type SubmitTryonLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 商拍换装整批提交（模特 × 每位张数，服务端拆任务）；必须带 Idempotency-Key
func NewSubmitTryonLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitTryonLogic {
	return &SubmitTryonLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SubmitTryonLogic) SubmitTryon(req *types.StudioTryonSubmitReq) (resp *types.StudioSubmitResp, err error) {
	garments := make([]*studio_client.GarmentImage, 0, len(req.GarmentImages))
	for _, g := range req.GarmentImages {
		garments = append(garments, &studio_client.GarmentImage{Url: g.Url, Role: g.Role, Angle: g.Angle})
	}
	out, err := l.svcCtx.StudioRpc.SubmitTryon(l.ctx, &studio_client.SubmitTryonReq{
		UserId: ctxdata.UserID(l.ctx), WorkspaceId: req.WorkspaceId, RequestId: req.RequestId,
		ModelImageUrls: req.ModelImageUrls, GarmentImages: garments, Items: req.Items, SetMode: req.SetMode,
		Lengths: req.Lengths, Sleeves: req.Sleeves, Fabric: req.Fabric, Prompt: req.Prompt, Model: req.Model,
		Resolution: req.Resolution, AspectRatio: req.AspectRatio, CountPerModel: req.CountPerModel,
		ClothImageUrl: req.ClothImageUrl,
	})
	if err != nil {
		return nil, err
	}
	return toSubmitResp(out), nil
}
