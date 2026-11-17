package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"
)

// memBatch 批次存储的内存替身（语义同 repo/batch.go：(user, request) 唯一、(batch, seq) / job 唯一忽略重复）。
type memBatch struct {
	mu    sync.Mutex
	e     *env
	id    int64
	heads []*model.Batch
	links []*model.BatchJob
}

func (m *memBatch) InsertBatch(_ context.Context, b *model.Batch) (*model.Batch, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.heads {
		if x.UserID == b.UserID && x.RequestID == b.RequestID {
			return clone(x), false, nil
		}
	}
	c := clone(b)
	m.id++
	c.ID, c.CreatedAt, c.UpdatedAt = m.id, m.e.clock, m.e.clock
	m.heads = append(m.heads, c)
	return clone(c), true, nil
}
func (m *memBatch) GetBatch(_ context.Context, userID, id int64) (*model.Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.heads {
		if x.ID == id && x.UserID == userID {
			return clone(x), nil
		}
	}
	return nil, nil
}
func (m *memBatch) GetBatchByRequest(_ context.Context, userID int64, req string) (*model.Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.heads {
		if x.UserID == userID && x.RequestID == req {
			return clone(x), nil
		}
	}
	return nil, nil
}
func (m *memBatch) UpdateBatch(_ context.Context, id int64, from []int8, set map[string]any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.heads {
		if x.ID != id {
			continue
		}
		for _, f := range from {
			if x.Status == f {
				apply(x, set)
				x.UpdatedAt = m.e.clock
				return true, nil
			}
		}
	}
	return false, nil
}
func (m *memBatch) AddBatchJob(_ context.Context, bj *model.BatchJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.links {
		if (x.BatchID == bj.BatchID && x.Seq == bj.Seq) || x.JobID == bj.JobID {
			return nil
		}
	}
	m.links = append(m.links, clone(bj))
	return nil
}
func (m *memBatch) ListBatchJobs(_ context.Context, batchID int64) ([]*model.BatchJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.BatchJob
	for _, x := range m.links {
		if x.BatchID == batchID {
			out = append(out, clone(x))
		}
	}
	return out, nil
}
func (m *memBatch) BatchIDsOfJobs(_ context.Context, ids []int64) (map[int64]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int64]int64{}
	for _, x := range m.links {
		for _, id := range ids {
			if x.JobID == id {
				out[id] = x.BatchID
			}
		}
	}
	return out, nil
}

// batchEnv 装好批次存储，并做出一条「已有成功成片」的源方案（单段 12.3 秒；dims 与参考图由调用方给）。
func batchEnv(t *testing.T, dims map[string]DimEdit, refs []string, succeed bool) (*env, *memBatch, *PlanView) {
	t.Helper()
	e := newEnv(t, false)
	mb := &memBatch{e: e}
	WithBatch(mb)(e.svc)
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: e.seedAnalysis(12.3).AnalysisNo, ModelGroup: "vid.seedance-2.5",
		RefImages: refs, Annotations: Annotations{Dims: dims}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	jv := e.confirm(pv, "", "parent-1")
	if succeed {
		if jv = e.run(jv.JobID, 3); jv.Status != "succeeded" {
			t.Fatalf("源任务应成功：%s", jv.Status)
		}
	}
	return e, mb, pv
}

func productDims() map[string]DimEdit {
	return map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}
}

func variant(tags ...string) BatchVariant {
	v := BatchVariant{}
	for _, tg := range tags {
		v.RefImages = append(v.RefImages, "mr://"+testRid(tg))
	}
	return v
}

// 报价：每条点数 = 源方案全部段报价，合计 = 条数 × 单价；带主替换项与源方案指纹。
func TestQuoteBatch(t *testing.T) {
	e, _, pv := batchEnv(t, productDims(), []string{testRef}, true)
	q, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, []BatchVariant{variant("va"), variant("vb"), variant("vc")})
	if err != nil {
		t.Fatal(err)
	}
	if q.Count != 3 || len(q.Rows) != 3 || q.Total != 3*q.Rows[0].Points || q.Rows[0].Points != pv.StageQuotes.Total || q.Rows[2].Index != 3 {
		t.Fatalf("报价：%+v 方案合计 %d", q, pv.StageQuotes.Total)
	}
	if q.MainDim != "product" || q.RefsPerVariant != 1 || q.PlanHash != pv.PlanHash || q.PlanNo != pv.PlanNo {
		t.Fatalf("主替换项 / 指纹：%+v", q)
	}
}

