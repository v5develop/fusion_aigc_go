package core

import (
	"context"
	"fmt"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 取消剩余段（POST /api/v1/replicate/jobs/{id}/cancel-rest）：
//   - 只取消还没开始出片的段（出片步骤 PENDING：排队中 / 等自动重试 / 重做已预留未开始），同时取消这些段的写手步骤；
//   - 在途段（RUNNING / DELIVERING）照常做完、照常扣费；已完成段照常入库（finalize → 素材库发件箱，取消的段不占位）；
//   - 未开始段的预留按「链上全部终态无成功」由 settleJob 退回（退回失败记结算待重试，补扫重做）；
//   - 幂等：没有可取消的段（重复点击 / 已终态）直接返回当前 JobView；待继续（只做了第 1 段）等同「不继续了」。
//
// 与轮询器并发安全：步骤 PENDING→CANCELLED 是 CAS，与轮询器 PENDING→RUNNING 互斥——没抢到说明已开始，按在途处理。

// codeCancelRest 用户取消剩余段（步骤 error_code）。
const codeCancelRest = "USER_CANCEL_REST"

// CancelRestView 任务视图上「不做剩下的段」的预览（二次确认框用）。
type CancelRestView struct {
	Available bool   `json:"available"`
	Segments  int    `json:"segments"` // 会取消几段
	Points    int64  `json:"points"`   // 会退回多少点（这些段当前的预留）
	Running   int    `json:"running"`  // 正在生成、会做完的段数
	Text      string `json:"text,omitempty"`
}

// cancelRestPreview 按当前步骤与镜像算预览。
func cancelRestPreview(j *model.Job, steps []*model.Step, charges []*model.Charge) *CancelRestView {
	if j.Status != model.JobQueued && j.Status != model.JobRunning {
		return nil
	}
	v := &CancelRestView{}
	for k := 1; k <= j.SegTotal; k++ {
		chain := segChain(steps, k)
		if len(chain) == 0 {
			continue
		}
		last, refining := segResult(chain)
		if refining {
			continue // 付费精修不是「剩下的段」：用户要求的重做不随「不做剩下的段」被取消
		}
		switch last.Status {
		case model.StepPending:
			v.Segments++
			if c := chargeOf(charges, last.BillingRequestID); c != nil && (c.Status == model.ChargeHeld || c.Status == model.ChargePending) {
				v.Points += c.Points
			}
		case model.StepRunning, model.StepDelivering, model.StepHolding:
			v.Running++
		}
	}
	if v.Segments == 0 {
		return v
	}
	v.Available = true
	v.Text = fmt.Sprintf("不做剩下的 %d 段，退回 %d 点", v.Segments, v.Points)
	if v.Running > 0 {
		v.Text += fmt.Sprintf("；正在生成的 %d 段会做完并正常扣点", v.Running)
	}
	return v
}

// CancelRest 取消剩余段，返回新的 JobView。
func (s *Service) CancelRest(ctx context.Context, userID, jobID int64) (*JobView, error) {
	j, err := s.ownJob(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	switch j.Status {
	case model.JobWaiting:
		if err := s.finishFirstOnly(ctx, j, "user_stop"); err != nil {
			return nil, err
		}
		return s.JobView(ctx, userID, jobID)
	case model.JobSubmitting, model.JobContinuing:
		return nil, xerr.NewReason(xerr.Conflict, "IN_PROGRESS", "正在提交…稍后再试")
	case model.JobQueued, model.JobRunning:
	default:
		return s.JobView(ctx, userID, jobID) // 已结束：幂等返回
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	dctx, cancel := s.detached(ctx) // 取消一旦开始就做完（客户端断开也要退点）
	defer cancel()
	canceledSegs := map[int]bool{}
	refiningSegs := map[int]bool{}
	for k := 1; k <= j.SegTotal; k++ {
		if _, refining := segResult(segChain(steps, k)); refining {
			refiningSegs[k] = true
		}
	}
	for _, st := range steps {
		if st.Kind != model.StepKindGateway || st.Status != model.StepPending || refiningSegs[st.SegIndex] {
			continue
		}
		ok, err := s.store.UpdateStep(dctx, st.ID, model.StepPending, map[string]any{"status": model.StepCanceled, "finished_at": s.now(),
			"error_code": codeCancelRest, "error_msg": "你选择了不做这一段"})
		if err != nil {
			return nil, err
		}
		if ok {
			canceledSegs[st.SegIndex] = true
		}
	}
	for _, st := range steps {
		if st.Kind == model.StepKindWriter && canceledSegs[st.SegIndex] && !model.StepFinal(st.Status) {
			_, _ = s.store.UpdateStep(dctx, st.ID, st.Status, map[string]any{"status": model.StepCanceled, "finished_at": s.now(), "error_code": codeCancelRest})
		}
	}
	if len(canceledSegs) == 0 {
		return s.JobView(ctx, userID, jobID)
	}
	logx.WithContext(ctx).Infof("replicate: 用户取消剩余段 job=%d 段=%v", j.ID, canceledSegs)
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
	_ = s.store.RescheduleJob(dctx, j.ID, 0) // 轮询器尽快补退点 / 合成已完成段
	return s.JobView(ctx, userID, jobID)
}
