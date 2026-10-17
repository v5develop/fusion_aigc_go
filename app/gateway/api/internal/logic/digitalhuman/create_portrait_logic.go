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

type CreatePortraitLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// AI 定制形象：提交生成（4 张 9:16 图）
func NewCreatePortraitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePortraitLogic {
	return &CreatePortraitLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *CreatePortraitLogic) CreatePortrait(req *types.CreatePortraitReq) (resp *types.Portrait, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.CreatePortrait(l.ctx, &digitalhuman.CreatePortraitReq{
		UserId: ctxdata.UserID(l.ctx), Gender: req.Gender, Age: req.Age, SkinTone: req.SkinTone, Role: req.Role,
		Description: req.Description, RequestId: req.RequestId,
	})
	if err != nil {
		return nil, err
	}
	return toPortrait(out), nil
}
