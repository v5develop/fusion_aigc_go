package logic

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// loopGW 按顺序返回预设的调用状态；block=true 时 Get 一直挂到 ctx 取消（模拟长轮询中进程停止 / 租约丢失）。
type loopGW struct {
	pipeline.Gateway
	mu      sync.Mutex
	seq     []*aigateway.Call
	waits   []time.Duration
	block   bool
	started chan struct{}
}

func (g *loopGW) Get(ctx context.Context, _ string, wait time.Duration) (*aigateway.Call, error) {
	g.mu.Lock()
	g.waits = append(g.waits, wait)
	block := g.block
	var c *aigateway.Call
	if !block && len(g.seq) > 0 {
		c, g.seq = g.seq[0], g.seq[1:]
	}
	g.mu.Unlock()
	if block {
		if g.started != nil {
			close(g.started)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c == nil {
		return nil, errors.New("no more calls")
	}
	return c, nil
}

func (g *loopGW) gets() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.waits)
}

var taskRowCols = []string{"id", "batch_id", "user_id", "kind", "status", "billing_status", "task_id", "billing_request_id",
	"gateway_call_id", "gateway_status", "started_at"}

func expectRunningTask1(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.id = 1) LIMIT 1")).WillReturnRows(sqlmock.NewRows(taskRowCols).
		AddRow(1, 9, 7, "imagegen", model.TaskRunning, model.BillingHeld, "simg_1", "studio-simg_1", "call_1", "RUNNING", time.Now()))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status", "created_at"}).
		AddRow(9, 7, model.BatchGenerating, time.Now()))
}

func useFastPollGap(t *testing.T) {
	old := minPollGap
	minPollGap = time.Millisecond
	t.Cleanup(func() { minPollGap = old })
}

// 长轮询循环：调用未终态就接着 GET ?wait=（不是每轮扫描只问一次），终态即交付，返回已到终态与所在批次。
func TestPollTaskLoopsUntilTerminal(t *testing.T) {
	useFastPollGap(t)
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	gw := &loopGW{seq: []*aigateway.Call{
		{ID: "call_1", Status: aigateway.StatusRunning},
		{ID: "call_1", Status: aigateway.StatusRunning},
		succeededCall(),
	}}
	f.gw = gw
	expectRunningTask1(mock)
	expectRunningTask1(mock)
	expectRunningTask1(mock)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET gateway_status = 'SUCCEEDED' WHERE (id = 1)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.id = 1) LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"id", "batch_id", "status"}).
		AddRow(1, 9, model.TaskSucceeded))

	batchID, finished := f.pollTask(context.Background(), 1)
	if !finished || batchID != 9 {
		t.Fatalf("应循环到终态: finished=%v batch=%d", finished, batchID)
	}
	if gw.gets() != 3 {
		t.Fatalf("应长轮询 3 次: %d", gw.gets())
	}
	for _, w := range gw.waits {
		if w != 20*time.Second {
			t.Fatalf("每次都应带 wait=20s: %v", gw.waits)
		}
	}
	if len(bill.settles) != 1 {
		t.Fatalf("应结算一次: %v", bill.settles)
	}
	met(t, mock)
}

// 长轮询挂起中 ctx 被取消（进程停止 / 心跳发现租约被接管）：立刻退出，不置失败、不退款、不再查库。
func TestPollTaskStopsOnCancelOrLeaseLoss(t *testing.T) {
	useFastPollGap(t)
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	gw := &loopGW{block: true, started: make(chan struct{})}
	f.gw = gw
	expectRunningTask1(mock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		_, finished := f.pollTask(ctx, 1)
		done <- finished
	}()
	<-gw.started
	cancel()
	select {
	case finished := <-done:
		if finished {
			t.Fatal("取消后不应报告已到终态")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后长轮询循环应退出")
	}
	if len(bill.fails) != 0 || len(bill.settles) != 0 {
		t.Fatalf("取消不应动钱: fails=%v settles=%v", bill.fails, bill.settles)
	}
	met(t, mock)
}

// 查询网关出错：这一步交回扫描（不在循环里空转），返回未到终态。
func TestPollTaskHandsBackOnGatewayError(t *testing.T) {
	useFastPollGap(t)
	f, mock, _ := newTestFlow(t, onBill())
	gw := &loopGW{} // 没有预设：Get 报错
	f.gw = gw
	expectRunningTask1(mock)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.id = 1) LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"id", "batch_id", "status"}).
		AddRow(1, 9, model.TaskRunning))
	if batchID, finished := f.pollTask(context.Background(), 1); finished || batchID != 9 || gw.gets() != 1 {
		t.Fatalf("出错应交回扫描: finished=%v batch=%d gets=%d", finished, batchID, gw.gets())
	}
	met(t, mock)
}

