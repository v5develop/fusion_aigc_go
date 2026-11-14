package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"
)

var ctx = context.Background()

// ---- 替身：计费（语义同 pkg/billing：锁内判断、已退回幂等、流水按幂等键唯一且带描述）----

type res struct {
	status   string
	points   int64
	consumed int64
}

type logRow struct {
	amount int64
	desc   string
}

type fakeBill struct {
	mu       sync.Mutex
	on       bool
	byReq    map[string]*res
	logs     map[string]logRow // 幂等键 → 流水
	refunds  int               // 实际动账次数（幂等命中不算）
	failOnce map[string]bool
	// gate 测试钩子：Refund 进入锁之前调用（制造并发交错）
	gate func()
	// onLoad 测试钩子：Load 之前调用（模拟读到 HELD 之后被轮询器确认扣除）
	onLoad func(req string)
	// noLog 退回成功但不写流水（模拟流水缺失）
	noLog bool
}

func (b *fakeBill) On() bool { return b.on }

func (b *fakeBill) LoadReservation(_ context.Context, _ int64, req string) (*billing.ReservationBrief, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.byReq[req]
	if r == nil {
		return nil, nil
	}
	return &billing.ReservationBrief{No: "BR" + req, Status: r.status, PricePoints: r.points}, nil
}

func (b *fakeBill) ConsumedPoints(_ context.Context, br *billing.ReservationBrief) (int64, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byReq[br.No[2:]].consumed, 0, nil
}

