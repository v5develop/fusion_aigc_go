package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// JobView GET /api/v1/replicate/jobs/{id}：逐段状态、钱、决策卡。文案取自 EXPERIENCE §4.7 / §4.8，数字全由后端字段驱动。
type JobView struct {
	JobID           int64     `json:"jobId"`
	TaskID          string    `json:"taskId"`
	PlanNo          string    `json:"planNo"`
	PlanHash        string    `json:"planHash"`
	Status          string    `json:"status"` // submitting / queued / running / waiting / continuing / succeeded / partial / failed / canceled
	StatusLabel     string    `json:"statusLabel"`
	Staged          bool      `json:"staged"`
	SegTotal        int       `json:"segTotal"`
	SegDone         int       `json:"segDone"`
	SegFailed       int       `json:"segFailed"`
	Overview        string    `json:"overview"`
	Segments        []SegView `json:"segments"`
	Money           MoneyView `json:"money"`
	Decision        *Decision `json:"decision,omitempty"`
	Message         string    `json:"message,omitempty"`
	Error           string    `json:"error,omitempty"`
	ConcurrencyText string    `json:"concurrencyText,omitempty"`
	// Result 整条成片（多段拼接 / 失败段占位预览版 / 叠字）；还没到终态为空
	Result *ResultView `json:"result,omitempty"`
	// URLExpiresAt 本视图里所有 *Url 字段的签名到期时间（默认签 1 小时）；过期后重新 GET 这个 job 即可拿到新地址。
	URLExpiresAt *time.Time `json:"urlExpiresAt,omitempty"`
	// Library 当前成片（完整版 / 部分成片 / 预览版）保存到素材库的状态（saved / saving / failed）；没入队（未到终态 / 没有已扣费的段 / 没开启入库）时省略
	Library *LibraryView `json:"library,omitempty"`
	// CancelRest 「不做剩下的段」预览（排队中 / 生成中才有；available=false 表示没有可取消的段）
	CancelRest *CancelRestView `json:"cancelRest,omitempty"`
	// Replay 确认接口返回的是已有 job（幂等重放 / 方案早已确认），不下发；接口层据此不计限频额度
	Replay bool `json:"-"`
}

// SegView 一段。
type SegView struct {
	Index      int    `json:"index"`
	State      string `json:"state"` // not_started / queued / retrying / running / succeeded / failed / canceled
	StateLabel string `json:"stateLabel"`
	Text       string `json:"text,omitempty"`
	MoneyText  string `json:"moneyText,omitempty"`
	Points     int64  `json:"points"`
	VideoRid   string `json:"videoRid,omitempty"`
	VideoURL   string `json:"videoUrl,omitempty"` // 现签播放 / 下载地址（见 urlExpiresAt）
	CoverRid   string `json:"coverRid,omitempty"` // 首帧（网关结果里有才有）
	CoverURL   string `json:"coverUrl,omitempty"`
	Redoable   bool   `json:"redoable"`
	RedoLabel  string `json:"redoLabel,omitempty"`
	Redos      int    `json:"redos"`
	// Refinable 已做好的段还能付费精修（写明要改什么再重做这一段；旧视频在新版本做好前一直可播放）；RefineLabel 如「精修这一段 · 80 点」。
	// 失败段的重做仍是 Redoable / RedoLabel，两者不会同时为真。
	Refinable   bool   `json:"refinable"`
	RefineLabel string `json:"refineLabel,omitempty"`
	// Library 这一段的素材库状态（收尾时已扣费的段自动入库；手动 POST /jobs/{id}/segments/{k}/save 同一行）
	Library *LibraryView `json:"library,omitempty"`
}

// MoneyView 钱（本任务）：预留中 / 已扣除 / 已退回 / 退点处理中 / 扣费确认中。
type MoneyView struct {
	Held          int64  `json:"held"`
	Confirmed     int64  `json:"confirmed"`
	Refunded      int64  `json:"refunded"`
	RefundPending int64  `json:"refundPending"` // 退回调用失败、等补扫重做
	ChargePending int64  `json:"chargePending"` // 段已做好、确认扣除调用失败、等重试（确认前不交付，不会重复扣）
	Text          string `json:"text,omitempty"`
}

