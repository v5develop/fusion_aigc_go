package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/notify"
)

// ---- 上线补齐（审核 / 通知 / 入库 / 强制收尾）的替身 ----

type memOps struct {
	mu    sync.Mutex
	mods  map[string]*model.Moderation
	logs  []ModerationLog
	dels  []*model.Delivery
	id    int64
	clock func() time.Time
	touch map[int64]int
	// stealOnRenew 模拟认领在复制期间被别的实例接手
	stealOnRenew bool
}

func newMemOps(clock func() time.Time) *memOps {
	return &memOps{mods: map[string]*model.Moderation{}, clock: clock, touch: map[int64]int{}}
}

func (m *memOps) GetModeration(_ context.Context, planID int64, hash string) (*model.Moderation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.mods[fmt.Sprintf("%d|%s", planID, hash)]), nil
}

func (m *memOps) PutModeration(_ context.Context, x *model.Moderation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := clone(x)
	if m.clock != nil {
		c.UpdatedAt = m.clock()
	}
	m.mods[fmt.Sprintf("%d|%s", x.PlanID, x.PlanHash)] = c
	return nil
}

func (m *memOps) LogModeration(_ context.Context, l ModerationLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(m.logs, l)
	return nil
}

func (m *memOps) EnqueueDelivery(_ context.Context, d *model.Delivery) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.dels {
		if x.JobID == d.JobID && x.Kind == d.Kind && x.SegIndex == d.SegIndex && x.Version == d.Version {
			return false, nil
		}
	}
	m.id++
	c := clone(d)
	c.ID = m.id
	m.dels = append(m.dels, c)
	return true, nil
}

func (m *memOps) ListDeliveries(_ context.Context, jobID int64) ([]*model.Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.Delivery
	for _, x := range m.dels {
		if x.JobID == jobID {
			out = append(out, clone(x))
		}
	}
	return out, nil
}

func (m *memOps) DueDeliveries(_ context.Context, limit int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	var out []int64
	for _, x := range m.dels {
		if x.Status == model.DeliveryPending && (x.NextRunAt == nil || !x.NextRunAt.After(now)) && (x.LeaseUntil == nil || x.LeaseUntil.Before(now)) {
			out = append(out, x.ID)
		}
	}
	return out, nil
}

func (m *memOps) find(id int64) *model.Delivery {
	for _, x := range m.dels {
		if x.ID == id {
			return x
		}
	}
	return nil
}

func (m *memOps) ClaimDelivery(_ context.Context, id int64, token string, ttl time.Duration) (*model.Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x := m.find(id)
	now := m.clock()
	if x == nil || x.Status != model.DeliveryPending || (x.LeaseUntil != nil && !x.LeaseUntil.Before(now)) || (x.NextRunAt != nil && x.NextRunAt.After(now)) {
		return nil, nil
	}
	until := now.Add(ttl)
	x.LeaseToken, x.LeaseUntil = &token, &until
	x.Attempts++
	return clone(x), nil
}

func (m *memOps) FinishDelivery(_ context.Context, id int64, token string, set map[string]any, retryAfter time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x := m.find(id)
	if x == nil || x.LeaseToken == nil || *x.LeaseToken != token {
		return false, nil
	}
	apply(x, set)
	if retryAfter > 0 {
		t := m.clock().Add(retryAfter)
		x.NextRunAt = &t
	}
	return true, nil
}

func (m *memOps) RenewDelivery(_ context.Context, id int64, token string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x := m.find(id)
	if x == nil || x.LeaseToken == nil || *x.LeaseToken != token || x.Status != model.DeliveryPending || m.stealOnRenew {
		return false, nil
	}
	until := m.clock().Add(ttl)
	x.LeaseUntil = &until
	return true, nil
}

func (m *memOps) TouchTaskCenter(_ context.Context, jobID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touch[jobID]++
	return nil
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent map[string]notify.Message
	err  error
}

func (f *fakeNotifier) Send(_ context.Context, m notify.Message) (notify.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return notify.Skipped, f.err
	}
	if f.sent == nil {
		f.sent = map[string]notify.Message{}
	}
	if _, ok := f.sent[m.DedupeKey]; ok {
		return notify.Duplicate, nil
	}
	f.sent[m.DedupeKey] = m
	return notify.Sent, nil
}