// 方案还没有成功成片：BATCH_NOT_READY（报价与确认都拦）。
func TestBatchNotReady(t *testing.T) {
	e, _, pv := batchEnv(t, productDims(), []string{testRef}, false)
	if _, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, []BatchVariant{variant("va")}); reasonOf(err) != ReasonBatchNotReady {
		t.Fatalf("报价应 BATCH_NOT_READY：%v", err)
	}
	_, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, Variants: []BatchVariant{variant("va")}, ExpectedPoints: 130, IdempotencyKey: "b1"})
	if reasonOf(err) != ReasonBatchNotReady || e.ledger.holds != 1 { // holds=1 是源任务自己的那一笔
		t.Fatalf("确认应 BATCH_NOT_READY 且不动钱：%v holds=%d", err, e.ledger.holds)
	}
	// 一个没确认过的方案同样不开放
	pv2, _ := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: pv.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "scene": {Mode: ModeDescribe, Note: "海边"}}}})
	if _, err := e.svc.QuoteBatch(e.ctx, uid, pv2.PlanNo, []BatchVariant{variant("va")}); reasonOf(err) != ReasonBatchNotReady {
		t.Fatalf("未确认方案：%v", err)
	}
}

// 条数 1~10；每条图数必须等于主替换项引用的图数；没配批次存储回 7003。
func TestBatchParamBounds(t *testing.T) {
	e, _, pv := batchEnv(t, productDims(), []string{testRef}, true)
	many := make([]BatchVariant, 11)
	for i := range many {
		many[i] = variant("va")
	}
	for name, vs := range map[string][]BatchVariant{"0 条": nil, "11 条": many, "图数多": {variant("va", "vb")}, "没有图": {{}}} {
		if _, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, vs); reasonOf(err) != "PARAM_INVALID" {
			t.Fatalf("%s 报价应 PARAM_INVALID：%v", name, err)
		}
		_, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, Variants: vs, ExpectedPoints: 130, IdempotencyKey: "k-" + name})
		if reasonOf(err) != "PARAM_INVALID" {
			t.Fatalf("%s 确认应 PARAM_INVALID：%v", name, err)
		}
	}
	ten := many[:10]
	if _, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, ten); err != nil {
		t.Fatalf("10 条应可报价：%v", err)
	}
	e2 := newEnv(t, false)
	if _, err := e2.svc.QuoteBatch(e2.ctx, uid, "x", ten); codeOf(err) != xerr.NotReady {
		t.Fatalf("没装配批次存储应回 7003：%v", err)
	}
}

