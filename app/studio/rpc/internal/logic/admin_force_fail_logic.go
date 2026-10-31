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

// AdminForceFail 运营强制失败，并置 admin_locked=1（封禁重做）：
//   - 排队 / 生成中：CAS → 失败，退款；有 gateway 调用号的先尽力 cancel（取消时已出图则拒绝，照常交付）；收敛批次（flow.forceFail）；
//   - 交付中：拒绝（已在确认扣费，不能再改失败）；
//   - 已失败 / 已取消：只置封禁，点数在失败时已退回，不动账（与 img/note/dh 的强制失败同口径）；
//   - 已成功：拒绝。
func (l *AdminForceFailLogic) AdminForceFail(in *studio.AdminTaskReq) (*studio.Empty, error) {
	if in.Id <= 0 && strings.TrimSpace(in.TaskId) == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务")
	}
	s := l.svcCtx
	t, b, err := adminTask(l.ctx, s.Repo, in)
	if err != nil {
		return nil, err
	}
	switch t.Status {
	case model.TaskSucceeded:
		return nil, xerr.New(xerr.Conflict, "任务已成功，不能强制失败")
	case model.TaskFailed, model.TaskCanceled:
		if t.AdminLocked != 0 {
			return &studio.Empty{}, nil
		}
		ok, err := s.Repo.Task.TransitStatus(l.ctx, t.ID, t.Status, t.Status, map[string]any{"admin_locked": 1})
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
		}
		return &studio.Empty{}, nil
	}
	if err := newAdminFlow(s).forceFail(l.ctx, t, b); err != nil {
		return nil, err
	}
	l.Infow("studio admin force-fail", logx.Field("task_id", t.TaskID.String), logx.Field("operator", in.OperatorId))
	return &studio.Empty{}, nil
}