func (f *fakeNotifier) byKind(kind string) []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []notify.Message
	for _, m := range f.sent {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

type fakeLib struct {
	mu     sync.Mutex
	errs   []error
	copies []string
}

func (f *fakeLib) CopyToLibrary(_ context.Context, rid, purpose, idem string) (mediasvc.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies = append(f.copies, idem)
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		if e != nil {
			return mediasvc.Resource{}, e
		}
	}
	if purpose != mediasvc.PurposeAssetLibrary {
		return mediasvc.Resource{}, errors.New("必须落素材库公有桶")
	}
	return mediasvc.Resource{RID: "lib-" + rid, URL: "https://pub.test/" + rid}, nil
}

type fakeAssets struct {
	mu   sync.Mutex
	rows []asset.Asset
	err  error
}

func (f *fakeAssets) InsertOnceByURL(_ context.Context, a asset.Asset) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	for _, r := range f.rows {
		if r.SourceTaskID == a.SourceTaskID && r.ResultURL == a.ResultURL && r.UserID == a.UserID {
			return false, nil
		}
	}
	f.rows = append(f.rows, a)
	return true, nil
}

type fakeWords struct{ hit string }

func (f fakeWords) Check(_ context.Context, _, text string) (moderation.Result, error) {
	if f.hit != "" && strings.Contains(text, f.hit) {
		return moderation.Result{Passed: false, Label: f.hit}, nil
	}
	return moderation.Result{Passed: true}, nil
}

// newOpsEnv 同 newEnv，外加 cfg 修改与额外装配。
func newOpsEnv(t *testing.T, withCutter bool, mut func(*Config), extra ...func(e *env) Option) *env {
	e := newEnv(t, withCutter)
	opts := []Option{WithGateway(e.gw), WithMedia(e.media), WithClock(func() time.Time { return e.clock }), WithOwner("test-owner"),
		WithURLOwner(testOwner), WithSigner(e.signer)}
	if withCutter {
		opts = append(opts, WithCutter(e.cutter))
	}
	for _, x := range extra {
		opts = append(opts, x(e))
	}
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	cfg.ModerationPlanWait = 5 * time.Second // 已有用例按「方案卡直接带结论」断言：替身网关秒回，等到即返回；异步口径的用例改回 0
	if mut != nil {
		mut(&cfg)
	}
	e.svc = New(cfg, e.store, e.ledger, opts...)
	return e
}

func verdicts(v ...string) json.RawMessage {
	items := make([]string, len(v))
	for i, x := range v {
		items[i] = fmt.Sprintf(`{"verdict":%q,"labels":[{"label":"porn","vendor_label":"p1","score":0.97}]}`, x)
	}
	return json.RawMessage(`{"operation":"moderation","results":[` + strings.Join(items, ",") + `]}`)
}

// ---------------------------------------------------------------- 审核

func TestModerationRejectedRefBlocksConfirmWithoutHold(t *testing.T) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	e.gw.analysis[opModerationImage] = verdicts("block")
	pv := e.plan(e.seedAnalysis(10))
	if pv.Moderation == nil || pv.Moderation.Status != "rejected" || pv.Moderation.Text != modTextRef {
		t.Fatalf("方案卡应显示参考图没通过：%+v", pv.Moderation)
	}
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != ReasonModerationRejected || !strings.Contains(err.Error(), "参考图") || strings.Contains(err.Error(), "porn") {
		t.Fatalf("应拒绝确认且不暴露命中标签：%v", err)
	}
	if e.ledger.holds != 0 {
		t.Fatalf("审核不通过不能预留点数：holds=%d", e.ledger.holds)
	}
	if j, _ := e.store.GetJobByRequest(e.ctx, uid, "k1"); j != nil {
		t.Fatal("审核不通过不能建 job")
	}
	// 结论缓存：确认时没有再调网关
	img := 0
	for _, k := range e.gw.submits {
		if strings.Contains(k, "mod:") {
			img++
		}
	}
	if img != 1 {
		t.Fatalf("同一版方案只审一次：%d", img)
	}
	if len(ops.logs) != 1 || ops.logs[0].BizType != ModBizImage || ops.logs[0].ResultStatus != ModResultRejected ||
		ops.logs[0].RequestID != "replicate:"+pv.PlanNo || !strings.Contains(ops.logs[0].Label, "porn") || ops.logs[0].OutTaskID == "" {
		t.Fatalf("审核日志不对：%+v", ops.logs)
	}
}