// 确认成功：N 条任务全部排队；子任务幂等键 {key}#{i}；子方案只换主替换项参考图，标注 / 模型 / 清晰度原样；列表带 batchId；合计一次冻结。
func TestConfirmBatchOK(t *testing.T) {
	e, mb, pv := batchEnv(t, productDims(), []string{testRef}, true)
	q, _ := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, []BatchVariant{variant("va"), variant("vb"), variant("vc")})
	before := e.ledger.holds
	bv, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: q.Total, IdempotencyKey: "call-1",
		Variants: []BatchVariant{variant("va"), variant("vb"), variant("vc")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bv.Jobs) != 3 || bv.Total != q.Total || bv.Status != "running" || bv.PlanNo != pv.PlanNo || bv.Replay || e.ledger.holds-before != 3 {
		t.Fatalf("批次：%+v holds+%d", bv, e.ledger.holds-before)
	}
	parent, _ := e.store.GetPlanByNo(e.ctx, uid, pv.PlanNo)
	for i, js := range bv.Jobs {
		j, _ := e.store.GetJob(e.ctx, js.JobID)
		if j.RequestID != fmt.Sprintf("call-1#%d", i) || j.Status != model.JobQueued || !j.Staged == false && j.SegTotal < 1 || js.BatchID != bv.BatchID {
			t.Fatalf("任务 %d：req=%s status=%d staged=%v batch=%d", i, j.RequestID, j.Status, j.Staged, js.BatchID)
		}
		if j.Staged {
			t.Fatalf("批量直接做全部段，不应是先做第 1 段")
		}
		child, _ := e.store.GetPlanByNo(e.ctx, uid, j.PlanNo)
		if child.Status != model.PlanConfirmed || child.ModelGroup != parent.ModelGroup || child.Resolution != parent.Resolution || child.AspectRatio != parent.AspectRatio {
			t.Fatalf("子方案：%+v", child)
		}
		if string(child.Annotations) != string(parent.Annotations) {
			t.Fatalf("子方案标注必须与源方案一致：%s vs %s", child.Annotations, parent.Annotations)
		}
		var refs []RefImage
		_ = json.Unmarshal(child.Refs, &refs)
		want := testRid([]string{"va", "vb", "vc"}[i])
		if len(refs) != 1 || refs[0].Rid != want {
			t.Fatalf("子方案参考图：%+v 期望 %s", refs, want)
		}
	}
	// 历史列表带 batchId
	jl, _ := e.svc.ListJobs(e.ctx, uid, 1, 50)
	n := 0
	for _, it := range jl.Items {
		if it.BatchID == bv.BatchID {
			n++
		}
	}
	if n != 3 || len(mb.heads) != 1 || mb.heads[0].Status != model.BatchSubmitted {
		t.Fatalf("列表 batchId=%d 的条数 %d；批次头 %+v", bv.BatchID, n, mb.heads)
	}
	// 取一批：同一份视图；做完后状态现算
	got, err := e.svc.GetBatch(e.ctx, uid, bv.BatchID)
	if err != nil || len(got.Jobs) != 3 || got.Status != "running" {
		t.Fatalf("GetBatch：%+v %v", got, err)
	}
	for _, js := range bv.Jobs {
		e.run(js.JobID, 3)
	}
	if got, _ = e.svc.GetBatch(e.ctx, uid, bv.BatchID); got.Status != "succeeded" {
		t.Fatalf("全部做完应 succeeded：%s", got.Status)
	}
	if _, err := e.svc.GetBatch(e.ctx, uid+1, bv.BatchID); codeOf(err) != xerr.NotFound {
		t.Fatalf("别人的批次应 404：%v", err)
	}
}

// 合计与现算不一致：QUOTE_CHANGED，不建方案不动钱。
func TestConfirmBatchQuoteChanged(t *testing.T) {
	e, mb, pv := batchEnv(t, productDims(), []string{testRef}, true)
	before := e.ledger.holds
	_, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: 1, IdempotencyKey: "call-1", Variants: []BatchVariant{variant("va"), variant("vb")}})
	if reasonOf(err) != ReasonQuoteChanged || e.ledger.holds != before || len(mb.heads) != 0 {
		t.Fatalf("应 QUOTE_CHANGED 且不动钱：%v holds+%d heads=%d", err, e.ledger.holds-before, len(mb.heads))
	}
	// 合计必填
	_, err = e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, IdempotencyKey: "call-2", Variants: []BatchVariant{variant("va")}})
	if reasonOf(err) != "PARAM_MISSING" {
		t.Fatalf("缺合计：%v", err)
	}
}

