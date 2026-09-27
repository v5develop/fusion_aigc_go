// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 开始生成数字人
func NewCreateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateLogic {
	return &CreateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateLogic) Create(req *types.CreateDigitalHumanReq) (resp *types.DigitalHuman, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.Create(l.ctx, &digitalhuman.CreateReq{
		CopywritingId: req.CopywritingId,
		UserId:        ctxdata.UserID(l.ctx), Name: req.Name, PortraitSource: req.PortraitSource, ImageUrl: req.ImageUrl,
		PublicId: req.PublicId, PortraitId: req.PortraitId, ImageIndex: req.ImageIndex, VoiceId: req.VoiceId,
		Script: req.Script, Model: req.Model, RequestId: req.RequestId,
	})
	if err != nil {
		return nil, err
	}
	return toDigitalHuman(out), nil
}
