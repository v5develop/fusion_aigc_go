package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
)

// fakeBillingAPI 按 (用户, 请求号) 幂等的 billing 替身：同一请求号再冻结原样返回已有单（不论当初按什么入口 / 价冻结），
// 与 pkg/billing 冻结按幂等键命中旧单的行为一致。
type fakeBillingAPI struct {
	unit     int64 // 本入口每秒点数
	res      map[string]*billing.Reservation
	status   map[string]string
	refunds  int
	statuses int
}

func newFakeBillingAPI() *fakeBillingAPI {
	return &fakeBillingAPI{unit: 10, res: map[string]*billing.Reservation{}, status: map[string]string{}}
}

func (f *fakeBillingAPI) price(c billing.Charge) int64 { return int64(c.Seconds) * f.unit }

func (f *fakeBillingAPI) Quote(_ context.Context, c billing.Charge) (*billing.Quote, error) {
	return &billing.Quote{EntryCode: c.EntryCode, ModelGroup: c.ModelGroup, PricePoints: f.price(c)}, nil
}

func (f *fakeBillingAPI) Hold(_ context.Context, c billing.Charge) (*billing.Reservation, error) {
	if r, ok := f.res[c.RequestID]; ok {
		return billing.ReplayedReservation(r.No, r.PricePoints, r.Quote), nil
	}
	q := &billing.Quote{EntryCode: c.EntryCode, ModelGroup: c.ModelGroup, PricePoints: f.price(c)}
	r := &billing.Reservation{No: "BR-" + c.RequestID, PricePoints: q.PricePoints, Quote: q}
	f.res[c.RequestID], f.status[c.RequestID] = r, resHeld
	return r, nil
}

// occupy 外部调用方（如图片入口带 Idempotency-Key）先用同一请求号冻结了一张别的单。
func (f *fakeBillingAPI) occupy(requestID, entry, group string, points int64) {
	f.res[requestID] = &billing.Reservation{No: "BR-foreign", PricePoints: points,
		Quote: &billing.Quote{EntryCode: entry, ModelGroup: group, PricePoints: points}}
	f.status[requestID] = resHeld
}

func (f *fakeBillingAPI) Status(_ context.Context, _ int64, rid string) (string, error) {
	f.statuses++
	return f.status[rid], nil
}
func (f *fakeBillingAPI) Load(_ context.Context, _ int64, rid string) (*billing.Reservation, error) {
	return f.res[rid], nil
}
func (f *fakeBillingAPI) Refund(_ context.Context, r *billing.Reservation, _ string) error {
	f.refunds++
	return nil
}
func (f *fakeBillingAPI) Settle(context.Context, int64, string, int) error { return nil }
func (f *fakeBillingAPI) Renew(context.Context, int64, string) error       { return nil }

// 阻塞项：外部 Idempotency-Key 先占了视频重做的请求号，BillingLedger 不得把那张单当成本段的预留。
func TestLedgerHoldRejectsForeignReplay(t *testing.T) {
	ctx := context.Background()
	c := billing.Charge{UserID: uid, RequestID: reqRedo("", 42, 1, 1), EntryCode: EntryCode, ModelGroup: "vid.seedance-2.5",
		Kind: billing.KindVideo, Seconds: 10}
	cases := []struct {
		name         string
		entry, group string
		points       int64
	}{
		{"入口不同（图片入口的单）", "studio_imagegen", "vid.seedance-2.5", 100},
		{"模型组不同", EntryCode, "vid.other", 100},
		{"点数不同（便宜单）", EntryCode, "vid.seedance-2.5", 1},
	}
	for _, tc := range cases {
		f := newFakeBillingAPI()
		f.occupy(c.RequestID, tc.entry, tc.group, tc.points)
		l := &BillingLedger{b: f}
		held, err := l.Hold(ctx, c)
		if held || !errors.Is(err, ErrHoldMismatch) {
			t.Fatalf("%s：必须拒绝复用并返回 ErrHoldMismatch：held=%v err=%v", tc.name, held, err)
		}
		if f.statuses != 0 || f.refunds != 0 {
			t.Fatalf("%s：不一致时不得当成已预留、也不动那张单：status=%d refund=%d", tc.name, f.statuses, f.refunds)
		}
	}

	// 自己的重放（同入口 / 同模型组 / 同价）照常算已预留；新冻结也照常
	f := newFakeBillingAPI()
	l := &BillingLedger{b: f}
	if held, err := l.Hold(ctx, c); !held || err != nil {
		t.Fatalf("新冻结应成功：%v %v", held, err)
	}
	if held, err := l.Hold(ctx, c); !held || err != nil {
		t.Fatalf("同一次预留的重放应命中原单：%v %v", held, err)
	}
	// 报价随时间变了（重放时价不同）也拒绝：不按旧价 / 新价混用
	f.unit = 11
	if held, err := l.Hold(ctx, c); held || !errors.Is(err, ErrHoldMismatch) {
		t.Fatalf("价已变的旧单不得复用：%v %v", held, err)
	}
}