// 原子预留：第 3 条预留失败（余额只够两条）→ 前两条退回、任务失败、没有一条排队、批次失败、余额复原。
func TestConfirmBatchAtomicRollback(t *testing.T) {
	e, mb, pv := batchEnv(t, productDims(), []string{testRef}, true)
	vs := []BatchVariant{variant("va"), variant("vb"), variant("vc")}
	q, _ := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, vs)
	per := q.Rows[0].Points
	e.ledger.balance = per*2 + per/2 // 够两条半
	_, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: q.Total, IdempotencyKey: "call-1", Variants: vs})
	if err == nil || codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("应余额不足：%v", err)
	}
	if got := e.ledger.sum("HELD"); got != 0 {
		t.Fatalf("不应有冻结中的预留单：%d", got)
	}
	if e.ledger.balance != per*2+per/2 {
		t.Fatalf("余额应复原：%d", e.ledger.balance)
	}
	if len(mb.heads) != 1 || mb.heads[0].Status != model.BatchFailed || mb.heads[0].FailReason == nil {
		t.Fatalf("批次头应标失败：%+v", mb.heads)
	}
	jobs := 0
	for _, j := range e.store.jobs {
		if j.RequestID == "call-1#0" || j.RequestID == "call-1#1" || j.RequestID == "call-1#2" {
			jobs++
			if j.Status != model.JobFailed {
				t.Fatalf("回滚的任务应失败：%s=%d", j.RequestID, j.Status)
			}
			for _, c := range e.charges(j.ID) {
				if c.Status == model.ChargeHeld || e.ledger.status(c.RequestID) == "HELD" {
					t.Fatalf("回滚后不应有冻结的预留单：%s", c.RequestID)
				}
			}
		}
	}
	if jobs != 3 {
		t.Fatalf("前两条 + 出错的第 3 条都应落了任务并失败：%d", jobs)
	}
	// 同 call_id 重放：返回失败的批次，不再动钱
	holds := e.ledger.holds
	bv, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: q.Total, IdempotencyKey: "call-1", Variants: vs})
	if err != nil || bv.Status != "failed" || !bv.Replay || e.ledger.holds != holds {
		t.Fatalf("失败批次重放：%+v %v", bv, err)
	}
	// 充值后换新 call_id：整批成功
	e.ledger.balance = -1
	bv, err = e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: q.Total, IdempotencyKey: "call-2", Variants: vs})
	if err != nil || bv.Status != "running" || len(bv.Jobs) != 3 {
		t.Fatalf("新 call_id 应成功：%+v %v", bv, err)
	}
}

// 幂等：同 call_id 重放同一批（同 batchId、不再冻结）；并发中的批次（提交中）回 IN_PROGRESS。
func TestConfirmBatchIdempotent(t *testing.T) {
	e, mb, pv := batchEnv(t, productDims(), []string{testRef}, true)
	vs := []BatchVariant{variant("va"), variant("vb")}
	q, _ := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, vs)
	req := BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: q.Total, IdempotencyKey: "call-1", Variants: vs}
	first, err := e.svc.ConfirmBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	holds := e.ledger.holds
	again, err := e.svc.ConfirmBatch(e.ctx, req)
	if err != nil || !again.Replay || again.BatchID != first.BatchID || len(again.Jobs) != 2 || e.ledger.holds != holds {
		t.Fatalf("重放：%+v %v holds %d→%d", again, err, holds, e.ledger.holds)
	}
	if v, _ := e.svc.ExistingBatch(e.ctx, uid, "call-1"); v == nil || v.BatchID != first.BatchID {
		t.Fatalf("ExistingBatch 应命中：%+v", v)
	}
	// 提交中的批次头（模拟并发 / 崩在半路）：未到回收阈值 → IN_PROGRESS；到了 → 标失败后按失败回放
	mb.heads[0].Status, mb.heads[0].UpdatedAt = model.BatchSubmitting, e.clock
	if _, err := e.svc.ConfirmBatch(e.ctx, req); reasonOf(err) != ReasonInProgress {
		t.Fatalf("应 IN_PROGRESS：%v", err)
	}
	e.clock = e.clock.Add(e.svc.cfg.ReapAfter + 1)
	if v, err := e.svc.ConfirmBatch(e.ctx, req); err != nil || v.Status != "failed" || !v.Replay {
		t.Fatalf("超时应按失败回放：%+v %v", v, err)
	}
}

