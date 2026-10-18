package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminForceFailLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminForceFailLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminForceFailLogic {
	return &AdminForceFailLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminForceFail 运营强制失败生成失败的图片 / 长图：封禁运营重做。点数在失败时已经退回，这里不动账。
func (l *AdminForceFailLogic) AdminForceFail(in *pb.IdReq) (*pb.Empty, error) {
	c, err := l.svcCtx.Creation.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("创作记录")
	}
	if err != nil {
		return nil, err
	}
	if c.Status != model.StatusFailed {
		return nil, xerr.New(xerr.Conflict, "记录不是生成失败，不能强制失败")
	}
	ok, err := l.svcCtx.Creation.ForceFail(l.ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if !ok {
		locked, err := l.svcCtx.Creation.AdminLocked(l.ctx, in.Id)
		if err != nil {
			return nil, err
		}
		if !locked {
			return nil, xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
		}
	}
	return &pb.Empty{}, nil
}