func (b *fakeBill) Load(_ context.Context, _ int64, req string) (*billing.Reservation, error) {
	if b.onLoad != nil {
		b.onLoad(req)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.byReq[req]
	if r == nil || r.status != "HELD" {
		return nil, nil
	}
	return &billing.Reservation{No: "BR" + req, PricePoints: r.points}, nil
}

func (b *fakeBill) Refund(_ context.Context, rv *billing.Reservation, reason string) error {
	if b.gate != nil {
		b.gate()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	req := rv.No[2:]
	if b.failOnce[req] {
		delete(b.failOnce, req)
		return errors.New("db down")
	}
	r := b.byReq[req]
	switch r.status {
	case "REFUNDED":
		return nil
	case "HELD":
		r.status = "REFUNDED"
		if !b.noLog {
			b.logs["refunded:"+rv.No] = logRow{r.points, reason}
		}
		b.refunds++
		return nil
	}
	return billing.ErrGone
}

func (b *fakeBill) RefundConfirmed(_ context.Context, _ int64, req, reason string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failOnce[req] {
		delete(b.failOnce, req)
		return 0, errors.New("db down")
	}
	r := b.byReq[req]
	switch r.status {
	case "REFUNDED":
		return 0, nil
	case "CONFIRMED", "PARTIALLY_REFUNDED":
		r.status = "REFUNDED"
		b.logs["post-refund:BR"+req] = logRow{r.consumed, "人工处理退回：" + reason}
		b.refunds++
		return r.consumed, nil
	}
	return 0, billing.ErrGone
}

func (b *fakeBill) set(req, status string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.byReq[req].status = status
}

// ---- 替身：业务表 ----

type fakeStore struct {
	mu      sync.Mutex
	bill    *fakeBill
	job     *model.Job
	steps   []*model.Step
	charges []*model.Charge
	actions []*model.AdminAction
	lines   []*model.AdminRefundLine
	now     func() time.Time
}

func cp[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (s *fakeStore) GetJob(_ context.Context, id int64) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil || s.job.ID != id {
		return nil, nil
	}
	return cp(s.job), nil
}
func (s *fakeStore) ListSteps(context.Context, int64) ([]*model.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Step
	for _, x := range s.steps {
		out = append(out, cp(x))
	}
	return out, nil
}
func (s *fakeStore) ListCharges(context.Context, int64) ([]*model.Charge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Charge
	for _, x := range s.charges {
		out = append(out, cp(x))
	}
	return out, nil
}
func (s *fakeStore) UpdateCharge(_ context.Context, id int64, from []int8, set map[string]any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.charges {
		if c.ID != id {
			continue
		}
		for _, f := range from {
			if c.Status == f {
				c.Status = set["status"].(int8)
				return true, nil
			}
		}
	}
	return false, nil
}
func (s *fakeStore) AdminListJobs(_ context.Context, f repo.JobFilter, _, _ int) ([]*model.Job, int64, error) {
	if f.UserID != 0 && f.UserID != s.job.UserID {
		return nil, 0, nil
	}
	return []*model.Job{cp(s.job)}, 1, nil
}
func (s *fakeStore) AdminGetPlan(context.Context, int64) (*model.Plan, error) { return nil, nil }
func (s *fakeStore) ListModerations(context.Context, int64) ([]*model.Moderation, error) {
	return nil, nil
}
func (s *fakeStore) ListModerationLogs(context.Context, string) ([]repo.ModerationLogRow, error) {
	return nil, nil
}
func (s *fakeStore) ListDeliveries(context.Context, int64) ([]*model.Delivery, error) {
	return nil, nil
}
func (s *fakeStore) ListAdminActions(context.Context, int64) ([]*model.AdminAction, error) {
	return s.actions, nil
}
func (s *fakeStore) GetAdminAction(_ context.Context, jobID int64, action, key string) (*model.AdminAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.actions {
		if a.JobID == jobID && a.Action == action && a.RequestKey == key {
			return cp(a), nil
		}
	}
	return nil, nil
}
func (s *fakeStore) BeginAdminAction(_ context.Context, a *model.AdminAction, ttl time.Duration) (*model.AdminAction, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.actions {
		if x.JobID == a.JobID && x.Action == a.Action && x.RequestKey == a.RequestKey {
			return cp(x), false, nil
		}
	}
	c := cp(a)
	c.ID = int64(len(s.actions) + 1)
	until := s.now().Add(ttl)
	c.LeaseUntil = &until
	s.actions = append(s.actions, c)
	return cp(c), true, nil
}
func (s *fakeStore) tokenOK(actionID int64, token string) bool {
	for _, x := range s.actions {
		if x.ID == actionID {
			return x.LeaseToken != nil && *x.LeaseToken == token
		}
	}
	return false
}
func (s *fakeStore) FinishAdminAction(_ context.Context, id int64, token string, status string, points int64, result []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tokenOK(id, token) {
		return false, nil
	}
	for _, x := range s.actions {
		if x.ID == id {
			x.Status, x.Points, x.Result, x.LeaseUntil = status, points, json.RawMessage(result), nil
		}
	}
	return true, nil
}
func (s *fakeStore) TakeOverAdminAction(_ context.Context, id int64, token string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.actions {
		expired := x.Status == model.AdminActionRunning && (x.LeaseUntil == nil || x.LeaseUntil.Before(s.now()))
		if x.ID == id && (x.Status == model.AdminActionFailed || expired) {
			until := s.now().Add(ttl)
			x.Status, x.LeaseUntil, x.LeaseToken = model.AdminActionRunning, &until, &token
			return true, nil
		}
	}
	return false, nil
}
func (s *fakeStore) UpsertRefundLine(_ context.Context, l *model.AdminRefundLine) (*model.AdminRefundLine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.lines {
		if x.ActionID == l.ActionID && x.RequestID == l.RequestID {
			if x.Status != model.RefundLineDone { // 同键重试：重置为 PENDING 并刷新
				x.Status, x.Error, x.ExpectedPoints, x.BeforeStatus, x.ReservationNo = model.RefundLinePending, nil, l.ExpectedPoints, l.BeforeStatus, l.ReservationNo
			}
			return cp(x), nil
		}
	}
	c := cp(l)
	c.ID = int64(len(s.lines) + 1)
	s.lines = append(s.lines, c)
	return cp(c), nil
}
func (s *fakeStore) FinishRefundLine(_ context.Context, id int64, token string, status string, points int64, errMsg string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.lines {
		if x.ID == id {
			if !s.tokenOK(x.ActionID, token) {
				return false, nil
			}
			x.Status, x.Points, x.Error = status, points, nil
			if errMsg != "" {
				x.Error = &errMsg
			}
			return true, nil
		}
	}
	return false, nil
}
func (s *fakeStore) ListRefundLines(_ context.Context, actionID int64) ([]*model.AdminRefundLine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.AdminRefundLine
	for _, x := range s.lines {
		if x.ActionID == actionID {
			out = append(out, cp(x))
		}
	}
	return out, nil
}
func (s *fakeStore) ListJobRefundLines(context.Context, int64) ([]*model.AdminRefundLine, error) {
	return s.lines, nil
}
func (s *fakeStore) RefundLog(_ context.Context, _ int64, key string) (int64, string, bool, error) {
	s.bill.mu.Lock()
	defer s.bill.mu.Unlock()
	l, ok := s.bill.logs[key]
	return l.amount, l.desc, ok, nil
}

type fakeForcer struct{ calls int }

func (f *fakeForcer) ForceFinish(_ context.Context, id int64) (*core.ForceFinishResult, error) {
	f.calls++
	return &core.ForceFinishResult{JobID: id, Status: "failed"}, nil
}

func ptr[T any](v T) *T { return &v }

// 场景：3 段 job——第 1 段成功已扣 120；第 2 段失败、预留仍 HELD（退点待重试）80；第 3 段还在生成（HELD 100）。
func setup() (*Service, *fakeStore, *fakeBill) {
	b := &fakeBill{on: true, byReq: map[string]*res{
		"r1": {status: "CONFIRMED", points: 120, consumed: 120},
		"r2": {status: "HELD", points: 80},
		"r3": {status: "HELD", points: 100},
	}, logs: map[string]logRow{}, failOnce: map[string]bool{}}
	st := &fakeStore{bill: b, now: time.Now, job: &model.Job{ID: 42, UserID: 7, Status: model.JobRunning, SegTotal: 3, PlanNo: "VRPx", Input: json.RawMessage(`{}`)}}
	st.steps = []*model.Step{
		{ID: 1, JobID: 42, StepKey: "seg/01/generate", Kind: model.StepKindGateway, SegIndex: 1, Status: model.StepSucceeded, BillingRequestID: ptr("r1"), GatewayCallID: ptr("call_1")},
		{ID: 2, JobID: 42, StepKey: "seg/02/generate", Kind: model.StepKindGateway, SegIndex: 2, Status: model.StepFailed, BillingRequestID: ptr("r2"), ErrorCode: ptr("VENDOR_FAILED")},
		{ID: 3, JobID: 42, StepKey: "seg/03/generate", Kind: model.StepKindGateway, SegIndex: 3, Status: model.StepRunning, BillingRequestID: ptr("r3")},
	}
	st.charges = []*model.Charge{
		{ID: 11, JobID: 42, SegIndex: 1, RequestID: "r1", Points: 120, Status: model.ChargeConfirmed},
		{ID: 12, JobID: 42, SegIndex: 2, RequestID: "r2", Points: 80, Status: model.ChargeSettleFailed, Reason: ptr("refund: db down")},
		{ID: 13, JobID: 42, SegIndex: 3, RequestID: "r3", Points: 100, Status: model.ChargeHeld},
	}
	return New(st, b, &fakeForcer{}), st, b
}

func donePoints(lines []*model.AdminRefundLine) (sum int64) {
	for _, l := range lines {
		if l.Status == model.RefundLineDone {
			sum += l.Points
		}
	}
	return
}

func TestDetailRefundableAndFlags(t *testing.T) {
	s, _, _ := setup()
	d, err := s.Detail(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{1: 120, 2: 80, 3: 0}
	for _, seg := range d.Segments {
		if seg.RefundablePoints != want[seg.Index] || seg.CanRefund != (want[seg.Index] > 0) {
			t.Fatalf("第 %d 段可退 %d，应为 %d", seg.Index, seg.RefundablePoints, want[seg.Index])
		}
	}
	if d.Segments[2].Charges[0].BlockedReason == "" {
		t.Fatal("在生成的段要给出不能退的原因")
	}
	if !d.CanRefund || d.CanForce || d.ForceBlocker == "" {
		t.Fatalf("有段在生成：可退点、不可收尾：%+v", d)
	}
	if d.Segments[0].GatewayCallIDs[0] != "call_1" || d.Segments[1].FailReason == "" || d.Charges[1].Mismatch {
		t.Fatalf("网关调用号 / 失败原因 / 过渡态：%+v", d.Segments)
	}
}

func TestRefundGuards(t *testing.T) {
	s, _, b := setup()
	cases := []struct {
		req    RefundReq
		reason string
		code   int
	}{
		{RefundReq{JobID: 42, SegIndexes: []int{1}, Reason: " ", ExpectedPoints: 120}, "", xerr.ParamMissing},
		{RefundReq{JobID: 42, SegIndexes: nil, Reason: "客诉", ExpectedPoints: 120}, "", xerr.ParamMissing},
		{RefundReq{JobID: 42, SegIndexes: []int{1}, Reason: "客诉", ExpectedPoints: 100}, ReasonPointsChanged, xerr.Conflict},
		{RefundReq{JobID: 42, SegIndexes: []int{3}, Reason: "客诉", ExpectedPoints: 0}, ReasonNothingToRefund, xerr.Conflict},
		{RefundReq{JobID: 42, SegIndexes: []int{9}, Reason: "客诉", ExpectedPoints: 0}, "", xerr.ParamError},
	}
	for i, c := range cases {
		_, err := s.Refund(ctx, c.req)
		code, _, _ := xerr.Parse(err)
		if code != c.code || (c.reason != "" && xerr.ReasonOf(err) != c.reason) {
			t.Fatalf("#%d 应拒绝 %d/%s：%v", i, c.code, c.reason, err)
		}
	}
	if b.refunds != 0 {
		t.Fatal("被拒的请求不能动钱")
	}
	b.on = false
	if _, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1}, Reason: "客诉", ExpectedPoints: 120}); xerr.ReasonOf(err) != ReasonBillingOff {
		t.Fatalf("计费没开不能退：%v", err)
	}
}