func TestModerationReviewAsBlockConfigurable(t *testing.T) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, func(c *Config) { c.ModerationReviewPass = true }, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	e.gw.analysis[opModerationImage] = verdicts("review")
	pv := e.plan(e.seedAnalysis(10))
	if pv.Moderation.Status != "passed" {
		t.Fatalf("review 放行时应通过：%+v", pv.Moderation)
	}
	jv := e.confirm(pv, "", "k1")
	if jv.Money.Held != 100 {
		t.Fatalf("通过后应正常预留：%+v", jv.Money)
	}

	ops2 := newMemOps(nil)
	e2 := newOpsEnv(t, false, nil, func(*env) Option { return WithModeration(ops2, ops2, nil) })
	ops2.clock = func() time.Time { return e2.clock }
	e2.gw.analysis[opModerationImage] = verdicts("review")
	if pv := e2.plan(e2.seedAnalysis(10)); pv.Moderation.Status != "rejected" {
		t.Fatalf("默认 review 按违规：%+v", pv.Moderation)
	}
}

func TestModerationFailCloseAndFailOpen(t *testing.T) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	// 没配审核结果：假网关回一个读不懂的结果 → 审核服务故障
	pv := e.plan(e.seedAnalysis(10))
	if pv.Moderation.Status != "pending" {
		t.Fatalf("生成方案时审核故障不挡方案：%+v", pv.Moderation)
	}
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != ReasonModerationUnavailable || e.ledger.holds != 0 {
		t.Fatalf("fail-close：故障不让确认、不预留：%v holds=%d", err, e.ledger.holds)
	}
	if m, _ := ops.GetModeration(e.ctx, 1, pv.PlanHash); m != nil {
		t.Fatalf("故障结论不落库（下次重审）：%+v", m)
	}
	if len(ops.logs) == 0 || ops.logs[0].ResultStatus != ModResultError || ops.logs[0].Passed {
		t.Fatalf("故障也要落日志：%+v", ops.logs)
	}

	ops2 := newMemOps(nil)
	e2 := newOpsEnv(t, false, func(c *Config) { c.ModerationFailOpen = true }, func(*env) Option { return WithModeration(ops2, ops2, nil) })
	ops2.clock = func() time.Time { return e2.clock }
	pv2 := e2.plan(e2.seedAnalysis(10))
	jv := e2.confirm(pv2, "", "k1")
	if jv.Money.Held != 100 {
		t.Fatalf("fail-open 放行：%+v", jv.Money)
	}
	p, _ := e2.store.GetPlanByNo(e2.ctx, uid, pv2.PlanNo)
	if m, _ := ops2.GetModeration(e2.ctx, p.ID, p.PlanHash); m == nil || m.Status != model.ModerationErrorOpen {
		t.Fatalf("fail-open 应记 ERROR_OPEN：%+v", m)
	}
}

