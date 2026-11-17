package core

import (
	"context"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 后台强制收尾（P0-6，admin-api /api/v1/replicate/admin/jobs/{id}/force-finish）。
//
// 只收「非在途」的 job：提交中 / 继续中（请求正在逐段预留）、有段在生成（RUNNING / DELIVERING）或重做预留进行中（HOLDING）一律拒绝，
// 等它结束或被回收器接管。收尾 = 取消没开始的段 → 按步骤状态结清预留镜像（成功的确认扣除、其余退回，与轮询器同一套 settleJob）
// → 汇总到终态。动钱只经 Ledger（pkg/billing 幂等）；结清不了的镜像（扣费 / 退点调用失败）留给轮询器补扫，结果里如实返回。
// 审计由调用方（app/replicate/admin）写 afv_replicate_admin_action。
// ---------------------------------------------------------------------------------------------

// codeAdminForce 后台收尾取消的步骤 / 结束原因。
const codeAdminForce = "admin_force"

// ReasonInFlight 有在途步骤，不能强制收尾。
const ReasonInFlight = "IN_FLIGHT"

// ForceFinishResult 强制收尾结果。
type ForceFinishResult struct {
	JobID         int64  `json:"jobId"`
	FromStatus    string `json:"fromStatus"`
	Status        string `json:"status"`
	CanceledSteps int    `json:"canceledSteps"`
	OpenCharges   int    `json:"openCharges"` // 收尾后仍未结清的镜像（扣费 / 退点调用失败，轮询器继续补扫）
	Message       string `json:"message"`
}

// ForceBlocker 不能强制收尾的原因（空 = 可以）。
func ForceBlocker(j *model.Job, steps []*model.Step, charges []*model.Charge) string {
	if j == nil {
		return "任务不存在"
	}
	if j.Status == model.JobSubmitting || j.Status == model.JobContinuing {
		return "任务正在提交（逐段预留中），稍后再试"
	}
	for _, st := range steps {
		if st.Kind != model.StepKindGateway {
			continue
		}
		switch st.Status {
		case model.StepRunning, model.StepDelivering:
			return "有段正在生成，等它结束或超时后再收尾"
		case model.StepHolding:
			return "有段的重做正在预留点数，稍后再试"
		}
	}
	if model.JobFinal(j.Status) {
		for _, c := range charges {
			if model.ChargeOpen(c.Status) {
				return ""
			}
		}
		return "任务已结束且预留都已结清，不需要收尾"
	}
	return ""
}

// ForceFinish 后台强制收尾（见文件头）。
func (s *Service) ForceFinish(ctx context.Context, jobID int64) (*ForceFinishResult, error) {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, xerr.New(xerr.NotFound, "任务不存在")
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	if why := ForceBlocker(j, steps, charges); why != "" {
		return nil, xerr.NewReason(xerr.Conflict, ReasonInFlight, why)
	}
	res := &ForceFinishResult{JobID: j.ID, FromStatus: jobStatus[j.Status][0]}
	dctx, cancel := s.detached(ctx)
	defer cancel()
	switch j.Status {
	case model.JobWaiting:
		if err := s.finishFirstOnly(dctx, j, codeAdminForce, ""); err != nil {
			return nil, err
		}
	case model.JobQueued, model.JobRunning:
		for _, st := range steps {
			cancelIt := (st.Kind == model.StepKindGateway && st.Status == model.StepPending) ||
				(st.Kind == model.StepKindWriter && !model.StepFinal(st.Status)) ||
				(st.StepKey == model.GateKey && st.Status == model.StepWaiting)
			if !cancelIt {
				continue
			}
			ok, err := s.store.UpdateStep(dctx, st.ID, st.Status, map[string]any{"status": model.StepCanceled,
				"error_code": codeAdminForce, "error_msg": "后台强制收尾", "finished_at": s.now()})
			if err != nil {
				return nil, err
			}
			if ok {
				res.CanceledSteps++
			}
		}
	}
	cur, err := s.store.GetJob(dctx, j.ID)
	if err != nil || cur == nil {
		return nil, err
	}
	if err := s.settleJob(dctx, cur); err != nil {
		return nil, err
	}
	if err := s.rollup(dctx, j.ID); err != nil {
		return nil, err
	}
	if cur, err = s.store.GetJob(dctx, j.ID); err != nil || cur == nil {
		return nil, err
	}
	if charges, err = s.store.ListCharges(dctx, j.ID); err != nil {
		return nil, err
	}
	for _, c := range charges {
		if model.ChargeOpen(c.Status) {
			res.OpenCharges++
		}
	}
	res.Status = jobStatus[cur.Status][0]
	switch {
	case res.OpenCharges > 0:
		res.Message = "已收尾；还有预留单没结清（扣费 / 退点调用失败），系统会自动重试"
	default:
		res.Message = "已收尾，预留都已结清"
	}
	_ = s.store.RescheduleJob(dctx, j.ID, 0) // 让轮询器尽快补合成成片 / 补扫
	s.notifyJob(dctx, j.ID)                  // 收尾后按新状态补发通知（取消的段计入「没做成」；失败只打日志）
	logx.WithContext(ctx).Infof("replicate: 后台强制收尾 job=%d %s→%s 取消步骤=%d 未结镜像=%d", j.ID, res.FromStatus, res.Status, res.CanceledSteps, res.OpenCharges)
	return res, nil
}

// JobStatusName job 状态的对外码与中文（后台列表用，与用户端 JobView 同一张表）。
func JobStatusName(st int8) (code, label string) {
	v, ok := jobStatus[st]
	if !ok {
		return "unknown", "未知"
	}
	return v[0], v[1]
}

// JobStatusCode 对外码 → 库里的状态值（后台按状态筛选用）。
func JobStatusCode(code string) (int8, bool) {
	for k, v := range jobStatus {
		if v[0] == code {
			return k, true
		}
	}
	return 0, false
}