// Decision 决策卡。
type Decision struct {
	Kind              string           `json:"kind"` // continue / redo
	Title             string           `json:"title"`
	Options           []DecisionOption `json:"options"`
	AutoContinueLabel string           `json:"autoContinueLabel,omitempty"`
	ExpiresAt         *time.Time       `json:"expiresAt,omitempty"`
	RemainingText     string           `json:"remainingText,omitempty"`
}

// DecisionOption 决策卡上的一个按钮。
type DecisionOption struct {
	Action   string `json:"action"` // continue / adjust / stop / redo
	Label    string `json:"label"`
	Points   int64  `json:"points,omitempty"`
	SegIndex int    `json:"segIndex,omitempty"`
}

var jobStatus = map[int8][2]string{
	model.JobQueued: {"queued", "排队中"}, model.JobRunning: {"running", "正在生成"}, model.JobSucceeded: {"succeeded", "做好了！"},
	model.JobFailed: {"failed", "这次没有生成成功"}, model.JobCanceled: {"canceled", "已取消"}, model.JobPartial: {"partial", "部分做好了"},
	model.JobSubmitting: {"submitting", "正在提交…"}, model.JobWaiting: {"waiting", "第 1 段做好了"}, model.JobContinuing: {"continuing", "正在提交…"},
}