func TestModerationKeywordAndTextsAndFrames(t *testing.T) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithModeration(ops, ops, fakeWords{hit: "违禁词"}) })
	ops.clock = func() time.Time { return e.clock }
	e.gw.analysis[opModerationImage] = verdicts("pass", "pass", "pass") // 1 张参考图 + 2 帧（VIDEO 那批截前 2 条也够：按条数校验）
	a := e.seedAnalysis(10)
	st, _ := json.Marshal(AnalysisState{Frames: []FrameOut{{T: 1, Rid: "f1"}, {T: 2, Rid: "f2"}}})
	for _, x := range e.store.analyses {
		if x.ID == a.ID {
			x.State = st
		}
	}
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}},
			Texts: []TextOverlay{{Text: "某某店 违禁词 特价", Kind: "shop_name"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Moderation.Status != "rejected" || pv.Moderation.Text != modTextWords || strings.Contains(pv.Moderation.Text, "违禁词") {
		t.Fatalf("关键词命中应拒绝且不暴露命中词：%+v", pv.Moderation)
	}
	for _, k := range e.gw.submits {
		if strings.Contains(k, ":text:") {
			t.Fatalf("关键词已命中就不再调网关文本审核：%s", k)
		}
	}
	var kw bool
	for _, l := range ops.logs {
		kw = kw || (l.Provider == ModProviderKeyword && l.BizType == ModBizText)
	}
	if !kw {
		t.Fatalf("关键词命中要落日志：%+v", ops.logs)
	}
}

// ---------------------------------------------------------------- 通知

func TestNotifyDoneOnceAndFirstReady(t *testing.T) {
	n := &fakeNotifier{}
	e := newOpsEnv(t, true, nil, func(*env) Option { return WithNotifier(n) })
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if jv.Status != "waiting" {
		t.Fatalf("应待继续：%s", jv.Status)
	}
	fr := n.byKind(KindFirstReady)
	if len(fr) != 1 || fr[0].Link != JobLink(jv.JobID) || !strings.Contains(fr[0].Body, "第 1 段做好了") {
		t.Fatalf("第 1 段好了应通知一次：%+v", fr)
	}
	e.run(jv.JobID, 2)
	if len(n.byKind(KindFirstReady)) != 1 {
		t.Fatal("同 job 同类只 1 次")
	}
	if _, err := e.svc.Continue(e.ctx, ContinueReq{UserID: uid, JobID: jv.JobID, PlanHash: pv.PlanHash, IdempotencyKey: "c1"}); err != nil {
		t.Fatal(err)
	}
	jv = e.run(jv.JobID, 6)
	if jv.Status != "succeeded" {
		t.Fatalf("应完成：%s", jv.Status)
	}
	done := n.byKind(KindDone)
	if len(done) != 1 || !strings.Contains(done[0].Body, "3 段全部成功") {
		t.Fatalf("完成应通知一次：%+v", done)
	}
}

func TestNotifySegFailedOnlyAfterRefundSucceeds(t *testing.T) {
	n := &fakeNotifier{}
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithNotifier(n) })
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "CONTENT_POLICY"}}
	e.ledger.refundErrs = 1 // 第一次退回失败：不能说「已退回」
	pv := e.plan(e.seedAnalysis(10))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 1)
	if got := n.byKind(KindSegFailed); len(got) != 0 {
		t.Fatalf("退点还没成功不能发「已退回」：%+v", got)
	}
	jv = e.run(jv.JobID, 3)
	got := n.byKind(KindSegFailed)
	if jv.Status != "failed" || len(got) != 1 || !strings.Contains(got[0].Body, "已全部退回 100 点") {
		t.Fatalf("退成功后应通知一次：%s %+v", jv.Status, got)
	}
	if len(n.byKind(KindDone)) != 0 {
		t.Fatal("全部失败不发「做好了」")
	}
}

func TestNotifyFailureDoesNotBreakAdvance(t *testing.T) {
	n := &fakeNotifier{err: errors.New("db down")}
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithNotifier(n) })
	pv := e.plan(e.seedAnalysis(10))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if jv.Status != "succeeded" {
		t.Fatalf("通知失败不影响任务：%s", jv.Status)
	}
	e.assertMoney(jv.JobID)
}

// ---------------------------------------------------------------- 入素材库

func deliveryEnv(t *testing.T, lib *fakeLib, assets *fakeAssets) (*env, *memOps) {
	var ops *memOps
	e := newOpsEnv(t, false, nil, func(e *env) Option {
		ops = newMemOps(func() time.Time { return e.clock })
		return WithDelivery(ops, lib, assets, DeliveryConfig{MaxAttempts: 3})
	})
	return e, ops
}

