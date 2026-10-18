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

type AdminRetryVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminRetryVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminRetryVideoLogic {
	return &AdminRetryVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminRetryVideo 运营重做出片失败的视频：沿用用户的「重新出片」（剧本不变，重新冻结、重新提交）。
// 只收出片阶段的失败（已拿到出片任务号）；构思阶段的失败没有可重做的剧本，不走这里。
func (l *AdminRetryVideoLogic) AdminRetryVideo(in *pb.IdReq) (*pb.Empty, error) {
	v, err := l.svcCtx.Video.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("视频记录")
	}
	if err != nil {
		return nil, err
	}
	locked, err := l.svcCtx.Video.AdminLocked(l.ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if locked {
		return nil, xerr.New(xerr.Conflict, "已强制失败，不能重试")
	}
	if v.Status != model.VideoFailed || !v.GenTaskId.Valid {
		return nil, xerr.New(xerr.Conflict, "视频不是出片失败，不能重试")
	}
	if _, err := NewGenerateVideoLogic(l.ctx, l.svcCtx).GenerateVideo(&pb.GenerateVideoReq{Id: in.Id, UserId: v.UserId}); err != nil {
		return nil, err
	}
	return &pb.Empty{}, nil
}
