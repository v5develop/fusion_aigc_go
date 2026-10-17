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

type GetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 数字人详情（生成中轮询此接口）
func NewGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLogic {
	return &GetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetLogic) Get(req *types.IdPath) (resp *types.DigitalHuman, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.Get(l.ctx, &digitalhuman.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return toDigitalHuman(out), nil
}