// 子任务到终态后立刻给同批派发下一个排队的子任务（不等下一轮扫描），并为它起轮询协程。
func TestSpawnPollDispatchesNextChild(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	f.owner = "inst-a"
	mock.MatchExpectationsInOrder(false) // 新子任务的轮询协程与派发收尾并发
	mock.ExpectExec(qTaskLease + ".*WHERE `id` = 1 AND").WillReturnResult(ok1)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.id = 1) LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"id", "batch_id", "status"}).
		AddRow(1, 9, model.TaskSucceeded))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `lease_owner` = NULL, `lease_until` = NULL, `updated_at` = `updated_at` WHERE `id` = 1 AND `lease_owner` = 'inst-a'")).
		WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `lease_until` = NOW(3), `updated_at` = `updated_at` WHERE `id` = 1")).WillReturnResult(ok1)
	// 派发：抢批次租约 → 本批 1 成功 + 1 排队 → 全局有名额 → 排队→生成中 → 放批次租约
	mock.ExpectExec(qBatchLease + ".*WHERE `id` = 9 AND").WillReturnResult(ok1)
	expectBatchTasks(mock, model.TaskSucceeded, model.TaskQueued)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM `afv_studio_task` AS `t` WHERE (t.status = 1)")).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 1, `updated_at` = NOW(3), `started_at` = NULL WHERE (`id` = 2) AND (`status` = 0)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `lease_owner` = NULL")).WillReturnResult(ok1)
	// 新派发的子任务 2 起了轮询协程（这里租约被别的实例持有，跳过）
	mock.ExpectExec(qTaskLease + ".*WHERE `id` = 2 AND").WillReturnResult(ok0)
	f.spawnPoll(context.Background(), 1)
	f.wg.Wait()
	met(t, mock)
}

