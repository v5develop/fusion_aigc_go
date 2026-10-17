// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateVoiceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增系统音色
func NewCreateVoiceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateVoiceLogic {
	return &CreateVoiceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreateVoiceLogic) CreateVoice(req *types.SaveVoiceReq) (resp *types.Voice, err error) {
	req.Id = 0
	out, err := l.svcCtx.DigitalHumanRpc.AdminSaveVoice(l.ctx, fromSaveVoice(req))
	if err != nil {
		return nil, err
	}
	return toVoice(out), nil
}