func TestDeliveryFinalOnceAndIdempotent(t *testing.T) {
	lib, assets := &fakeLib{}, &fakeAssets{}
	e, ops := deliveryEnv(t, lib, assets)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if jv.Status != "succeeded" || jv.Library == nil || jv.Library.Status != "saving" {
		t.Fatalf("成片应入队：%s %+v", jv.Status, jv.Library)
	}
	e.run(jv.JobID, 3) // 多次推进不重复入队
	if len(ops.dels) != 1 || ops.dels[0].Kind != model.DeliveryFinal {
		t.Fatalf("单段无叠字：成片就是那一段，同 rid 只入 final 一条：%+v", ops.dels)
	}
	e.svc.TickDeliveries(e.ctx, 10)
	e.svc.TickDeliveries(e.ctx, 10)
	if len(assets.rows) != 1 || assets.rows[0].SourceTaskID != jv.TaskID || assets.rows[0].Type != "video" ||
		!strings.HasPrefix(assets.rows[0].ResultURL, "https://pub.test/") || assets.rows[0].EntryCode != EntryCode {
		t.Fatalf("应入素材库一次（公有桶地址、任务号）：%+v", assets.rows)
	}
	if ops.touch[jv.JobID] != 1 {
		t.Fatalf("入库完成刷新任务中心水位：%d", ops.touch[jv.JobID])
	}
	jv, _ = e.svc.JobView(e.ctx, uid, jv.JobID)
	if jv.Library.Status != "saved" {
		t.Fatalf("应显示已保存：%+v", jv.Library)
	}
	// 崩溃重放：同一行再跑一次（模拟写完资产、没标完成）也不会写第二条
	ops.dels[0].Status, ops.dels[0].LeaseUntil = model.DeliveryPending, nil
	e.svc.TickDeliveries(e.ctx, 10)
	if len(assets.rows) != 1 {
		t.Fatalf("重放不能写第二条：%d", len(assets.rows))
	}
}

