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

type AdminForceFailVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminForceFailVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminForceFailVideoLogic {
	return &AdminForceFailVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminForceFailVideo 运营强制失败出片失败的视频：封禁运营重做，剧本保留。点数在失败时已经退回，这里不动账。
func (l *AdminForceFailVideoLogic) AdminForceFailVideo(in *pb.IdReq) (*pb.Empty, error) {
	v, err := l.svcCtx.Video.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("视频记录")
	}
	if err != nil {
		return nil, err
	}
	if v.Status != model.VideoFailed {
		return nil, xerr.New(xerr.Conflict, "视频不是出片失败，不能强制失败")
	}
	ok, err := l.svcCtx.Video.ForceFail(l.ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if !ok {
		locked, err := l.svcCtx.Video.AdminLocked(l.ctx, in.Id)
		if err != nil {
			return nil, err
		}
		if !locked {
			return nil, xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
		}
	}
	return &pb.Empty{}, nil
}
