package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"
)

// adminTask 后台按主键或任务号取子任务与所属批次（含已删除）。
func adminTask(ctx context.Context, r *repo.Repo, in *studio.AdminTaskReq) (*model.Task, *model.Batch, error) {
	var (
		t   *model.Task
		err error
	)
	if in.Id > 0 {
		t, err = r.Task.GetByID(ctx, in.Id)
	} else {
		t, err = r.Task.GetByTaskID(ctx, strings.TrimSpace(in.TaskId))
	}
	if err != nil {
		return nil, nil, err
	}
	if t == nil {
		return nil, nil, xerr.NotFoundf("任务")
	}
	b, err := r.Batch.GetByID(ctx, t.BatchID, 0)
	if err != nil {
		return nil, nil, err
	}
	if b == nil {
		return nil, nil, xerr.NotFoundf("批次")
	}
	return t, b, nil
}
