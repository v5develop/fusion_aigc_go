// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"

	"github.com/zeromicro/go-zero/core/logx"
)

type GenerateVoicePreviewLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 生成试听：用该音色合成一句固定文案
func NewGenerateVoicePreviewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateVoicePreviewLogic {
	return &GenerateVoicePreviewLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GenerateVoicePreviewLogic) GenerateVoicePreview(req *types.IdPath) (resp *types.Voice, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.AdminGenerateVoicePreview(l.ctx, &digitalhuman.IdReq{Id: req.Id})
	if err != nil {
		return nil, err
	}
	return toVoice(out), nil
}
