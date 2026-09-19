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

type GetPortraitLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// AI 定制形象：查询（生成中轮询此接口）
func NewGetPortraitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPortraitLogic {
	return &GetPortraitLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetPortraitLogic) GetPortrait(req *types.IdPath) (resp *types.Portrait, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.GetPortrait(l.ctx, &digitalhuman.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return toPortrait(out), nil
}
