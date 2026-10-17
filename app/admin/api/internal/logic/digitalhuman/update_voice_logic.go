// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateVoiceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 编辑系统音色
func NewUpdateVoiceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateVoiceLogic {
	return &UpdateVoiceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateVoiceLogic) UpdateVoice(req *types.SaveVoiceReq) (resp *types.Voice, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.AdminSaveVoice(l.ctx, fromSaveVoice(req))
	if err != nil {
		return nil, err
	}
	return toVoice(out), nil
}