func TestDeliveryRetryBackoffAndGiveUp(t *testing.T) {
	transient := &mediasvc.Error{Status: 503, Code: "busy"}
	lib, assets := &fakeLib{errs: []error{transient, transient, transient}}, &fakeAssets{}
	e, ops := deliveryEnv(t, lib, assets)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if jv.Status != "succeeded" {
		t.Fatalf("入库失败不阻塞 job 收尾：%s", jv.Status)
	}
	e.svc.TickDeliveries(e.ctx, 10)
	d := ops.dels[0]
	if d.Status != model.DeliveryPending || d.NextRunAt == nil || d.Attempts != 1 || d.LastError == nil {
		t.Fatalf("暂时失败应退避重试：%+v", d)
	}
	jv, _ = e.svc.JobView(e.ctx, uid, jv.JobID)
	if jv.Library.Status != "saving" || !strings.Contains(jv.Library.Text, "已在重试") {
		t.Fatalf("文案：%+v", jv.Library)
	}
	e.svc.TickDeliveries(e.ctx, 10) // 未到期不取
	if d.Attempts != 1 {
		t.Fatal("退避期内不应重试")
	}
	for i := 0; i < 2; i++ {
		e.clock = e.clock.Add(time.Hour)
		e.svc.TickDeliveries(e.ctx, 10)
	}
	if d.Status != model.DeliveryGaveUp || len(assets.rows) != 0 {
		t.Fatalf("超过次数应放弃：%+v", d)
	}
	// 永久错误（4xx）一次就放弃
	lib2 := &fakeLib{errs: []error{&mediasvc.Error{Status: 403, Code: "forbidden"}}}
	e2, ops2 := deliveryEnv(t, lib2, &fakeAssets{})
	e2.run(e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1").JobID, 3)
	e2.svc.TickDeliveries(e2.ctx, 10)
	if ops2.dels[0].Status != model.DeliveryGaveUp {
		t.Fatalf("4xx 应直接放弃：%+v", ops2.dels[0])
	}
}

func TestSaveSegmentReturnsAutoDelivered(t *testing.T) {
	lib, assets := &fakeLib{}, &fakeAssets{}
	e, ops := deliveryEnv(t, lib, assets)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if len(ops.dels) != 1 {
		t.Fatalf("收尾时自动入队（单段：同 rid 只入 final）：%d", len(ops.dels))
	}
	v, err := e.svc.SaveSegment(e.ctx, uid, jv.JobID, 1)
	if err != nil || v.Library.Status != "saving" {
		t.Fatalf("手动保存命中自动入队的那一行：%+v %v", v, err)
	}
	if len(ops.dels) != 1 {
		t.Fatalf("手动保存按内容（同 rid）命中成片那一条，不另入：%d", len(ops.dels))
	}
	if _, err := e.svc.SaveSegment(e.ctx, uid+1, jv.JobID, 1); err == nil {
		t.Fatal("别人的任务不能保存")
	}
	e.svc.TickDeliveries(e.ctx, 10)
	// 单段任务的成片就是这一段（同一个 rid → 同一个公有地址）：按地址查重只入库 1 条，不重复
	if len(assets.rows) != 1 {
		t.Fatalf("同一视频只入库 1 条：%+v", assets.rows)
	}
	if v, _ := e.svc.SaveSegment(e.ctx, uid, jv.JobID, 1); v == nil || v.Library.Status != "saved" {
		t.Fatalf("已入库直接返回已有：%+v", v)
	}
}

// ---------------------------------------------------------------- 强制收尾

func TestForceFinishGuardsAndSettles(t *testing.T) {
	e := newEnv(t, true)
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if jv.Status != "waiting" {
		t.Fatalf("应待继续：%s", jv.Status)
	}
	res, err := e.svc.ForceFinish(e.ctx, jv.JobID)
	if err != nil || res.Status != "succeeded" {
		t.Fatalf("待继续强制收尾 = 只交付第 1 段：%+v %v", res, err)
	}
	e.assertMoney(jv.JobID)
	if _, err := e.svc.ForceFinish(e.ctx, jv.JobID); reasonOf(err) != ReasonInFlight {
		t.Fatalf("已结清的终态不需要收尾：%v", err)
	}

	// 有段在生成：拒绝
	e2 := newEnv(t, false)
	pv2 := e2.plan(e2.seedAnalysis(10))
	j2 := e2.confirm(pv2, "", "k1")
	if err := e2.svc.Advance(e2.ctx, j2.JobID); err != nil {
		t.Fatal(err)
	}
	steps := e2.steps(j2.JobID)
	running := false
	for _, st := range steps {
		running = running || st.Status == model.StepRunning
	}
	if running {
		if _, err := e2.svc.ForceFinish(e2.ctx, j2.JobID); reasonOf(err) != ReasonInFlight {
			t.Fatalf("在途不能收尾：%v", err)
		}
	}
	// 排队（还没领段）：取消并退回
	e3 := newEnv(t, false)
	pv3 := e3.plan(e3.seedAnalysis(10))
	j3 := e3.confirm(pv3, "", "k1")
	res3, err := e3.svc.ForceFinish(e3.ctx, j3.JobID)
	if err != nil || res3.Status != "failed" || res3.CanceledSteps != 1 || res3.OpenCharges != 0 {
		t.Fatalf("排队强制收尾应取消并退回：%+v %v", res3, err)
	}
	if e3.ledger.sum("REFUNDED") != 100 {
		t.Fatalf("应退回 100：%d", e3.ledger.sum("REFUNDED"))
	}
	e3.assertMoney(j3.JobID)
}

// ---------------------------------------------------------------- 审查修复（中4 / 中5 / 低8 / 低9 / 低10）

func modCalls(e *env) int {
	n := 0
	for _, k := range e.gw.submits {
		if strings.Contains(k, "mod:") {
			n++
		}
	}
	return n
}

// 中4：ERROR_OPEN 在确认时必须重审（成功覆盖）。
func TestModerationErrorOpenRecheckedAtConfirm(t *testing.T) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, func(c *Config) { c.ModerationFailOpen = true }, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	pv := e.plan(e.seedAnalysis(10)) // 网关结果读不懂 → 故障，fail-open 记 ERROR_OPEN
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	if m, _ := ops.GetModeration(e.ctx, p.ID, p.PlanHash); m == nil || m.Status != model.ModerationErrorOpen {
		t.Fatalf("先记 ERROR_OPEN：%+v", m)
	}
	// 审核服务恢复：确认时重审，结果是违规 → 不让确认
	e.gw.analysis[opModerationImage] = verdicts("block")
	_, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, IdempotencyKey: "k1"})
	if reasonOf(err) != ReasonModerationRejected || e.ledger.holds != 0 {
		t.Fatalf("确认时必须重审：%v holds=%d", err, e.ledger.holds)
	}
	if m, _ := ops.GetModeration(e.ctx, p.ID, p.PlanHash); m.Status != model.ModerationRejected {
		t.Fatalf("重审结果覆盖：%+v", m)
	}
}