func TestRefundConfirmedAndHeldWithAuditAndReplay(t *testing.T) {
	s, st, b := setup()
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, AdminID: 1, SegIndexes: []int{1, 2}, Reason: "客诉：画面异常", ExpectedPoints: 200, IdempotencyKey: "k1"})
	if err != nil || resp.Points != 200 || resp.ThisPoints != 200 || len(resp.Lines) != 2 || resp.Replay {
		t.Fatalf("应退 200：%+v %v", resp, err)
	}
	if b.byReq["r1"].status != "REFUNDED" || b.byReq["r2"].status != "REFUNDED" || b.byReq["r3"].status != "HELD" {
		t.Fatal("只动所选段")
	}
	if !strings.HasPrefix(b.logs["refunded:BRr2"].desc, heldReasonPrefix(resp.ActionID)) {
		t.Fatalf("退回原因要带动作标记：%+v", b.logs)
	}
	for _, c := range st.charges[:2] {
		if c.Status != model.ChargeRefunded {
			t.Fatalf("镜像应收敛为已退回：%+v", c)
		}
	}
	if st.actions[0].Status != model.AdminActionDone || st.actions[0].Points != 200 || donePoints(st.lines) != 200 {
		t.Fatalf("审计：%+v %+v", st.actions, st.lines)
	}
	again, err := s.Refund(ctx, RefundReq{JobID: 42, AdminID: 1, SegIndexes: []int{1, 2}, Reason: "客诉：画面异常", ExpectedPoints: 200, IdempotencyKey: "k1"})
	if err != nil || !again.Replay || again.Points != 200 || len(again.Lines) != 2 || b.refunds != 2 {
		t.Fatalf("重放：%+v %v refunds=%d", again, err, b.refunds)
	}
	if _, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1}, Reason: "再退", ExpectedPoints: 120, IdempotencyKey: "k2"}); xerr.ReasonOf(err) != ReasonNothingToRefund {
		t.Fatalf("已退过：%v", err)
	}
}

