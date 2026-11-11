package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"google.golang.org/grpc"
)

const (
	callA = "call_aaaaaaaaaa01"
	callB = "call_aaaaaaaaaa02"
)

func fastWatch(t *testing.T) {
	old := watchMinGap
	watchMinGap = time.Millisecond
	t.Cleanup(func() { watchMinGap = old })
}

func callJSON(id, status, url string) []byte {
	c := aigateway.Call{ID: id, Status: status}
	if status == aigateway.StatusSucceeded {
		c.Result = &aigateway.Result{Outputs: []aigateway.OutputItem{{Status: aigateway.StatusSucceeded,
			Media: &aigateway.MediaOut{URL: url, Width: 1024, Height: 1024}}}}
	}
	b, _ := json.Marshal(c)
	return b
}

var runRowCols = []string{"id", "canvas_id", "node_id", "user_id", "kind", "backend", "status", "billing_status",
	"requested_count", "backend_ref", "task_id", "request", "created_at"}

func expectRunningRun(mock sqlmock.Sqlmock, id int64, ref string, count int) {
	mock.ExpectQuery(regexp.QuoteMeta(fmt.Sprintf("WHERE (r.id = %d) LIMIT 1", id))).WillReturnRows(sqlmock.NewRows(runRowCols).
		AddRow(id, "cv_1", "m", 7, "image", "gateway", model.RunRunning, model.BillNone, count, ref, "cv_t", `{"model":"Banana"}`, time.Now()))
}