// 阻塞项：新 job 的请求号带内部盐，外部 Idempotency-Key 占用旧格式 replicate-<job>-01-u1 时，视频重做不复用其冻结单。
func TestRedoNotReuseExternallyOccupiedRequestID(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.confirm(pv, "", "k1")
	salt := e.salt(jv.JobID)
	if salt == "" || !strings.HasPrefix(e.charges(jv.JobID)[0].RequestID, "replicate."+salt+"-") {
		t.Fatalf("新 job 的请求号必须带内部盐：salt=%q rid=%s", salt, e.charges(jv.JobID)[0].RequestID)
	}
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	jv = e.run(jv.JobID, 5)
	if jv.Status != "failed" {
		t.Fatalf("应失败：%+v", jv)
	}
	// 外部调用方先用旧格式请求号冻结了一张 1 点的单
	legacy := reqRedo("", jv.JobID, 1, 1)
	e.ledger.res[legacy] = &fakeRes{status: "HELD", points: 1}
	holds := e.ledger.holds
	jv, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"})
	if err != nil || jv.Status != "running" {
		t.Fatalf("重做应成功：%v %+v", err, jv)
	}
	var redo *model.Charge
	for _, c := range e.charges(jv.JobID) {
		if c.Stage == model.ChargeStageRedo {
			redo = c
		}
	}
	if redo == nil || redo.RequestID == legacy || redo.RequestID != e.rRedo(jv.JobID, 1, 1) {
		t.Fatalf("重做请求号不得与外部旧格式碰撞：%+v", redo)
	}
	if e.ledger.holds != holds+1 || e.ledger.res[redo.RequestID].points != 100 {
		t.Fatalf("重做必须按本段价新冻结：holds=%d→%d %+v", holds, e.ledger.holds, e.ledger.res[redo.RequestID])
	}
	if r := e.ledger.res[legacy]; r.status != "HELD" || r.points != 1 {
		t.Fatalf("外部的单不得被动：%+v", r)
	}
	jv = e.run(jv.JobID, 3)
	if jv.Status != "succeeded" || jv.Money.Confirmed != 100 {
		t.Fatalf("重做成功扣本段价：%+v", jv.Money)
	}
}

// 即便请求号被猜中（命中别处的单 → ErrHoldMismatch），重做按预留失败处理：不出片、不按请求号退回别人的单。
func TestRedoHoldMismatchNotTreatedAsHeld(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.confirm(pv, "", "k1")
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "NO_CAPACITY"}}
	jv = e.run(jv.JobID, 5)
	rid := e.rRedo(jv.JobID, 1, 1)
	e.ledger.res[rid] = &fakeRes{status: "HELD", points: 1}
	e.ledger.foreign = map[string]bool{rid: true}
	if _, err := e.svc.Redo(e.ctx, RedoReq{UserID: uid, JobID: jv.JobID, SegIndex: 1, IdempotencyKey: "r1"}); err == nil {
		t.Fatal("命中别处的单必须拒绝重做")
	}
	if r := e.ledger.res[rid]; r.status != "HELD" {
		t.Fatalf("不得按请求号退回别人的单：%+v", r)
	}
	for _, c := range e.charges(jv.JobID) {
		if c.Stage == model.ChargeStageRedo && c.Status != model.ChargeHoldFailed {
			t.Fatalf("重做镜像应记预留失败：%+v", c)
		}
	}
	if st := e.step(jv.JobID, "seg/01/generate#u1"); st == nil || st.Status != model.StepCanceled {
		t.Fatalf("重做步骤应取消、不出片：%+v", st)
	}
}

// 中：自动继续只按确认时冻结的其余段点数；价变则停在待继续等用户确认，不按新价冻结。
func TestAutoContinuePriceChangedStaysWaiting(t *testing.T) {
	e := newEnv(t, true)
	_ = e.svc.SetAutoContinue(e.ctx, uid, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.confirm(pv, "first", "k1")
	j := e.job(jv.JobID)
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	rest := pv.Cost.Total - pv.Cost.First
	if !in.AutoContinue || in.RestPoints != rest {
		t.Fatalf("确认时应冻结自动继续与其余段点数 %d：%+v", rest, in)
	}
	holds := e.ledger.holds
	e.ledger.unit = 11 // 第 1 段出片期间涨价
	jv = e.run(jv.JobID, 3)
	if jv.Status != "waiting" {
		t.Fatalf("价变不得自动继续，应停在待继续：%+v", jv)
	}
	if e.ledger.holds != holds || len(e.charges(jv.JobID)) != 1 {
		t.Fatalf("价变不得按新价冻结其余段：holds=%d→%d charges=%d", holds, e.ledger.holds, len(e.charges(jv.JobID)))
	}
	if cur := e.job(jv.JobID); cur.ContinueAttempt != 0 || cur.ContinueRequestID != nil {
		t.Fatalf("价变不应占用继续：%+v", cur)
	}
	// 用户看到新价后手动确认继续
	newRest := jv.Decision.Options[0].Points
	if newRest == rest {
		t.Fatalf("决策卡应显示新价：%d", newRest)
	}
	jv, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1", ExpectedPoints: newRest})
	if err != nil || jv.Status != "running" {
		t.Fatalf("用户确认新价后继续应成功：%v %+v", err, jv)
	}
	jv = e.run(jv.JobID, 4)
	if jv.Status != "succeeded" {
		t.Fatalf("%+v", jv)
	}
	e.assertMoney(jv.JobID)
}

// 确认时没勾「以后直接全部做」（没冻结自动继续），之后才打开：这一单不自动继续，等用户确认。
func TestAutoContinueNotFrozenAtConfirm(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.confirm(pv, "first", "k1")
	_ = e.svc.SetAutoContinue(e.ctx, uid, true)
	jv = e.run(jv.JobID, 3)
	if jv.Status != "waiting" || len(e.charges(jv.JobID)) != 1 {
		t.Fatalf("确认时未冻结自动继续，不得自动预留其余段：%+v", jv)
	}
}