// 低7：没带幂等键时按 段 + 原因 + 当前预留单号集合 派生；同样的请求重放而不是再退。
func TestRefundDerivedKeyReplays(t *testing.T) {
	s, _, b := setup()
	r1, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 80})
	if err != nil || r1.Replay {
		t.Fatalf("%+v %v", r1, err)
	}
	r2, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 80})
	if err != nil || !r2.Replay || r2.ActionID != r1.ActionID || b.refunds != 1 {
		t.Fatalf("派生键应命中同一动作：%+v %v", r2, err)
	}
}

// 中3：不同管理员（不同键）同时退同一张冻结单——billing 两边都返回成功，但审计只记一次（读流水的动作标记）；
// 同键并发——只有一个执行，其余 IN_PROGRESS / 重放。
func TestRefundConcurrentHeldCountedOnce(t *testing.T) {
	s, st, b := setup()
	var arrived sync.WaitGroup
	release := make(chan struct{})
	const n = 6
	arrived.Add(n)
	var once sync.Once
	b.gate = func() { // 让所有调用都先读到 HELD 再进锁，制造「两边都成功」
		once.Do(func() {})
		arrived.Done()
		<-release
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var this int64
	errs := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := s.Refund(ctx, RefundReq{JobID: 42, AdminID: int64(i), SegIndexes: []int{2}, Reason: "并发",
				ExpectedPoints: 80, IdempotencyKey: "k" + string(rune('a'+i))})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			this += resp.ThisPoints
		}(i)
	}
	arrived.Wait()
	close(release)
	wg.Wait()
	if errs != 0 || b.refunds != 1 || this != 80 || donePoints(st.lines) != 80 {
		t.Fatalf("同一张冻结单只能记一次：errs=%d 实际动账=%d 各动作合计=%d 明细合计=%d", errs, b.refunds, this, donePoints(st.lines))
	}
	var sum int64
	for _, a := range st.actions {
		sum += a.Points
	}
	if sum != 80 {
		t.Fatalf("审计行合计应为 80：%d", sum)
	}
}