// 中4：策略签名变了重审；REJECTED 超过有效期重审。
func TestModerationPolicySigAndRejectTTL(t *testing.T) {
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	e.gw.analysis[opModerationImage] = verdicts("review")
	pv := e.plan(e.seedAnalysis(10))
	if pv.Moderation.Status != "rejected" || modCalls(e) != 1 {
		t.Fatalf("默认 review 按违规：%+v calls=%d", pv.Moderation, modCalls(e))
	}
	if pv, _ := e.svc.QuotePlan(e.ctx, uid, pv.PlanNo); pv.Moderation.Status != "rejected" || modCalls(e) != 1 {
		t.Fatalf("有效期内复用结论：calls=%d", modCalls(e))
	}
	// 运营把 review 改成放行：签名变了 → 重审（同一幂等键网关重放同一结论，策略按新口径评估）
	svc2 := New(func() Config {
		c := DefaultConfig()
		c.ModerationReviewPass = true
		c.ModerationPlanWait = 5 * time.Second
		return c
	}(), e.store, e.ledger,
		WithGateway(e.gw), WithClock(func() time.Time { return e.clock }), WithSigner(e.signer), WithModeration(ops, ops, nil))
	if pv, _ := svc2.QuotePlan(e.ctx, uid, pv.PlanNo); pv.Moderation.Status != "passed" {
		t.Fatalf("策略变了应按新口径重审：%+v", pv.Moderation)
	}
	// 回到默认策略：结论又是旧签名 → 重审为不通过；过 24 小时后不通过也允许重审
	e.gw.analysis[opModerationImage] = verdicts("block")
	if pv, _ := e.svc.QuotePlan(e.ctx, uid, pv.PlanNo); pv.Moderation.Status != "rejected" {
		t.Fatal("签名不一致重审")
	}
	e.gw.analysis[opModerationImage] = verdicts("pass")
	if pv, _ := e.svc.QuotePlan(e.ctx, uid, pv.PlanNo); pv.Moderation.Status != "rejected" {
		t.Fatal("有效期内不重审")
	}
	e.clock = e.clock.Add(25 * time.Hour)
	if pv, _ := e.svc.QuotePlan(e.ctx, uid, pv.PlanNo); pv == nil || pv.Moderation.Status != "passed" {
		t.Fatalf("不通过超过 24 小时允许重审：%+v", pv)
	}
}

// 中5：自动重试共用原预留单，退点数按计费请求号去重。
func TestNotifyRefundDedupByRequest(t *testing.T) {
	n := &fakeNotifier{}
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithNotifier(n) })
	e.gw.script["seg/01/generate"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	e.gw.script["seg/01/generate#r1"] = []outcome{{status: aigateway.StatusFailed, class: "VENDOR_FAILED"}}
	pv := e.plan(e.seedAnalysis(10))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 5)
	got := n.byKind(KindSegFailed)
	if jv.Status != "failed" || len(got) != 1 || !strings.Contains(got[0].Body, "已全部退回 100 点") {
		t.Fatalf("重试链只退一次 100 点：%s %+v", jv.Status, got)
	}
}

// 低10：强制收尾取消的段计入「没做成」，并补发通知。
func TestForceFinishNotifies(t *testing.T) {
	n := &fakeNotifier{}
	e := newOpsEnv(t, true, nil, func(*env) Option { return WithNotifier(n) })
	pv := e.plan(e.seedAnalysis(32, 12, 25))
	jv := e.run(e.confirm(pv, "", "k1").JobID, 3)
	if _, err := e.svc.ForceFinish(e.ctx, jv.JobID); err != nil {
		t.Fatal(err)
	}
	done := n.byKind(KindDone)
	if len(done) != 1 || !strings.Contains(done[0].Body, "做好了 1 段，2 段没做成") {
		t.Fatalf("待继续被收尾：%+v", done)
	}
	n2 := &fakeNotifier{}
	e2 := newOpsEnv(t, false, nil, func(*env) Option { return WithNotifier(n2) })
	j2 := e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1")
	if _, err := e2.svc.ForceFinish(e2.ctx, j2.JobID); err != nil {
		t.Fatal(err)
	}
	if got := n2.byKind(KindSegFailed); len(got) != 1 || !strings.Contains(got[0].Body, "已全部退回 100 点") {
		t.Fatalf("排队被收尾：取消的段计入没做成并说明已退回：%+v", got)
	}
}