// 长轮询循环：调用未终态就接着 GET ?wait=LongPollWait（不靠 3 秒扫描），终态即收尾（CAS 成功）。
func TestWatchRunLoopsUntilTerminal(t *testing.T) {
	fastWatch(t)
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.s.Config.AiGateway.LongPollWait = 25
	var n atomic.Int32
	var mu sync.Mutex
	var waits []string
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		mu.Lock()
		waits = append(waits, r.URL.Query().Get("wait"))
		mu.Unlock()
		if n.Add(1) < 3 {
			_, _ = w.Write(callJSON(callA, aigateway.StatusRunning, ""))
			return
		}
		_, _ = w.Write(callJSON(callA, aigateway.StatusSucceeded, "https://gw/out.png"))
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	for i := 0; i < 3; i++ {
		expectRunningRun(mock, 5, callA, 1)
	}
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*`candidates` = .*out.png.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.watchRun(context.Background(), 5)
	if n.Load() != 3 {
		t.Fatalf("应长轮询 3 次: %d", n.Load())
	}
	for _, w := range waits {
		if w != "25" {
			t.Fatalf("每次都应带 wait=LongPollWait: %v", waits)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// count=2：先长轮询第一张，到终态后批量查其余的；没到终态的下一圈接着长轮询，全部终态才收尾。
func TestWatchRunMultiCall(t *testing.T) {
	fastWatch(t)
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	var bGets atomic.Int32
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.URL.Path == "/v1/calls/"+callA:
			_, _ = w.Write(callJSON(callA, aigateway.StatusSucceeded, "https://gw/a.png"))
		case r.URL.Path == "/v1/calls:batchGet":
			_, _ = fmt.Fprintf(w, `{"items":[%s]}`, callJSON(callB, aigateway.StatusRunning, ""))
		case r.URL.Path == "/v1/calls/"+callB:
			bGets.Add(1)
			_, _ = w.Write(callJSON(callB, aigateway.StatusSucceeded, "https://gw/b.png"))
		default:
			w.WriteHeader(404)
		}
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	expectRunningRun(mock, 5, callA+","+callB, 2)
	expectRunningRun(mock, 5, callA+","+callB, 2)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*a.png.*b.png.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.watchRun(context.Background(), 5)
	if bGets.Load() != 1 || strings.Count(strings.Join(rec.paths, " "), "/v1/calls/"+callA) != 1 {
		t.Fatalf("已终态的调用不应再查: %v", rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 未终态且已超时：置失败退款、取消上游调用。
func TestWatchRunTimeout(t *testing.T) {
	fastWatch(t)
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			_, _ = w.Write(callJSON(callA, aigateway.StatusCancelled, ""))
			return
		}
		_, _ = w.Write(callJSON(callA, aigateway.StatusRunning, ""))
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.id = 5) LIMIT 1")).WillReturnRows(sqlmock.NewRows(runRowCols).
		AddRow(5, "cv_1", "m", 7, "image", "gateway", model.RunRunning, model.BillNone, 1, callA, "cv_t", `{}`, time.Now().Add(-time.Hour)))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_code` = 'TIMEOUT'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.watchRun(context.Background(), 5)
	if !rec.has("/v1/calls/" + callA + "/cancel") {
		t.Fatalf("超时应取消上游: %v", rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 长轮询挂起中租约丢失 / 进程停止（ctx 取消）：立即退出，不收尾、不动钱，交给接管方 / 扫描。
func TestWatchRunStopsOnLeaseLoss(t *testing.T) {
	fastWatch(t)
	c, mock := mockCore(t)
	started := make(chan struct{}, 1)
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	expectRunningRun(mock, 5, callA, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.watchRun(ctx, 5); close(done) }()
	<-started
	cancel() // 心跳发现租约被接管 → Keep 取消 ctx
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后长轮询应立即退出")
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有任何 UPDATE
		t.Fatal(err)
	}
}

// 协程池：本地去重、名额有界（满了交给扫描）、抢不到租约不推进、Stop 时取消并等全部退出、租约释放。
func TestWatcherBoundedDedupAndShutdown(t *testing.T) {
	c := testCore()
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: "http://127.0.0.1:1", KeyID: "k", Secret: "s"})
	c.s.Config.Poll.Watchers = 1
	root, cancel := context.WithCancel(context.Background())
	w := newWatcher(root, c)
	entered := make(chan int64, 4)
	w.keep = func(ctx context.Context, id int64) (context.Context, func(), bool, error) {
		entered <- id
		if id == 3 {
			return nil, nil, false, nil // 别的实例持有
		}
		<-ctx.Done() // 模拟长轮询挂起直到进程停止
		return nil, nil, false, nil
	}
	if !w.Watch(1) {
		t.Fatal("第一个应起协程")
	}
	<-entered
	if w.Watch(1) {
		t.Fatal("同一 run 不应起第二个协程")
	}
	if w.Watch(2) {
		t.Fatal("名额满时应返回 false（交给扫描）")
	}
	cancel()
	w.close()
	if w.Watch(3) {
		t.Fatal("关闭后不应再接")
	}

	// 抢不到租约：协程直接退出、释放名额
	w2 := newWatcher(context.Background(), c)
	w2.keep = w.keep
	if !w2.Watch(3) {
		t.Fatal("应起协程")
	}
	<-entered
	w2.close()
	if len(w2.slots) != 0 {
		t.Fatal("退出后应释放名额")
	}
}

// 租约在长轮询中丢失：watcher 协程退出并调用 stop（释放租约）、释放名额。
func TestWatcherLeaseLostReleases(t *testing.T) {
	fastWatch(t)
	c, mock := mockCore(t)
	started := make(chan struct{}, 1)
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	expectRunningRun(mock, 5, callA, 1)
	w := newWatcher(context.Background(), c)
	var stopped atomic.Bool
	lost := make(chan struct{})
	w.keep = func(ctx context.Context, id int64) (context.Context, func(), bool, error) {
		lctx, cancel := context.WithCancel(ctx)
		go func() { <-lost; cancel() }()
		return lctx, func() { stopped.Store(true); cancel() }, true, nil
	}
	if !w.Watch(5) {
		t.Fatal("应起协程")
	}
	<-started
	close(lost)
	w.close()
	if !stopped.Load() || len(w.slots) != 0 {
		t.Fatalf("丢租约后应退出并释放: stopped=%v slots=%d", stopped.Load(), len(w.slots))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------- 产物并发转存 ----------------

// copyMedia 媒体服务：resources/copy 按幂等键派生 rid（同键同 rid），fail 里的键第一次回 503。记录每个键的次数与并发峰值。
type copyMedia struct {
	mu       sync.Mutex
	keys     map[string]int
	fail     map[string]bool
	inflight atomic.Int32
	peak     atomic.Int32
}

func newCopyMedia(t *testing.T, fail ...string) (*mediasvc.Client, *copyMedia) {
	cm := &copyMedia{keys: map[string]int{}, fail: map[string]bool{}}
	for _, k := range fail {
		cm.fail[k] = true
	}
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/v1/mediasvc/resources/copy" {
			w.WriteHeader(404)
			return
		}
		n := cm.inflight.Add(1)
		defer cm.inflight.Add(-1)
		for p := cm.peak.Load(); n > p && !cm.peak.CompareAndSwap(p, n); p = cm.peak.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		key := r.Header.Get("Idempotency-Key")
		cm.mu.Lock()
		cm.keys[key]++
		failNow := cm.fail[key]
		cm.mu.Unlock()
		if failNow {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"code":"transient","message":"busy"}}`))
			return
		}
		idx := key[strings.LastIndex(key, ":")+1:]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": map[string]any{"rid": "ast" + idx + "-asset-aaaaaaaaaaaaaaaaaaaaaaaaaa", "width": 10, "height": 10},
			"url":    map[string]any{"kind": "public", "value": "https://cdn/" + idx + ".png"},
		})
	})
	return mediasvc.New([]string{srv.URL}, "tok"), cm
}

