package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/note/model"
	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/app/note/rpc/pb"
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

// AdminForceFail 运营强制失败生成失败的笔记：封禁运营重做，状态保持失败。点数在失败时已经退回，这里不动账。
func (l *AdminForceFailLogic) AdminForceFail(in *pb.IdReq) (*pb.Empty, error) {
	n, err := l.svcCtx.Note.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("笔记")
	}
	if err != nil {
		return nil, err
	}
	if n.Status != model.StatusGenFailed {
		return nil, xerr.New(xerr.Conflict, "笔记不是生成失败，不能强制失败")
	}
	ok, err := l.svcCtx.Note.ForceFail(l.ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if !ok {
		locked, err := l.svcCtx.Note.AdminLocked(l.ctx, in.Id)
		if err != nil {
			return nil, err
		}
		if !locked {
			return nil, xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
		}
	}
	return &pb.Empty{}, nil
}