// 低8：复制期间认领被接手 → 本轮不写素材库（接手者写）。
func TestDeliveryClaimLostSkipsAssetWrite(t *testing.T) {
	lib, assets := &fakeLib{}, &fakeAssets{}
	e, ops := deliveryEnv(t, lib, assets)
	e.run(e.confirm(e.plan(e.seedAnalysis(10)), "", "k1").JobID, 3)
	ops.stealOnRenew = true
	e.svc.TickDeliveries(e.ctx, 10)
	if len(assets.rows) != 0 || ops.dels[0].Status != model.DeliveryPending {
		t.Fatalf("认领丢了不写库：%+v", assets.rows)
	}
	ops.stealOnRenew = false
	ops.dels[0].LeaseUntil = nil
	e.svc.TickDeliveries(e.ctx, 10)
	if len(assets.rows) != 1 {
		t.Fatalf("接手后写一条：%+v", assets.rows)
	}
}

// 低9：确认的重放 / 方案已确认标记 Replay（接口层不计额度）。
func TestConfirmReplayFlag(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	jv := e.confirm(pv, "", "k1")
	if jv.Replay {
		t.Fatal("首次确认不是重放")
	}
	if again := e.confirm(pv, "", "k1"); !again.Replay || again.JobID != jv.JobID {
		t.Fatalf("同键重放：%+v", again)
	}
	if other := e.confirm(pv, "", "k2"); !other.Replay || other.JobID != jv.JobID {
		t.Fatalf("方案已确认：%+v", other)
	}
}

// 新3：重审一定是新的网关幂等键（策略签名 / 重审轮次参与），不会被网关按旧键重放旧结论。
func TestModerationRecheckUsesNewGatewayKey(t *testing.T) {
	if moderationKey("", "VRP1", "image", "sig-a", 0, []string{"u"}) == moderationKey("", "VRP1", "image", "sig-b", 0, []string{"u"}) ||
		moderationKey("", "VRP1", "image", "sig-a", 0, []string{"u"}) == moderationKey("", "VRP1", "image", "sig-a", 1, []string{"u"}) {
		t.Fatal("策略签名 / 轮次要进幂等键")
	}
	ops := newMemOps(nil)
	e := newOpsEnv(t, false, nil, func(*env) Option { return WithModeration(ops, ops, nil) })
	ops.clock = func() time.Time { return e.clock }
	e.gw.analysis[opModerationImage] = verdicts("block")
	pv := e.plan(e.seedAnalysis(10))
	e.clock = e.clock.Add(25 * time.Hour)
	e.gw.analysis[opModerationImage] = verdicts("pass")
	if pv, _ := e.svc.QuotePlan(e.ctx, uid, pv.PlanNo); pv.Moderation.Status != "passed" || modCalls(e) != 2 {
		t.Fatalf("过期重审应是新调用：%+v calls=%d", pv.Moderation, modCalls(e))
	}
	p, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	if m, _ := ops.GetModeration(e.ctx, p.ID, p.PlanHash); m.Attempt != 1 {
		t.Fatalf("重审轮次 +1：%+v", m)
	}
}

// 新5：确认前查已有 job（同幂等键 / 方案已确认），命中直接返回。
func TestExistingConfirm(t *testing.T) {
	e := newEnv(t, false)
	pv := e.plan(e.seedAnalysis(10))
	if v, err := e.svc.ExistingConfirm(e.ctx, uid, pv.PlanNo, "k1"); err != nil || v != nil {
		t.Fatalf("还没确认：%+v %v", v, err)
	}
	jv := e.confirm(pv, "", "k1")
	for _, key := range []string{"k1", "other"} {
		if v, err := e.svc.ExistingConfirm(e.ctx, uid, pv.PlanNo, key); err != nil || v == nil || !v.Replay || v.JobID != jv.JobID {
			t.Fatalf("键 %s 应命中已有 job：%+v %v", key, v, err)
		}
	}
	if v, _ := e.svc.ExistingConfirm(e.ctx, uid+1, pv.PlanNo, "k1"); v != nil {
		t.Fatal("别人的不命中")
	}
}