// 编排名额按 kind 分池：商拍池占满时图片生成照样拿得到名额。
func TestBatchConcurrencyPerKind(t *testing.T) {
	var c config.Config
	c.Poll.BatchConcurrency = 2
	if c.BatchSlots("tryon") != 2 || c.BatchSlots("pose") != 2 || c.BatchSlots("imagegen") != 4 {
		t.Fatalf("回退值: tryon=%d pose=%d imagegen=%d", c.BatchSlots("tryon"), c.BatchSlots("pose"), c.BatchSlots("imagegen"))
	}
	c.Poll.KindBatchConcurrency.Tryon, c.Poll.KindBatchConcurrency.Pose, c.Poll.KindBatchConcurrency.ImageGen = 1, 3, 6
	if c.BatchSlots("tryon") != 1 || c.BatchSlots("pose") != 3 || c.BatchSlots("imagegen") != 6 {
		t.Fatal("按 kind 配置应生效")
	}
	keys := map[int64]bool{semBatchKey(model.KindTryon): true, semBatchKey(model.KindPose): true, semBatchKey(model.KindImageGen): true, semPoll: true}
	if len(keys) != 4 {
		t.Fatal("三个 kind 与轮询应各用一个名额池")
	}

	f, _, _ := newTestFlow(t, onBill())
	rel, err := f.sem.Acquire(context.Background(), semBatchKey(model.KindTryon), c.BatchSlots("tryon"))
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.sem.Acquire(ctx, semBatchKey(model.KindTryon), c.BatchSlots("tryon")); err == nil {
		t.Fatal("商拍池已满，不应再拿到")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	rel2, err := f.sem.Acquire(ctx2, semBatchKey(model.KindImageGen), c.BatchSlots("imagegen"))
	if err != nil {
		t.Fatalf("图片生成不应被商拍池饿住: %v", err)
	}
	rel2()
}

// 轮询名额不少于全局生成中上限。
func TestPollWorkersAtLeastMaxRunning(t *testing.T) {
	var c config.Config
	if c.PollWorkers() != 64 {
		t.Fatalf("默认 64: %d", c.PollWorkers())
	}
	c.Poll.Workers, c.Poll.MaxRunning = 16, 64
	if c.PollWorkers() != 64 {
		t.Fatalf("Workers < MaxRunning 时取 MaxRunning: %d", c.PollWorkers())
	}
	c.Poll.Workers = 100
	if c.PollWorkers() != 100 {
		t.Fatal("Workers 更大时取 Workers")
	}
}

// ---------------- 并发冻结 ----------------

// parBill 线程安全的假计费：fail 里的请求号冻结失败；barrier>0 时前 barrier 个 Hold 互相等齐再返回（保证真的并发在途）。
type parBill struct {
	mu       sync.Mutex
	fail     map[string]bool
	holds    []string
	fails    []string
	barrier  int
	arrived  int
	gate     chan struct{}
	inflight atomic.Int32
	peak     atomic.Int32
	slow     time.Duration // 冻结成功的每张耗时（让失败那张先返回）
	panicOn  string        // 这个请求号的 Hold 直接 panic
}

func (b *parBill) Mode() string                                     { return billing.ModeOn }
func (b *parBill) CheckAccess(context.Context, int64, string) error { return nil }
func (b *parBill) Hold(_ context.Context, c billing.Charge) (*billing.Reservation, error) {
	n := b.inflight.Add(1)
	defer b.inflight.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if b.barrier > 0 {
		b.mu.Lock()
		b.arrived++
		if b.arrived == b.barrier {
			close(b.gate)
		}
		b.mu.Unlock()
		<-b.gate
	}
	if c.RequestID == b.panicOn {
		panic("hold boom")
	}
	b.mu.Lock()
	failing := b.fail[c.RequestID]
	b.mu.Unlock()
	if !failing && b.slow > 0 {
		time.Sleep(b.slow)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if failing {
		return nil, xerr.Newf(xerr.InsufficientPoints, "点数不足，还差 %d 点", 5)
	}
	b.holds = append(b.holds, c.RequestID)
	return &billing.Reservation{PricePoints: 10}, nil
}
func (b *parBill) Fail(_ context.Context, _ int64, requestID, _ string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = append(b.fails, requestID)
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// 4 张并发冻结、第 3 张失败：4 张同时在途；冻结成功的 3 张各退一次、失败那张 Fail 一次，子任务与批次置失败，返回余额不足。
func TestSubmitParallelHoldPartialFailureRefundsEachOnce(t *testing.T) {
	bill := &parBill{fail: map[string]bool{"studio-simg_203": true}, barrier: 4, gate: make(chan struct{})}
	d, mock := newTestDeps(t, nil)
	d.bill, d.holdConc = bill, 4
	expectSubmitTx(mock, 4)
	mock.MatchExpectationsInOrder(false)
	for _, id := range []int{201, 202, 204} {
		mock.ExpectExec(regexp.QuoteMeta(fmt.Sprintf("SET billing_status = 1, billing_request_id = 'studio-simg_%d' WHERE (id = %d)", id, id))).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(fmt.Sprintf("SET billing_status = 3 WHERE (id = %d)", id))).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(regexp.QuoteMeta("SET status = 3, finished_at = NOW(3), error_reason = '点数不足，还差 5 点' WHERE (batch_id = 100) AND (status = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 5")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET step_log = JSON_ARRAY_APPEND") + ".*hold_failed").WillReturnResult(sqlmock.NewResult(0, 1))

	_, err := d.submit(context.Background(), mustImageGenSpec(t, imageGenReq(4)))
	if codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("应返回余额不足：%v", err)
	}
	if bill.peak.Load() != 4 {
		t.Fatalf("4 张应同时在途冻结: peak=%d", bill.peak.Load())
	}
	if got, want := sorted(bill.holds), []string{"studio-simg_201", "studio-simg_202", "studio-simg_204"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("holds=%v", got)
	}
	// 每张预留单恰好处理一次：成功的 3 张退回 + 失败那张 Fail
	if got, want := sorted(bill.fails), []string{"studio-simg_201", "studio-simg_202", "studio-simg_203", "studio-simg_204"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("fails=%v", got)
	}
	met(t, mock)
}

// 8 张、并发 4、第 1 张就失败：还没开始的不再冻结；已冻结的每张恰好退一次，没冻结的一张都不碰。
func TestSubmitParallelHoldAbortSkipsUnstarted(t *testing.T) {
	bill := &parBill{fail: map[string]bool{"studio-simg_201": true}, slow: 50 * time.Millisecond}
	d, mock := newTestDeps(t, nil)
	d.bill, d.holdConc = bill, 4
	expectSubmitTx(mock, 8)
	mock.MatchExpectationsInOrder(false)
	// 写计费镜像 / 善后的 SQL 条数取决于失败前已在途的张数，这里不逐条断言（未预期的语句只记日志，不影响钱的路径）
	_, err := d.submit(context.Background(), mustImageGenSpec(t, imageGenReq(8)))
	if codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("应返回余额不足：%v", err)
	}
	held := map[string]int{}
	for _, h := range bill.holds {
		held[h]++
	}
	failCount := map[string]int{}
	for _, f := range bill.fails {
		failCount[f]++
	}
	if failCount["studio-simg_201"] != 1 {
		t.Fatalf("失败那张应 Fail 一次: %v", bill.fails)
	}
	for h, n := range held {
		if n != 1 || failCount[h] != 1 {
			t.Fatalf("冻结成功的 %s 应恰好退一次: holds=%v fails=%v", h, bill.holds, bill.fails)
		}
	}
	if len(bill.fails) != len(bill.holds)+1 {
		t.Fatalf("没冻结的不应被退: holds=%v fails=%v", bill.holds, bill.fails)
	}
	if len(bill.holds) > 3 {
		t.Fatalf("第 1 张失败后应跳过还没开始的: holds=%v", bill.holds)
	}
}

// 并发冻结里某张 panic：不打崩进程，按这张冻结失败处理——它 Fail 一次，冻结成功的各退一次，提交返回错误。
func TestSubmitParallelHoldPanicRefundsEachOnce(t *testing.T) {
	bill := &parBill{panicOn: "studio-simg_202"}
	d, mock := newTestDeps(t, nil)
	d.bill, d.holdConc = bill, 4
	expectSubmitTx(mock, 4)
	mock.MatchExpectationsInOrder(false)
	if _, err := d.submit(context.Background(), mustImageGenSpec(t, imageGenReq(4))); err == nil {
		t.Fatal("冻结 panic 应返回错误")
	}
	failCount := map[string]int{}
	for _, f := range bill.fails {
		failCount[f]++
	}
	if failCount["studio-simg_202"] != 1 {
		t.Fatalf("panic 那张应 Fail 一次: %v", bill.fails)
	}
	for _, h := range bill.holds {
		if failCount[h] != 1 {
			t.Fatalf("冻结成功的 %s 应恰好退一次: holds=%v fails=%v", h, bill.holds, bill.fails)
		}
	}
	if len(bill.fails) != len(bill.holds)+1 {
		t.Fatalf("没冻结的不应被退: holds=%v fails=%v", bill.holds, bill.fails)
	}
}

// 同批两个子任务几乎同时到终态：后到的派发撞上本地去重时不丢，由正在派发的持有者补一轮（批次租约被抢两次）。
func TestDispatchBatchCoalescesConcurrentRequests(t *testing.T) {
	f, mock, _ := newTestFlow(t, onBill())
	f.owner = "inst-a"
	// 第一轮抢批次租约慢 80ms（期间第二个请求到达）；两轮都抢不到（别的实例持有），派发本身什么都不做
	mock.ExpectExec(qBatchLease + ".*WHERE `id` = 9 AND").WillDelayFor(80 * time.Millisecond).WillReturnResult(ok0)
	mock.ExpectExec(qBatchLease + ".*WHERE `id` = 9 AND").WillReturnResult(ok0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.dispatchBatch(context.Background(), 9)
	}()
	time.Sleep(20 * time.Millisecond)
	f.dispatchBatch(context.Background(), 9) // 撞上去重：只记标记立即返回
	<-done
	met(t, mock)
	if _, left := f.redispatch.Load(int64(9)); left {
		t.Fatal("标记应已被消费")
	}
}
