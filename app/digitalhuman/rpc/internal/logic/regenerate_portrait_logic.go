package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type RegeneratePortraitLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegeneratePortraitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegeneratePortraitLogic {
	return &RegeneratePortraitLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *RegeneratePortraitLogic) RegeneratePortrait(in *pb.RegenerateReq) (*pb.PortraitInfo, error) {
	p, err := l.svcCtx.Portrait.FindOwned(l.ctx, in.Id, in.UserId)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("形象")
	}
	if err != nil {
		return nil, err
	}
	// 沿用原参数，作为一次新的生成（新记录、新 task_id）
	return NewCreatePortraitLogic(l.ctx, l.svcCtx).CreatePortrait(&pb.CreatePortraitReq{
		UserId: in.UserId, Gender: p.Gender, Age: p.Age, SkinTone: p.SkinTone, Role: p.Role.String,
		Description: p.Description.String, RequestId: in.RequestId,
	})
}