// 4 张产物并发转存：幂等键 canvas-copy:{task}:{i}，候选顺序与产物下标一致；某张暂时失败 → 整个 run 下轮重试，
// 重试用同一批键（媒体服务按键得到同一个 rid，不会重复落桶），之后才 CAS 成功。
func TestSucceedParallelCopyIdempotent(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	media, cm := newCopyMedia(t, "canvas-copy:cv_t:2")
	c.s.Media = media
	run := gatewayRun(5, model.RunRunning)
	run.RequestedCount = 4
	var outs []mediaOutput
	for i := 0; i < 4; i++ {
		outs = append(outs, mediaOutput{RID: fmt.Sprintf("out%d-work-aaaaaaaaaaaaaaaaaaaaaaaaaa", i), W: 8, H: 8})
	}
	o := outcome{done: true, ok: true, actual: 4, outputs: outs}

	// 第 1 轮：第 2 张一直 503（媒体服务客户端自己会退避重试，这里用短 ctx 让它在本轮内放弃）→ 暂时失败，不 CAS，下轮重试
	ctx1, cancel1 := context.WithTimeout(context.Background(), 400*time.Millisecond)
	c.succeed(ctx1, run, o, nil)
	cancel1()
	if run.Status != model.RunRunning {
		t.Fatal("暂时失败不应收尾")
	}
	cm.mu.Lock()
	cm.fail = map[string]bool{}
	cm.mu.Unlock()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*cdn/0.png.*cdn/1.png.*cdn/2.png.*cdn/3.png.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.succeed(context.Background(), run, o, nil)
	if run.Status != model.RunSucceeded {
		t.Fatal("重试应成功收尾")
	}
	var keys []string
	for k, n := range cm.keys {
		keys = append(keys, k)
		if n < 2 {
			t.Fatalf("两轮都应转存 %s: %v", k, cm.keys)
		}
	}
	sort.Strings(keys)
	want := []string{"canvas-copy:cv_t:0", "canvas-copy:cv_t:1", "canvas-copy:cv_t:2", "canvas-copy:cv_t:3"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("两轮只应出现这 4 个幂等键（重试同键，不会新落一份）: %v", cm.keys)
	}
	if cm.peak.Load() < 2 {
		t.Fatalf("应并发转存: peak=%d", cm.peak.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 永久失败（4xx）→ 失败退款，与原先逐张转存一致。
func TestSucceedParallelCopyPermanentFails(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"bad rid"}}`))
	})
	c.s.Media = mediasvc.New([]string{srv.URL}, "tok")
	run := gatewayRun(5, model.RunRunning)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_code` = 'UPSTREAM_FAILED', `error_reason` = '产物转存失败'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.succeed(context.Background(), run, outcome{done: true, ok: true, actual: 2, outputs: []mediaOutput{
		{RID: "out0-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"}, {RID: "out1-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"}}}, nil)
	if run.Status != model.RunFailed {
		t.Fatal("永久失败应置失败")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------- 并发提交 ----------------

// submitGW gateway：按幂等键末位下标决定结果（bad → 400 永久，busy → 503 暂时，其余建调用）；记录取消。
func submitGW(t *testing.T, bad, busy string) (*aigateway.Client, *recorder, *atomic.Int32) {
	var peak, inflight atomic.Int32
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			_, _ = w.Write(callJSON(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/calls/"), "/cancel"), aigateway.StatusCancelled, ""))
			return
		}
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		key := r.Header.Get("Idempotency-Key")
		idx := key[strings.LastIndex(key, ":")+1:]
		switch idx {
		case bad:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"input_rejected","message":"bad prompt"}}`))
		case busy:
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"busy"}}`))
		default:
			_, _ = w.Write(callJSON("call_aaaaaaaaaa1"+idx, aigateway.StatusPending, ""))
		}
	})
	return aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"}), rec, &peak
}

