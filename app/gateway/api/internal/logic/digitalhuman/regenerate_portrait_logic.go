// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type RegeneratePortraitLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// AI 定制形象：重新生成
func NewRegeneratePortraitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegeneratePortraitLogic {
	return &RegeneratePortraitLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RegeneratePortraitLogic) RegeneratePortrait(req *types.RegenerateReq) (resp *types.Portrait, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.RegeneratePortrait(l.ctx, &digitalhuman.RegenerateReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx), RequestId: req.RequestId})
	if err != nil {
		return nil, err
	}
	return toPortrait(out), nil
}
