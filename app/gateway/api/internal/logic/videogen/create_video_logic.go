// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/imagegen/rpc/imagegen"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateVideoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交表单，开始构思剧本（探店 72 / 短剧 87）
func NewCreateVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateVideoLogic {
	return &CreateVideoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateVideoLogic) CreateVideo(req *types.CreateVideoReq) (resp *types.VideoCreation, err error) {
	out, err := l.svcCtx.ImageGenRpc.CreateVideo(l.ctx, &imagegen.CreateVideoReq{
		UserId: ctxdata.UserID(l.ctx), Kind: req.Kind, RequestId: req.RequestId, UserText: req.UserText,
		SellingPoints: req.SellingPoints, ShopProfileId: req.ShopProfileId,
		PortraitSource: req.PortraitSource, ImageUrl: req.ImageUrl, PublicId: req.PublicId,
		PortraitId: req.PortraitId, ImageIndex: req.ImageIndex,
		Env1Url: req.Env1Url, Env2Url: req.Env2Url, PropUrl: req.PropUrl,
		Duration: req.Duration, Size: req.Size, Model: req.Model,
		Source: req.Source, VideoUrl: req.VideoUrl, Link: req.Link,
	})
	if err != nil {
		return nil, err
	}
	return toVideoCreation(out), nil
}
