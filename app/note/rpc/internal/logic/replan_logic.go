package logic

import (
	"context"

	"aigc-life/app/note/model"
	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReplanLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReplanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReplanLogic {
	return &ReplanLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 规划页「重新生成」：用原表单重新构思
func (l *ReplanLogic) Replan(in *pb.ReplanReq) (*pb.NoteInfo, error) {
	n, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if n.Status != model.StatusPlanned && n.Status != model.StatusPlanFailed {
		return nil, xerr.New(xerr.Conflict, "当前状态不能重新构思")
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	count := n.ImageCount
	if in.ImageCount != 0 {
		if in.ImageCount < 1 {
			return nil, xerr.Param("套图数量至少 1 张")
		}
		count = int64(in.ImageCount)
	}
	taskID := newRefTaskID(l.svcCtx, "note", n.Id, "plan")
	ok, err := l.svcCtx.Note.Replan(l.ctx, n.Id, taskID, count)
	if err != nil {
		return nil, err
	}
	if !ok { // 并发下被别的请求抢先改了状态
		return nil, xerr.New(xerr.Conflict, "当前状态不能重新构思")
	}
	n.PlanTaskId, n.ImageCount = nullStr(taskID), count
	submitPlan(l.ctx, l.svcCtx, n)
	if n, err = l.svcCtx.Note.FindOne(l.ctx, n.Id); err != nil {
		return nil, err
	}
	return toInfo(n), nil
}