// count=3 并发提交、第 2 张（下标 1）被拒：整单失败退款（billing_status=refunded、points=0），已建好的另两张调用全部取消。
func TestDispatchParallelSubmitPartialFailureRefunds(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billingOn(t)
	c.s.Config.Poll.MaxSubmitAttempts = 5
	gw, rec, peak := submitGW(t, "1", "")
	c.s.Gateway = gw
	run := gatewayRun(5, model.RunQueued)
	run.BillingStatus, run.Points, run.RequestedCount = model.BillHeld, 120, 3
	sp := *imageSpec
	sp.Count = 3
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`billing_status` = 'refunded', `error_code` = 'PARAM_INVALID'.*`points` = 0 WHERE \\(`id` = 5\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(context.Background(), run, &sp, ""); err == nil {
		t.Fatal("被拒应返回错误")
	}
	if run.Status != model.RunFailed || run.BillingStatus != model.BillRefunded {
		t.Fatalf("应失败并退款: status=%d bill=%s", run.Status, run.BillingStatus)
	}
	if !rec.has("/v1/calls/call_aaaaaaaaaa10/cancel") || !rec.has("/v1/calls/call_aaaaaaaaaa12/cancel") {
		t.Fatalf("已建好的调用都应取消: %v", rec.paths)
	}
	if peak.Load() < 2 {
		t.Fatalf("应并发提交: peak=%d", peak.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 某张暂时失败（503）且还有重试次数：留在排队、attempt+1，已建好的调用不取消（重提同幂等键拿回它们），不退款。
func TestDispatchParallelSubmitTemporaryKeepsCalls(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billingOn(t)
	c.s.Config.Poll.MaxSubmitAttempts = 5
	gw, rec, _ := submitGW(t, "", "2")
	c.s.Gateway = gw
	run := gatewayRun(5, model.RunQueued)
	run.BillingStatus, run.RequestedCount = model.BillHeld, 3
	sp := *imageSpec
	sp.Count = 3
	mock.ExpectExec(regexp.QuoteMeta("SET `attempt` = 1")).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(context.Background(), run, &sp, ""); err != nil {
		t.Fatalf("暂时失败应留在排队: %v", err)
	}
	if run.Status != model.RunQueued || run.BillingStatus != model.BillHeld {
		t.Fatalf("不应收尾 / 退款: %+v", run)
	}
	for _, p := range rec.paths {
		if strings.HasSuffix(p, "/cancel") {
			t.Fatalf("暂时失败不应取消调用: %v", rec.paths)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------- 扫描里 Java / studio 并发查询 ----------------

type slowStudio struct {
	studio.Studio
	inflight, peak atomic.Int32
}

func (s *slowStudio) GetBatch(context.Context, *studio.GetBatchReq, ...grpc.CallOption) (*studio.BatchDetail, error) {
	n := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for p := s.peak.Load(); n > p && !s.peak.CompareAndSwap(p, n); p = s.peak.Load() {
	}
	time.Sleep(50 * time.Millisecond)
	return &studio.BatchDetail{Batch: &studio.BatchInfo{Status: 3}}, nil // 未终态
}

func TestPollRunningLookupsInParallel(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.s.Config.Poll.BatchSize, c.s.Config.Poll.LookupConcurrency = 100, 8
	st := &slowStudio{}
	c.s.Studio = st
	mock.MatchExpectationsInOrder(false)
	rows := sqlmock.NewRows([]string{"id", "user_id", "status", "backend", "kind", "billing_status", "backend_ref", "created_at"})
	for i := 61; i <= 64; i++ {
		rows.AddRow(i, 7, 1, "studio", "tool_tryon", "studio", fmt.Sprint(900+i), time.Now())
	}
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.status = 1) ") + qFreeRun).WillReturnRows(rows)
	for i := 61; i <= 64; i++ {
		mock.ExpectExec(qRunLease + fmt.Sprintf(".*WHERE `id` = %d AND", i)).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(qRunRelease + fmt.Sprintf("WHERE `id` = %d AND", i)).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET updated_at = NOW(3) WHERE (id IN (61, 62, 63, 64)) AND (status = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 4))
	start := time.Now()
	if got := c.pollRunning(context.Background()); len(got) != 0 {
		t.Fatalf("studio run 不交给长轮询: %v", got)
	}
	if st.peak.Load() != 4 || time.Since(start) > 180*time.Millisecond {
		t.Fatalf("4 个 studio 查询应并发: peak=%d took=%v", st.peak.Load(), time.Since(start))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 扫描处理后仍在跑的 gateway run：租约放掉后交还调用方（Tick 交给长轮询协程）。
func TestPollRunningReturnsStillRunningGateway(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.s.Config.Poll.BatchSize = 100
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = fmt.Fprintf(w, `{"items":[%s]}`, callJSON(callA, aigateway.StatusRunning, ""))
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.status = 1) ") + qFreeRun).WillReturnRows(
		sqlmock.NewRows([]string{"id", "user_id", "status", "backend", "kind", "billing_status", "backend_ref", "created_at"}).
			AddRow(71, 7, 1, "gateway", "image", "none", callA, time.Now()))
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 71 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET updated_at = NOW(3) WHERE (id IN (71))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 71 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	got := c.pollRunning(context.Background())
	if len(got) != 1 || got[0].ID != 71 {
		t.Fatalf("仍在跑的 gateway run 应交还: %v", got)
	}
	var handed []int64
	c.s.WatchRun = func(id int64) { handed = append(handed, id) }
	c.handOff(got[0])
	if len(handed) != 1 || handed[0] != 71 {
		t.Fatal("应交给长轮询")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// RunNode 内联提交成功后：放掉租约，再交给长轮询（顺序不能反，否则协程抢不到租约）。
func TestDispatchInlineHandsOffAfterRelease(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_, _ = w.Write(callJSON(callA, aigateway.StatusPending, ""))
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	var handed []int64
	c.s.WatchRun = func(id int64) {
		// 此刻释放租约的语句必须已经执行（全部预期都已满足）
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("交给长轮询时租约还没放: %v", err)
		}
		handed = append(handed, id)
	}
	run := gatewayRun(81, model.RunQueued)
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 81 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 1, .*`backend_ref` = '" + callA + "'.* WHERE \\(`id` = 81\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 81 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatchInline(context.Background(), run, imageSpec, ""); err != nil || run.Status != model.RunRunning {
		t.Fatalf("应提交成功: status=%d err=%v", run.Status, err)
	}
	if len(handed) != 1 || handed[0] != 81 {
		t.Fatalf("应交给长轮询: %v", handed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// NewPoller 把长轮询入口挂到 svc.WatchRun；没 Start 就 Stop 不阻塞，Stop 后不再接新的长轮询。
func TestPollerWiresWatchRunAndStops(t *testing.T) {
	c := testCore()
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: "http://127.0.0.1:1", KeyID: "k", Secret: "s"})
	p := NewPoller(c.s)
	if c.s.WatchRun == nil {
		t.Fatal("应挂上 WatchRun")
	}
	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("没 Start 的 Stop 不应阻塞")
	}
	if p.w.Watch(1) {
		t.Fatal("Stop 后不应再接")
	}
}

// 并发提交：首个错误永久（下标 0 被拒）、另一张暂时失败（下标 2 的 503，网关上可能已建调用）→ 报的仍是被拒那张（非暂时性），
// 但要标成「可能留有调用」，dispatch 据此按幂等键回收；只有永久错误时不标。
func TestSubmitGatewayPermanentWithTemporaryMarksMaybeCreated(t *testing.T) {
	c := testCore()
	c.s.Config.Poll.FanoutConcurrency = 4
	sp := *imageSpec
	sp.Count = 3
	run := gatewayRun(5, model.RunQueued)
	run.RequestedCount = 3

	gw, _, _ := submitGW(t, "0", "2")
	c.s.Gateway = gw
	ids, err := c.submitGateway(context.Background(), run, &sp)
	if err == nil || isTemporary(err) || !submitMaybeCreated(err) {
		t.Fatalf("永久 + 暂时：应非暂时性但标可能留有调用: %v", err)
	}
	if code, _, ok := xerr.Parse(err); !ok || code != xerr.ParamError {
		t.Fatalf("报错应仍是被拒那张的参数错误: %v", err)
	}
	if len(ids) != 1 || ids[0] != "call_aaaaaaaaaa11" {
		t.Fatalf("应返回已拿到的调用: %v", ids)
	}

	gw2, _, _ := submitGW(t, "0", "")
	c.s.Gateway = gw2
	if _, err := c.submitGateway(context.Background(), run, &sp); err == nil || submitMaybeCreated(err) {
		t.Fatalf("只有永久错误不应标可能留有调用: %v", err)
	}
}

// fanout：子协程 panic 不打崩进程，等全部返回后在调用方重新抛出；其余下标照常执行。
func TestFanoutRepanicsInCaller(t *testing.T) {
	var ran atomic.Int32
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("应在调用方重新抛出 panic: %v", r)
		}
		if ran.Load() != 3 {
			t.Fatalf("其余下标应照常执行完: %d", ran.Load())
		}
	}()
	fanout(4, 4, func(i int) error {
		if i == 1 {
			panic("boom")
		}
		time.Sleep(10 * time.Millisecond)
		ran.Add(1)
		return nil
	})
	t.Fatal("不应走到这里")
}