// JobView 本人的任务视图。
func (s *Service) JobView(ctx context.Context, userID, jobID int64) (*JobView, error) {
	j, err := s.ownJob(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	charges, err := s.store.ListCharges(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	st := jobStatus[j.Status]
	v := &JobView{JobID: j.ID, TaskID: deref(j.TaskID), PlanNo: j.PlanNo, PlanHash: j.PlanHash, Status: st[0], StatusLabel: st[1],
		Staged: j.Staged, SegTotal: j.SegTotal, Error: deref(j.ErrorReason), Result: resultView(j.ResultDocument)}
	gateWaiting := false
	for _, x := range steps {
		if x.StepKey == model.GateKey && x.Status == model.StepWaiting {
			gateWaiting = true
		}
	}
	for _, c := range charges {
		switch c.Status {
		case model.ChargeHeld:
			v.Money.Held += c.Points
		case model.ChargeConfirmed:
			v.Money.Confirmed += c.Points
		case model.ChargeRefunded:
			v.Money.Refunded += c.Points
		case model.ChargeSettleFailed:
			if confirmPending(c) {
				v.Money.ChargePending += c.Points
			} else {
				v.Money.RefundPending += c.Points
			}
		}
	}
	v.CancelRest = cancelRestPreview(j, steps, charges)
	var failedSegs []SegView
	for k := 1; k <= j.SegTotal; k++ {
		chain := segChain(steps, k)
		if len(chain) == 0 {
			continue
		}
		sv := s.segView(j, chain, charges, gateWaiting && k > 1)
		if sv.Refinable {
			sv.RefineLabel = s.refineLabel(ctx, j, &in, sv)
		}
		switch sv.State {
		case "succeeded", "refining":
			v.SegDone++
		case "failed":
			v.SegFailed++
			failedSegs = append(failedSegs, sv)
		}
		v.Segments = append(v.Segments, sv)
	}
	v.Overview = fmt.Sprintf("已完成 %d/%d 段", v.SegDone, j.SegTotal)
	if j.SegTotal > 1 {
		v.ConcurrencyText = fmt.Sprintf("最多同时做 %d 段，其余排队", s.cfg.MaxInFlight)
	}
	switch {
	case v.Money.RefundPending > 0:
		v.Money.Text = "退点正在处理中，一般 5 分钟内到账"
	case v.Money.ChargePending > 0:
		v.Money.Text = "有段已经做好，正在确认扣点数（不会重复扣），确认后就能下载"
	case j.Status == model.JobPartial && v.Money.Refunded > 0:
		v.Money.Text = fmt.Sprintf("共花费 %d 点（%d 段做好 %d 段，退回 %d 点）", v.Money.Confirmed, j.SegTotal, v.SegDone, v.Money.Refunded)
	case j.Status == model.JobCanceled && v.Money.Refunded > 0:
		v.Money.Text = fmt.Sprintf("已取消，退回 %d 点", v.Money.Refunded)
	case j.Status == model.JobFailed && v.Money.Refunded > 0:
		v.Money.Text = fmt.Sprintf("这次没有生成成功，已全部退回 %d 点", v.Money.Refunded)
	}

	switch j.Status {
	case model.JobWaiting:
		rest := s.restPoints(ctx, j, &in)
		d := &Decision{Kind: "continue", Title: "第 1 段做好了，和原片对比看看。", AutoContinueLabel: "以后直接全部做（不再先看第 1 段）",
			Options: []DecisionOption{
				{Action: "continue", Label: fmt.Sprintf("满意，继续做剩下 %d 段（预计 %d 点）", j.SegTotal-1, rest), Points: rest},
				{Action: "adjust", Label: "调整后重做第 1 段"},
				{Action: "stop", Label: "不继续了"},
			}}
		if j.ContinueDeadline != nil {
			d.ExpiresAt = j.ContinueDeadline
			if h := int(math.Ceil(j.ContinueDeadline.Sub(s.now()).Hours())); h > 0 {
				d.RemainingText = fmt.Sprintf("还剩 %d 小时", h)
			}
		}
		v.Decision = d
	case model.JobSucceeded:
		switch deref(j.StopReason) {
		case "continue_expired":
			v.Message = fmt.Sprintf("超过 %d 小时没有继续，已为你结束，只保留了第 1 段，没有收费", int(s.cfg.ContinueWindow/time.Hour))
		case "user_stop":
			v.Message = "好的，已结束。第 1 段留给你，可以下载，没有再收费"
		}
	case model.JobPartial, model.JobFailed:
		var opts []DecisionOption
		for _, f := range failedSegs {
			if f.Redoable {
				opts = append(opts, DecisionOption{Action: "redo", Label: f.RedoLabel, SegIndex: f.Index, Points: f.Points})
			}
		}
		if len(opts) > 0 {
			title := "这次没有生成成功"
			if j.Status == model.JobPartial {
				title = fmt.Sprintf("做好了 %d 段，%d 段没成功。重做这一段，做好后会自动拼成完整视频", v.SegDone, v.SegFailed)
			}
			v.Decision = &Decision{Kind: "redo", Title: title, Options: opts}
		}
	}
	if s.delivery != nil {
		if list, err := s.delivery.store.ListDeliveries(ctx, j.ID); err == nil {
			var final, preview *model.Delivery
			for _, d := range list {
				switch d.Kind {
				case model.DeliveryFinal:
					final = d
				case model.DeliveryPreview:
					preview = d
				case model.DeliverySegment:
					for i := range v.Segments {
						if v.Segments[i].Index == d.SegIndex { // 精修后的新版本是更晚的一行（按 id 升序，后者覆盖前者）
							v.Segments[i].Library = libraryView(d)
						}
					}
				}
			}
			// 成片的素材库状态跟着当前成片走：当前是预览版就报预览版那条，否则报完整版 / 部分成片
			if v.Result != nil && v.Result.Kind == ResultPreview && preview != nil {
				v.Library = libraryView(preview)
			} else if final != nil {
				v.Library = libraryView(final)
			}
		}
	}
	s.signJobView(ctx, v)
	return v, nil
}

// signJobView 给逐段视频 / 封面与整条成片现签播放地址（一次批量签；签名失败只打日志、不影响视图，前端可重拉）。
func (s *Service) signJobView(ctx context.Context, v *JobView) {
	var rids []string
	for _, sg := range v.Segments {
		rids = append(rids, sg.VideoRid, sg.CoverRid)
	}
	if v.Result != nil {
		rids = append(rids, v.Result.VideoRid, v.Result.CoverRid)
	}
	urls, exp := s.signRIDs(ctx, rids)
	if urls == nil {
		return
	}
	for i := range v.Segments {
		v.Segments[i].VideoURL, v.Segments[i].CoverURL = urls[v.Segments[i].VideoRid], urls[v.Segments[i].CoverRid]
	}
	if v.Result != nil {
		v.Result.VideoURL, v.Result.CoverURL = urls[v.Result.VideoRid], urls[v.Result.CoverRid]
	}
	v.URLExpiresAt = exp
}

// signRIDs 现签一批 rid（去空去重、只签合法 rid）。返回 rid → 地址与到期时间；没装配签名或全空返回 nil。
func (s *Service) signRIDs(ctx context.Context, rids []string) (map[string]string, *time.Time) {
	if s.signer == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	var list []string
	for _, r := range rids {
		if r != "" && !seen[r] && mediasvc.ValidRID(r) {
			seen[r] = true
			list = append(list, r)
		}
	}
	if len(list) == 0 {
		return nil, nil
	}
	exp := s.now().Add(s.cfg.URLTTL)
	urls, err := s.signer.ResourceURLs(ctx, list, s.cfg.URLTTL)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 现签播放地址失败（视图只给 rid） 判据=SIGN_FAILED 数量=%d: %v", len(list), err)
		return nil, nil
	}
	return urls, &exp
}

func (s *Service) segView(j *model.Job, chain []*model.Step, charges []*model.Charge, notReleased bool) SegView {
	last := chain[len(chain)-1]
	eff, refining := segResult(chain) // 视频 / 状态以有效结果为准；链尾只在「正在精修 / 精修失败」时用来补充说明
	sv := SegView{Index: last.SegIndex, Redos: redoCount(chain)}
	if c := chargeOf(charges, eff.BillingRequestID); c != nil {
		sv.Points = c.Points
	}
	if refining {
		sv.State, sv.StateLabel = "refining", "精修中"
		sv.Text = "正在按你的要求精修，做好前仍可播放和下载现在的版本"
		if isAutoRetry(last.StepKey) {
			sv.Text = "精修遇到点小问题，正在自动重试（不会多收点数），做好前仍可播放现在的版本"
		}
		if v, ok := videoOf(eff); ok {
			sv.VideoRid, sv.CoverRid = v.VideoRid, v.CoverRid
		}
		if c := chargeOf(charges, last.BillingRequestID); c != nil {
			sv.Points = c.Points // 精修这一次的预留
		}
		return sv
	}
	if eff != last { // 精修没成功：退回了精修预留，这一段仍是旧版本
		if c := chargeOf(charges, last.BillingRequestID); c != nil {
			sv.MoneyText = ChargeMoneyText(c)
		}
		sv.Text = "上一次精修没成功，已退回点数，现在的版本没有变"
	}
	last = eff
	switch last.Status {
	case model.StepPending, model.StepHolding:
		if notReleased {
			sv.State, sv.StateLabel = "not_started", "满意第 1 段后再做"
		} else if isAutoRetry(last.StepKey) {
			sv.State, sv.StateLabel, sv.Text = "retrying", "生成中", "这一段遇到点小问题，正在自动重试（不会多收点数）"
		} else {
			sv.State, sv.StateLabel = "queued", "排队中"
		}
	case model.StepRunning, model.StepDelivering:
		sv.State, sv.StateLabel = "running", "生成中"
		if isAutoRetry(last.StepKey) {
			sv.Text = "这一段遇到点小问题，正在自动重试（不会多收点数）"
		}
	case model.StepSucceeded:
		sv.State, sv.StateLabel = "succeeded", "已完成"
		var out struct {
			VideoRid string `json:"videoRid"`
			CoverRid string `json:"coverRid"`
		}
		_ = json.Unmarshal(last.Output, &out)
		sv.VideoRid, sv.CoverRid = out.VideoRid, out.CoverRid
		if s.refineAllowed(j, sv.Redos) {
			sv.Refinable = true
			sv.RefineLabel = fmt.Sprintf("精修这一段 · %d 点", sv.Points)
		}
	case model.StepCanceled:
		sv.State, sv.StateLabel = "canceled", "已取消"
		switch deref(last.ErrorCode) {
		case codeNotContinued:
			sv.StateLabel, sv.Text = "未做", "没有继续，这一段没有收费"
		case codeCancelRest:
			sv.StateLabel, sv.Text = "未做", "你选择了不做这一段，点数已退回"
			if c := chargeOf(charges, last.BillingRequestID); c != nil {
				if model.ChargeOpen(c.Status) {
					sv.Text = "你选择了不做这一段，退点正在处理中"
				} else if t := ChargeMoneyText(c); t != "" {
					sv.MoneyText = t
				}
			}
		}
	case model.StepFailed:
		sv.State, sv.StateLabel = "failed", "没成功"
		f := failTable[deref(last.ErrorCode)]
		sv.Text = orText(deref(last.ErrorMsg), f.Text)
		if c := chargeOf(charges, last.BillingRequestID); c != nil {
			sv.MoneyText = ChargeMoneyText(c)
		}
		if sv.Redos < s.cfg.RedoMaxPerSegment && j.RedoCount < s.cfg.RedoMaxPerJob && deref(last.ErrorCode) != FailRejected {
			sv.Redoable = true
			sv.RedoLabel = orText(f.Action, "重做这一段")
			if sv.Points > 0 && strings.HasPrefix(sv.RedoLabel, "重做") {
				sv.RedoLabel = fmt.Sprintf("%s（%d 点）", sv.RedoLabel, sv.Points)
			}
		}
	}
	return sv
}

// refineAllowed 已做好的段能否再精修：任务状态（成功 / 部分成功 / 在途）、每段 / 每任务重做次数与 Redo 一致。
func (s *Service) refineAllowed(j *model.Job, segRedos int) bool {
	switch j.Status {
	case model.JobSucceeded, model.JobPartial, model.JobQueued, model.JobRunning:
	default:
		return false
	}
	return segRedos < s.cfg.RedoMaxPerSegment && j.RedoCount < s.cfg.RedoMaxPerJob
}

// refineLabel 精修按钮文案：点数取现在的实时报价（与 Redo 的 ExpectedPoints 校验同口径）；报价失败回落这一段原来的点数。
func (s *Service) refineLabel(ctx context.Context, j *model.Job, in *JobInput, sv SegView) string {
	pts := sv.Points
	if sv.Index >= 1 && sv.Index <= len(in.Segments) {
		if q, err := s.ledger.Quote(ctx, s.charge(j.UserID, "", in.ModelGroup, in.Resolution, in.Segments[sv.Index-1].GenDuration)); err == nil {
			pts = q
		}
	}
	return fmt.Sprintf("精修这一段 · %d 点", pts)
}

// restPoints 决策卡上「继续」的预计点数（实时报价；报价失败时不显示数字）。
func (s *Service) restPoints(ctx context.Context, j *model.Job, in *JobInput) int64 {
	if len(in.Segments) < 2 {
		return 0
	}
	var idx []int
	for k := 2; k <= len(in.Segments); k++ {
		idx = append(idx, k)
	}
	_, total, err := s.quoteIndexes(ctx, j.UserID, in, idx)
	if err != nil {
		return 0
	}
	return total
}

// JobSummary 历史列表的一行（GET /api/v1/replicate/jobs）。
type JobSummary struct {
	JobID          int64     `json:"jobId"`
	TaskID         string    `json:"taskId"`
	PlanNo         string    `json:"planNo"`
	AnalysisNo     string    `json:"analysisNo"` // 源视频的分析编号（同一源视频的任务相同），取不到为空
	Status         string    `json:"status"`
	StatusLabel    string    `json:"statusLabel"`
	Title          string    `json:"title"`
	SegTotal       int       `json:"segTotal"`
	SegDone        int       `json:"segDone"`
	Points         int64     `json:"points"`                   // 合计点数：已扣除 + 预留中（已退回的不算）
	SourceThumbRid string    `json:"sourceThumbRid,omitempty"` // 源视频首帧
	SourceThumbURL string    `json:"sourceThumbUrl,omitempty"`
	CoverRid       string    `json:"coverRid,omitempty"` // 结果封面（成片 / 第一个成功段的首帧）
	CoverURL       string    `json:"coverUrl,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// JobList 历史列表。
type JobList struct {
	Items        []JobSummary `json:"items"`
	Total        int64        `json:"total"`
	Page         int          `json:"page"`
	Size         int          `json:"size"`
	URLExpiresAt *time.Time   `json:"urlExpiresAt,omitempty"`
}

// 历史列表分页上限。
const (
	defaultListSize = 20
	maxListSize     = 50
)

// ListJobs 本人的出片任务（创建倒序）。page 从 1 起；size 默认 20、最大 50。
func (s *Service) ListJobs(ctx context.Context, userID int64, page, size int) (*JobList, error) {
	if userID <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = defaultListSize
	}
	if size > maxListSize {
		size = maxListSize
	}
	jobs, total, err := s.store.ListJobs(ctx, userID, (page-1)*size, size)
	if err != nil {
		return nil, err
	}
	out := &JobList{Items: make([]JobSummary, 0, len(jobs)), Total: total, Page: page, Size: size}
	var rids []string
	analysisNos := map[int64]string{} // 方案 id → 分析编号（同一页里多个任务常出自同一方案 / 同一分析）
	for _, j := range jobs {
		var in JobInput
		_ = json.Unmarshal(j.Input, &in)
		st := jobStatus[j.Status]
		it := JobSummary{JobID: j.ID, TaskID: deref(j.TaskID), PlanNo: j.PlanNo, Status: st[0], StatusLabel: st[1],
			Title: orText(in.Title, "视频做同款"), SegTotal: j.SegTotal, SegDone: j.SegDone, SourceThumbRid: in.SourceThumbRid, CreatedAt: j.CreatedAt}
		if j.PlanID > 0 {
			no, ok := analysisNos[j.PlanID]
			if !ok {
				if p, _ := s.store.GetPlan(ctx, j.PlanID); p != nil {
					if a, _ := s.store.GetAnalysis(ctx, p.AnalysisID); a != nil {
						no = a.AnalysisNo
					}
				}
				analysisNos[j.PlanID] = no
			}
			it.AnalysisNo = no
		}
		if charges, err := s.store.ListCharges(ctx, j.ID); err == nil {
			for _, c := range charges {
				if c.Status == model.ChargeConfirmed || c.Status == model.ChargeHeld || confirmPending(c) {
					it.Points += c.Points
				}
			}
		}
		if rv := resultView(j.ResultDocument); rv != nil && rv.CoverRid != "" {
			it.CoverRid = rv.CoverRid
		} else if steps, err := s.store.ListSteps(ctx, j.ID); err == nil {
			for k := 1; k <= j.SegTotal && it.CoverRid == ""; k++ {
				if eff, _ := segResult(segChain(steps, k)); eff != nil && eff.Status == model.StepSucceeded {
					var o struct {
						CoverRid string `json:"coverRid"`
					}
					_ = json.Unmarshal(eff.Output, &o)
					it.CoverRid = o.CoverRid
				}
			}
		}
		rids = append(rids, it.SourceThumbRid, it.CoverRid)
		out.Items = append(out.Items, it)
	}
	if urls, exp := s.signRIDs(ctx, rids); urls != nil {
		for i := range out.Items {
			out.Items[i].SourceThumbURL, out.Items[i].CoverURL = urls[out.Items[i].SourceThumbRid], urls[out.Items[i].CoverRid]
		}
		out.URLExpiresAt = exp
	}
	return out, nil
}
