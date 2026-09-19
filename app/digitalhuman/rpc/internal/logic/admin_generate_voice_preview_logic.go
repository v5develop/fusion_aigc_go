package logic

import (
	"context"
	"errors"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminGenerateVoicePreviewLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminGenerateVoicePreviewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminGenerateVoicePreviewLogic {
	return &AdminGenerateVoicePreviewLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 用该音色合成一句固定文案作为试听音频
func (l *AdminGenerateVoicePreviewLogic) AdminGenerateVoicePreview(in *pb.IdReq) (*pb.VoiceInfo, error) {
	v, err := l.svcCtx.Voice.FindAlive(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("音色")
	}
	if err != nil {
		return nil, err
	}
	d, err := synthesize(l.ctx, l.svcCtx, 0, l.svcCtx.Config.TTS.PreviewText, v.Voice)
	if err != nil {
		l.Errorf("preview %s: %v", v.Voice, err)
		return nil, xerr.Newf(xerr.ServerError, "合成试听失败：%v", err)
	}
	if err := l.svcCtx.Voice.UpdatePreview(l.ctx, v.Id, d.URL); err != nil {
		return nil, err
	}
	v.PreviewUrl = nullStr(d.URL)
	return toVoice(v, true), nil
}