func TestRefundConcurrentSameKey(t *testing.T) {
	s, _, b := setup()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1}, Reason: "并发", ExpectedPoints: 120, IdempotencyKey: "same"})
		}()
	}
	wg.Wait()
	if b.refunds != 1 || b.byReq["r1"].status != "REFUNDED" {
		t.Fatalf("同键只执行一次：%d", b.refunds)
	}
}

// 中1：部分失败后同键重试——expectedPoints 只比尚未退成的单；首批明细保留，累计点数正确。
func TestRefundPartialFailureThenRetrySameKey(t *testing.T) {
	s, st, b := setup()
	b.failOnce["r2"] = true
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1, 2}, Reason: "客诉", ExpectedPoints: 200, IdempotencyKey: "k1"})
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Resp != resp || len(pe.Resp.Lines) != 2 || resp.ThisPoints != 120 || !strings.Contains(err.Error(), "用同一请求重试") {
		t.Fatalf("部分失败应报错并如实给出已退：%+v %v", resp, err)
	}
	if st.actions[0].Status != model.AdminActionFailed || st.actions[0].Points != 120 {
		t.Fatalf("审计应记失败与已退 120：%+v", st.actions[0])
	}
	if _, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1, 2}, Reason: "客诉", ExpectedPoints: 200, IdempotencyKey: "k1"}); xerr.ReasonOf(err) != ReasonPointsChanged {
		t.Fatalf("重试时 expectedPoints 只比剩下的 80：%v", err)
	}
	resp, err = s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1, 2}, Reason: "客诉", ExpectedPoints: 80, IdempotencyKey: "k1"})
	if err != nil || resp.ThisPoints != 80 || resp.Points != 200 || len(resp.Lines) != 2 || b.byReq["r2"].status != "REFUNDED" {
		t.Fatalf("同键重试应补退 80、累计 200、保留首批明细：%+v %v", resp, err)
	}
	if st.actions[0].Status != model.AdminActionDone || st.actions[0].Points != 200 {
		t.Fatalf("重试成功后审计 DONE 200：%+v", st.actions[0])
	}
}

