package logic

import (
	"context"

	"database/sql"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReplanVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReplanVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReplanVideoLogic {
	return &ReplanVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ReplanVideo 规划剧本页的「重新生成」
func (l *ReplanVideoLogic) ReplanVideo(in *pb.IdUserReq) (*pb.VideoInfo, error) {
	c, err := findOwnedVideo(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if c.Status != model.VideoPlanned && c.Status != model.VideoFailed {
		return nil, xerr.New(xerr.Conflict, "当前状态不能重新构思")
	}
	if c.Kind == model.KindVideoRemake && !c.SourceVideoUrl.Valid {
		return nil, xerr.New(xerr.Conflict, "没有可解析的视频")
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	taskID := newRefTaskID(l.svcCtx, "vid", c.Id, "plan")
	ok, err := l.svcCtx.Video.Replan(l.ctx, c.Id, taskID)
	if err != nil {
		return nil, err
	}
	if !ok { // 并发下已被别的请求改了状态
		return nil, xerr.New(xerr.Conflict, "当前状态不能重新构思")
	}
	c.PlanTaskId = sql.NullString{String: taskID, Valid: true}
	if err := submitPlan(l.ctx, l.svcCtx, c, videoShopProfileText(l.ctx, l.svcCtx, c.ShopProfileId.Int64, c.UserId)); err != nil {
		failVideo(l.ctx, l.svcCtx, c.Id, model.VideoPlanning, "提交构思失败，请重试")
		return nil, err
	}
	cur, err := findOwnedVideo(l.ctx, l.svcCtx, c.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toVideoInfo(cur), nil
}
