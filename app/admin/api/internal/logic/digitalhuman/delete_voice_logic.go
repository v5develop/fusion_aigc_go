// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"
	"aigc-life/app/digitalhuman/rpc/digitalhuman"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteVoiceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除系统音色
func NewDeleteVoiceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteVoiceLogic {
	return &DeleteVoiceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *DeleteVoiceLogic) DeleteVoice(req *types.IdPath) (resp *types.EmptyResp, err error) {
	if _, err := l.svcCtx.DigitalHumanRpc.AdminDeleteVoice(l.ctx, &digitalhuman.IdReq{Id: req.Id}); err != nil {
		return nil, err
	}
	return &types.EmptyResp{}, nil
}