// 中2：处理中崩溃（钱已动、明细停在 PENDING、动作 RUNNING）→ 认领过期后同键接管：对账出已动的钱，不重复退。
func TestRefundCrashTakeover(t *testing.T) {
	s, st, b := setup()
	clock := time.Now()
	// 模拟崩溃现场：动作 1 RUNNING，第 2 段明细 PENDING，钱已按动作 1 的标记退了
	st.actions = []*model.AdminAction{{ID: 1, JobID: 42, Action: model.AdminActionRefund, RequestKey: "k1", Reason: "客诉",
		Status: model.AdminActionRunning, LeaseUntil: ptr(clock.Add(time.Minute)), LeaseToken: ptr("old")}}
	st.lines = []*model.AdminRefundLine{{ID: 1, ActionID: 1, JobID: 42, SegIndex: 2, RequestID: "r2", ReservationNo: "BRr2",
		BeforeStatus: "HELD", ExpectedPoints: 80, Status: model.RefundLinePending}}
	b.byReq["r2"].status = "REFUNDED"
	b.logs["refunded:BRr2"] = logRow{80, heldReasonPrefix(1) + "客诉"}
	if _, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 0, IdempotencyKey: "k1"}); xerr.ReasonOf(err) != ReasonInProgress {
		t.Fatalf("认领没过期：处理中：%v", err)
	}
	st.actions[0].LeaseUntil = ptr(time.Now().Add(-time.Second)) // 认领过期（崩溃的执行者不再续）
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 0, IdempotencyKey: "k1"})
	if err != nil || resp.Points != 80 || resp.ThisPoints != 0 || b.refunds != 0 {
		t.Fatalf("接管后应对账出已退的 80、不重复退：%+v %v refunds=%d", resp, err, b.refunds)
	}
	if st.lines[0].Status != model.RefundLineDone || st.lines[0].Points != 80 || st.actions[0].Status != model.AdminActionDone {
		t.Fatalf("明细 / 审计：%+v %+v", st.lines[0], st.actions[0])
	}
}

func TestForceFinishAuditAndBillingOff(t *testing.T) {
	s, st, b := setup()
	f := s.forcer.(*fakeForcer)
	if _, err := s.ForceFinish(ctx, ForceReq{JobID: 42, Reason: ""}); err == nil {
		t.Fatal("原因必填")
	}
	r, err := s.ForceFinish(ctx, ForceReq{JobID: 42, AdminID: 1, Reason: "供应商故障，人工收尾"})
	if err != nil || r.Status != "failed" || f.calls != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = s.ForceFinish(ctx, ForceReq{JobID: 42, AdminID: 1, Reason: "供应商故障，人工收尾"})
	if err != nil || !r.Replay || f.calls != 1 {
		t.Fatalf("派生键（原因 + 未结镜像集合）重放不再执行：%+v %v", r, err)
	}
	// 未结镜像集合变了 → 新动作
	st.charges[2].Status = model.ChargeRefunded
	if r, err = s.ForceFinish(ctx, ForceReq{JobID: 42, AdminID: 1, Reason: "供应商故障，人工收尾"}); err != nil || r.Replay || f.calls != 2 {
		t.Fatalf("状态变了应是新动作：%+v %v", r, err)
	}
	b.on = false
	if _, err := s.ForceFinish(ctx, ForceReq{JobID: 42, Reason: "x", IdempotencyKey: "f2"}); xerr.ReasonOf(err) != ReasonBillingOff {
		t.Fatalf("计费没开不能收尾：%v", err)
	}
}

func TestListStatusFilter(t *testing.T) {
	s, _, _ := setup()
	if _, err := s.List(ctx, ListReq{Status: "bogus"}); err == nil {
		t.Fatal("不认识的状态应报参数错")
	}
	out, err := s.List(ctx, ListReq{Status: "running,partial", Size: 500})
	if err != nil || out.Size != 100 || len(out.Items) != 1 || out.Items[0].Status != "running" {
		t.Fatalf("%+v %v", out, err)
	}
}

