// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/imagegen/rpc/imagegen"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交生成（一次可出多张，异步）
func NewCreateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateLogic {
	return &CreateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateLogic) Create(req *types.CreateImageGenReq) (resp *types.ImageCreation, err error) {
	out, err := l.svcCtx.ImageGenRpc.Create(l.ctx, &imagegen.CreateReq{
		UserId: ctxdata.UserID(l.ctx), Kind: req.Kind, Content: req.Content, MaterialUrls: req.MaterialUrls, TemplateId: req.TemplateId,
		PosterType: req.PosterType, PosterStyle: req.PosterStyle, Size: req.Size, ImageCount: req.ImageCount,
		RequestId: req.RequestId,
	})
	if err != nil {
		return nil, err
	}
	return toImageCreation(out), nil
}
