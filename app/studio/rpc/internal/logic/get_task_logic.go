package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTaskLogic {
	return &GetTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 单个子任务
func (l *GetTaskLogic) GetTask(in *studio.GetTaskReq) (*studio.TaskInfo, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	if strings.TrimSpace(in.TaskId) == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务号")
	}
	r := l.svcCtx.Repo
	t, err := r.Task.GetByTaskID(l.ctx, strings.TrimSpace(in.TaskId))
	if err != nil {
		return nil, err
	}
	if t == nil || t.UserID != in.UserId || t.Deleted != 0 {
		return nil, xerr.NotFoundf("任务")
	}
	b, err := r.Batch.GetByID(l.ctx, t.BatchID, 0)
	if err != nil {
		return nil, err
	}
	var status int8 = model.BatchGenerating
	if b != nil {
		status = b.Status
	}
	out := toTaskInfo(t, status)
	attachAssetIDs(l.ctx, r, in.UserId, []*studio.TaskInfo{out})
	return out, nil
}