// ---------------------------------------------------------------- 复核修复

// 新1：读到 HELD 之后、加载之前被轮询器确认扣除——不能当成「已被其它途径退回」，改走 RefundConfirmed；
// 镜像只在计费真实状态已是退回时才改已退回。
func TestRefundHeldConfirmedRaceUsesRefundConfirmed(t *testing.T) {
	s, st, b := setup()
	b.byReq["r2"].consumed = 80
	b.onLoad = func(req string) {
		if req == "r2" && b.byReq["r2"].status == "HELD" {
			b.set("r2", "CONFIRMED") // 轮询器在这一瞬间交付并确认扣除
		}
	}
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 80, IdempotencyKey: "k1"})
	if err != nil || resp.ThisPoints != 80 || b.byReq["r2"].status != "REFUNDED" {
		t.Fatalf("应改走 RefundConfirmed 退回 80：%+v %v 状态=%s", resp, err, b.byReq["r2"].status)
	}
	if st.charges[1].Status != model.ChargeRefunded || !strings.HasPrefix(b.logs["post-refund:BRr2"].desc, confirmedDescPrefix+heldReasonPrefix(resp.ActionID)) {
		t.Fatalf("镜像 / 流水归属：%+v %+v", st.charges[1], b.logs)
	}
}

// 新1：确认后再退也失败 → 明细 FAILED，镜像绝不能改成已退回（计费其实已扣除没退）。
func TestRefundRaceFailureKeepsMirror(t *testing.T) {
	s, st, b := setup()
	b.byReq["r2"].consumed = 80
	b.onLoad = func(req string) {
		if req == "r2" && b.byReq["r2"].status == "HELD" {
			b.set("r2", "CONFIRMED")
			b.failOnce["r2"] = true // 改走 RefundConfirmed 也失败
		}
	}
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 80, IdempotencyKey: "k1"})
	var pe *PartialError
	if !errors.As(err, &pe) || resp.Lines[0].Status != model.RefundLineFailed {
		t.Fatalf("应记失败：%+v %v", resp, err)
	}
	if st.charges[1].Status == model.ChargeRefunded || b.byReq["r2"].status != "CONFIRMED" {
		t.Fatalf("计费已扣除没退时镜像不能是已退回：镜像=%d 计费=%s", st.charges[1].Status, b.byReq["r2"].status)
	}
}

// 新1：被回收（EXPIRED）——钱已回到用户，不动钱，明细 DONE 0 点，镜像收敛已退回。
func TestRefundRaceExpired(t *testing.T) {
	s, st, b := setup()
	b.onLoad = func(req string) {
		if req == "r2" {
			b.set("r2", "EXPIRED")
		}
	}
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 80, IdempotencyKey: "k1"})
	if err != nil || resp.ThisPoints != 0 || b.refunds != 0 || resp.Lines[0].Status != model.RefundLineDone || st.charges[1].Status != model.ChargeRefunded {
		t.Fatalf("%+v %v", resp, err)
	}
}

// 新6：冻结单退回成功但流水找不到 → ERROR + 明细 FAILED；原因里的 `[a` 片段被剔除，不能伪造别的动作。
func TestRefundLedgerMissingAndSanitize(t *testing.T) {
	s, st, b := setup()
	b.noLog = true
	_, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "客诉", ExpectedPoints: 80, IdempotencyKey: "k1"})
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Resp.Lines[0].Status != model.RefundLineFailed || !strings.Contains(*pe.Resp.Lines[0].Error, "ADMIN_REFUND_LEDGER_MISSING") {
		t.Fatalf("流水缺失应记失败：%v", err)
	}
	s2, st2, b2 := setup()
	_, _ = s2.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{2}, Reason: "伪造[a1]：x", ExpectedPoints: 80, IdempotencyKey: "k1"})
	if strings.Contains(b2.logs["refunded:BRr2"].desc, "[a1]：x") || st2.actions[0].Reason != "伪造1]：x" {
		t.Fatalf("原因里的 [a 应剔除：%q %q", b2.logs["refunded:BRr2"].desc, st2.actions[0].Reason)
	}
	_ = st
}