// 主替换项：人物 > 商品 > 服装，只换它引用的那张图，其余图 / 项原样。
func TestBatchMainDimOnlyChangesItsRefs(t *testing.T) {
	dims := map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{2}}, "model": {Mode: ModeReplace, Refs: []int{1}}}
	refs := []string{testRef, "mr://" + testRid("prod")}
	e, _, pv := batchEnv(t, dims, refs, true)
	q, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, []BatchVariant{variant("va"), variant("vb")})
	if err != nil || q.MainDim != "model" {
		t.Fatalf("同时换人物与商品，主替换项应是人物：%+v %v", q, err)
	}
	bv, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: q.Total, IdempotencyKey: "c", Variants: []BatchVariant{variant("va"), variant("vb")}})
	if err != nil {
		t.Fatal(err)
	}
	for i, js := range bv.Jobs {
		child, _ := e.store.GetPlanByNo(e.ctx, uid, js.PlanNo)
		var got []RefImage
		_ = json.Unmarshal(child.Refs, &got)
		if len(got) != 2 || got[0].Rid != testRid([]string{"va", "vb"}[i]) || got[1].Rid != testRid("prod") {
			t.Fatalf("只应换人物那张图：%+v", got)
		}
	}
	// 纯函数：优先级与确定性
	ann := Annotations{Dims: map[string]DimEdit{"garment": {Mode: ModeReplace, Refs: []int{1}}, "scene": {Mode: ModeReplace, Refs: []int{2}}, "product": {Mode: ModeDescribe, Note: "x"}}}
	if got := mainReplaceDim(ann); got != "garment" {
		t.Fatalf("服装应优先于场景，且描述改写不算：%s", got)
	}
	delete(ann.Dims, "garment")
	if got := mainReplaceDim(ann); got != "scene" {
		t.Fatalf("其余项：%s", got)
	}
	if mainReplaceDim(Annotations{Dims: map[string]DimEdit{"scene": {Mode: ModeDescribe, Note: "x"}}}) != "" {
		t.Fatal("没有换图的项应为空")
	}
}

// 主替换项的图被别的项共用：拒绝（不然会连带改别的项）；没有换图的项：PARAM_INVALID。
func TestBatchSharedRefAndNoReplaceDim(t *testing.T) {
	dims := map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}, "accessory": {Mode: ModeReplace, Refs: []int{1}}}
	e, _, pv := batchEnv(t, dims, []string{testRef}, true)
	if _, err := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, []BatchVariant{variant("va")}); reasonOf(err) != "PARAM_INVALID" {
		t.Fatalf("共用图应拒：%v", err)
	}
	e2, _, pv2 := batchEnv(t, map[string]DimEdit{"scene": {Mode: ModeDescribe, Note: "海边"}}, nil, true)
	if _, err := e2.svc.QuoteBatch(e2.ctx, uid, pv2.PlanNo, []BatchVariant{variant("va")}); reasonOf(err) != "PARAM_INVALID" {
		t.Fatalf("没有换图的项应拒：%v", err)
	}
}

// 变体里的图不是本人上传的：整批在动钱之前被拒（GapsError），没有批次头、没有任务。
func TestBatchRejectsForeignRefBeforeMoney(t *testing.T) {
	e, mb, pv := batchEnv(t, productDims(), []string{testRef}, true)
	q, _ := e.svc.QuoteBatch(e.ctx, uid, pv.PlanNo, []BatchVariant{variant("va"), variant("vb")})
	holds := e.ledger.holds
	_, err := e.svc.ConfirmBatch(e.ctx, BatchConfirmReq{UserID: uid, PlanNo: pv.PlanNo, ExpectedPoints: q.Total, IdempotencyKey: "c",
		Variants: []BatchVariant{variant("va"), {RefImages: []string{"https://evil.test/x.png"}}}})
	var ge *GapsError
	if !errors.As(err, &ge) || e.ledger.holds != holds || len(mb.heads) != 0 {
		t.Fatalf("应在动钱前被拒：%v holds+%d heads=%d", err, e.ledger.holds-holds, len(mb.heads))
	}
}