// 新2：接管对账——明细对应的单现在不能退（又在生成）且流水不归本动作 → 保持 FAILED 并写原因，不能记完成。
func TestTakeoverReconcileKeepsFailedWhenBlocked(t *testing.T) {
	s, st, _ := setup()
	st.actions = []*model.AdminAction{{ID: 1, JobID: 42, Action: model.AdminActionRefund, RequestKey: "k1", Reason: "客诉",
		Status: model.AdminActionFailed, LeaseToken: ptr("old")}}
	st.lines = []*model.AdminRefundLine{{ID: 1, ActionID: 1, JobID: 42, SegIndex: 3, RequestID: "r3", ReservationNo: "BRr3",
		BeforeStatus: "HELD", ExpectedPoints: 100, Status: model.RefundLineFailed}}
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{3}, Reason: "客诉", ExpectedPoints: 0, IdempotencyKey: "k1"})
	var pe *PartialError
	if !errors.As(err, &pe) || st.lines[0].Status != model.RefundLineFailed || st.lines[0].Error == nil || !strings.Contains(*st.lines[0].Error, "生成中") {
		t.Fatalf("应保持 FAILED 并写原因：%+v %v %+v", resp, err, st.lines[0])
	}
}

// 新7：被接管后旧执行者的结果写回落空；初始化断言。
func TestTokenFencingAndLeaseInvariant(t *testing.T) {
	if actionLease <= workTimeout+30*time.Second {
		t.Fatal("认领期必须大于脱离上下文时限 + 30s")
	}
	_, st, _ := setup()
	st.actions = []*model.AdminAction{{ID: 1, JobID: 42, Action: model.AdminActionRefund, RequestKey: "k1", Status: model.AdminActionRunning,
		LeaseUntil: ptr(time.Now().Add(-time.Second)), LeaseToken: ptr("old")}}
	st.lines = []*model.AdminRefundLine{{ID: 1, ActionID: 1, RequestID: "r1", Status: model.RefundLinePending}}
	if ok, _ := st.TakeOverAdminAction(ctx, 1, "new", time.Minute); !ok {
		t.Fatal("过期可接管")
	}
	if ok, _ := st.FinishRefundLine(ctx, 1, "old", model.RefundLineDone, 120, ""); ok {
		t.Fatal("旧令牌写明细应落空")
	}
	if ok, _ := st.FinishAdminAction(ctx, 1, "old", model.AdminActionDone, 120, nil); ok {
		t.Fatal("旧令牌写审计应落空")
	}
	if ok, _ := st.FinishRefundLine(ctx, 1, "new", model.RefundLineDone, 120, ""); !ok {
		t.Fatal("新令牌可以写")
	}
}

// 新8：同键重试把 FAILED 明细重置为 PENDING 并刷新可退点数 / 退前状态。
func TestRetryResetsFailedLine(t *testing.T) {
	s, st, b := setup()
	b.failOnce["r1"] = true
	_, _ = s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1}, Reason: "客诉", ExpectedPoints: 120, IdempotencyKey: "k1"})
	if st.lines[0].Status != model.RefundLineFailed {
		t.Fatalf("首次失败：%+v", st.lines[0])
	}
	b.byReq["r1"].consumed = 90 // 期间有过补退，可退点数变了
	b.byReq["r1"].status = "PARTIALLY_REFUNDED"
	resp, err := s.Refund(ctx, RefundReq{JobID: 42, SegIndexes: []int{1}, Reason: "客诉", ExpectedPoints: 90, IdempotencyKey: "k1"})
	if err != nil || resp.ThisPoints != 90 || st.lines[0].Status != model.RefundLineDone || st.lines[0].ExpectedPoints != 90 ||
		st.lines[0].BeforeStatus != "PARTIALLY_REFUNDED" || st.lines[0].Error != nil {
		t.Fatalf("重试应按当前状态重来：%+v %v %+v", resp, err, st.lines[0])
	}
}
