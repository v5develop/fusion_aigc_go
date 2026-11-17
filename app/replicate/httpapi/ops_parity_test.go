package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/taskcenter"

	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

// REST 与 Ops 一致性（P0 切片 S1 的验收）：同一组输入分别走 REST 处理器与 Ops 强类型方法，
// 经同一个 writeErr / OkJson 渲染后逐字节比较响应（状态码、Retry-After 头、body），并比较任务中心 Kick 次数与限频额度消耗。
// 范围：不需要真实库就能走到的口径——总开关、登录、参数校验、限频 take（含被拒不占额度）、ExistingConfirm 重放短路（不限频不计次）。
// 内核里的业务分支由 core 自己的用例覆盖；这里证明 REST 与 Ops 没有各自带一份口径。

// stubStore 只实现重放短路与 JobView 要用到的读方法；其余方法没实现（调到会 panic，等于断言「不该走到内核那一步」）。
type stubStore struct{ core.Store }

func (stubStore) GetJobByRequest(_ context.Context, userID int64, key string) (*model.Job, error) {
	if key == "k-replay" {
		return stubJob(userID), nil
	}
	return nil, nil
}
func (stubStore) GetPlanByNo(context.Context, int64, string) (*model.Plan, error) { return nil, nil }
func (stubStore) GetJob(_ context.Context, id int64) (*model.Job, error) {
	return stubJob(7), nil
}
func (stubStore) ListSteps(context.Context, int64) ([]*model.Step, error)     { return nil, nil }
func (stubStore) ListCharges(context.Context, int64) ([]*model.Charge, error) { return nil, nil }

func stubJob(uid int64) *model.Job {
	return &model.Job{ID: 42, UserID: uid, PlanNo: "VRPx", PlanHash: "h", Status: model.JobRunning, SegTotal: 1, Input: []byte(`{}`)}
}

type pEnv struct {
	m     *Module
	kicks atomic.Int32
}

func newPEnv(kv map[string]string) *pEnv {
	if kv == nil {
		kv = map[string]string{}
	}
	if _, ok := kv[KeyEnabled]; !ok {
		kv[KeyEnabled] = "true"
	}
	rt := newRuntime(core.DefaultConfig(), false, func(context.Context) (map[string]string, error) { return kv, nil },
		func(cfg core.Config, _ bool) *core.Service { return core.New(cfg, stubStore{}, nil) })
	e := &pEnv{}
	e.m = &Module{rt: rt, limiter: newRateLimiter(nil, nil)}
	fixed := time.Date(2026, 11, 17, 10, 0, 30, 0, cst)
	e.m.limiter.now = func() time.Time { return fixed }
	e.m.taskSync = taskcenter.NewAsyncSync("test", func(context.Context) error { e.kicks.Add(1); return nil })
	rt.Service(context.Background()) // 先读一次运行参数，限频参数才生效
	return e
}

type rendered struct {
	status int
	retry  string
	body   string
}

func render(w *httptest.ResponseRecorder) rendered {
	return rendered{w.Code, w.Header().Get("Retry-After"), w.Body.String()}
}

// renderOps 把 Ops 的结果经与 REST 同一套渲染写出来。
func renderOps(out any, err error) rendered {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		writeErr(w, r, err)
	} else {
		httpx.OkJsonCtx(r.Context(), w, out)
	}
	return render(w)
}

type pcase struct {
	name      string
	kv        map[string]string
	uid       int64 // 0 → 7
	noModule  bool
	write     bool
	preTake   string // 先占掉这个动作的额度（限频参数设成每分钟 1）
	fn        func(h *handlers) handlerFn
	method    string
	path      string
	body      string
	vars      map[string]string
	key       string // Idempotency-Key
	ops       func(sc *Scope) (any, error)
	wantKicks int
	// wantStatus / wantSub 额外断言（非空才查），防止两边「一致地错」
	wantStatus int
	wantSub    string
}

func i64(v int64) *int64 { return &v }

func pcases() []pcase {
	rate1 := func(extra map[string]string) map[string]string {
		kv := map[string]string{KeyRateAnalysisPerMinute: "1", KeyRatePlanPerMinute: "1", KeyRateConfirmPerMinute: "1", KeyRateAnnotatePerMinute: "1",
			KeyRateAnalysisPerDay: "100", KeyRatePlanPerDay: "100", KeyRateConfirmPerDay: "100", KeyRateAnnotatePerDay: "100", KeyRateMaxInFlightJobs: "0"}
		for k, v := range extra {
			kv[k] = v
		}
		return kv
	}
	off := map[string]string{KeyEnabled: "false"}
	return []pcase{
		{name: "总开关关着-确认回7003", kv: off, write: true, fn: func(h *handlers) handlerFn { return h.confirm }, method: "POST", path: "/x", vars: map[string]string{"no": "VRPx"},
			body: `{"planHash":"h","expectedPoints":100}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Confirm(context.Background(), ConfirmArgs{PlanNo: "VRPx", PlanHash: "h", ExpectedPoints: i64(100)})
			},
			wantStatus: 200, wantSub: `"code":7003`},
		{name: "总开关关着-出方案回7003", kv: off, write: true, fn: func(h *handlers) handlerFn { return h.createPlan }, method: "POST", path: "/x", body: `{"analysisNo":"VRA1"}`,
			ops: func(sc *Scope) (any, error) {
				return sc.CreatePlan(context.Background(), core.PlanReq{AnalysisNo: "VRA1"})
			}, wantSub: `"code":7003`},
		{name: "总开关关着-重做回7003", kv: off, write: true, fn: func(h *handlers) handlerFn { return h.redo }, method: "POST", path: "/x", vars: map[string]string{"id": "42", "k": "1"},
			body: `{"expectedPoints":10}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Redo(context.Background(), RedoArgs{JobID: 42, SegIndex: 1, ExpectedPoints: i64(10)})
			}, wantSub: `"code":7003`},
		{name: "总开关关着-limits照常可读且带enabled=false", kv: off, fn: func(h *handlers) handlerFn { return h.limits }, method: "GET", path: "/x",
			ops: func(sc *Scope) (any, error) { return sc.Limits(), nil }, wantSub: `"enabled":false`},
		{name: "总开关开着-limits带enabled=true", fn: func(h *handlers) handlerFn { return h.limits }, method: "GET", path: "/x",
			ops: func(sc *Scope) (any, error) { return sc.Limits(), nil }, wantSub: `"enabled":true`},
		{name: "模块没装配-7003", noModule: true, fn: func(h *handlers) handlerFn { return h.limits }, method: "GET", path: "/x",
			ops: func(sc *Scope) (any, error) { return sc.Limits(), nil }, wantSub: `"code":7003`},
		{name: "未登录", uid: -1, write: true, fn: func(h *handlers) handlerFn { return h.stop }, method: "POST", path: "/x", vars: map[string]string{"id": "42"},
			ops: func(sc *Scope) (any, error) { return sc.Stop(context.Background(), 42) }, wantSub: "请先登录"},
		// expectedPoints 必填 >0（在重放 / 限频 / 内核之前）
		{name: "确认缺expectedPoints", write: true, fn: func(h *handlers) handlerFn { return h.confirm }, method: "POST", path: "/x", vars: map[string]string{"no": "VRPx"},
			body: `{"planHash":"h","stage":"first"}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Confirm(context.Background(), ConfirmArgs{PlanNo: "VRPx", PlanHash: "h", Stage: "first"})
			}, wantSub: "expectedPoints"},
		{name: "确认expectedPoints=0", write: true, fn: func(h *handlers) handlerFn { return h.confirm }, method: "POST", path: "/x", vars: map[string]string{"no": "VRPx"},
			body: `{"expectedPoints":0}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Confirm(context.Background(), ConfirmArgs{PlanNo: "VRPx", ExpectedPoints: i64(0)})
			}, wantSub: "缺少确认点数"},
		{name: "继续expectedPoints为负", write: true, fn: func(h *handlers) handlerFn { return h.cont }, method: "POST", path: "/x", vars: map[string]string{"id": "42"},
			body: `{"planHash":"h","expectedPoints":-5}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Continue(context.Background(), ContinueArgs{JobID: 42, PlanHash: "h", ExpectedPoints: i64(-5)})
			}, wantSub: "缺少继续点数"},
		{name: "重做缺expectedPoints", write: true, fn: func(h *handlers) handlerFn { return h.redo }, method: "POST", path: "/x", vars: map[string]string{"id": "42", "k": "2"},
			body: `{}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Redo(context.Background(), RedoArgs{JobID: 42, SegIndex: 2})
			}, wantSub: "缺少重做点数"},
		{name: "确认stage非法", write: true, fn: func(h *handlers) handlerFn { return h.confirm }, method: "POST", path: "/x", vars: map[string]string{"no": "VRPx"},
			body: `{"stage":"half","expectedPoints":100}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Confirm(context.Background(), ConfirmArgs{PlanNo: "VRPx", Stage: "half", ExpectedPoints: i64(100)})
			}, wantSub: "stage 只能是 first 或 all"},
		{name: "方案号过长", write: true, fn: func(h *handlers) handlerFn { return h.confirm }, method: "POST", path: "/x", vars: map[string]string{"no": strings.Repeat("x", 65)},
			body: `{"expectedPoints":100}`,
			ops: func(sc *Scope) (any, error) {
				return sc.Confirm(context.Background(), ConfirmArgs{PlanNo: strings.Repeat("x", 65), ExpectedPoints: i64(100)})
			}, wantSub: "no 不合法"},
		{name: "分析缺视频地址", write: true, fn: func(h *handlers) handlerFn { return h.createAnalysis }, method: "POST", path: "/x", body: `{"sourceUrl":"  "}`,
			ops: func(sc *Scope) (any, error) { return sc.CreateAnalysis(context.Background(), "  ") }, wantSub: "请先上传要做同款的视频"},
		{name: "分析地址过长", write: true, fn: func(h *handlers) handlerFn { return h.createAnalysis }, method: "POST", path: "/x",
			body: `{"sourceUrl":"` + strings.Repeat("a", 1025) + `"}`,
			ops: func(sc *Scope) (any, error) {
				return sc.CreateAnalysis(context.Background(), strings.Repeat("a", 1025))
			}, wantSub: "视频地址太长"},
		{name: "出方案缺分析编号", write: true, fn: func(h *handlers) handlerFn { return h.createPlan }, method: "POST", path: "/x", body: `{}`,
			ops: func(sc *Scope) (any, error) { return sc.CreatePlan(context.Background(), core.PlanReq{}) }, wantSub: "缺少分析编号"},
		{name: "改标注缺annotations（先于限频）", kv: rate1(nil), preTake: ActionAnnotate, write: true, fn: func(h *handlers) handlerFn { return h.patchAnnotations },
			method: "PATCH", path: "/x", vars: map[string]string{"no": "VRPx"}, body: `{}`,
			ops: func(sc *Scope) (any, error) {
				return sc.PatchAnnotations(context.Background(), core.PatchReq{PlanNo: "VRPx"}, false)
			}, wantSub: "缺少 annotations"},
		{name: "路径参数id不合法", write: true, fn: func(h *handlers) handlerFn { return h.cancelRest }, method: "POST", path: "/x", vars: map[string]string{"id": "0"},
			ops: func(sc *Scope) (any, error) { return sc.CancelRest(context.Background(), 0) }, wantSub: "id 不合法"},
		// 限频：额度被占满后被拒（429），被拒的请求不进内核
		{name: "限频-确认", kv: rate1(nil), preTake: ActionConfirm, write: true, fn: func(h *handlers) handlerFn { return h.confirm }, method: "POST", path: "/x",
			vars: map[string]string{"no": "VRPx"}, body: `{"planHash":"h","expectedPoints":100}`, key: "k-new",
			ops: func(sc *Scope) (any, error) {
				return sc.Confirm(context.Background(), ConfirmArgs{PlanNo: "VRPx", PlanHash: "h", ExpectedPoints: i64(100), IdempotencyKey: "k-new"})
			}, wantStatus: 429, wantSub: `"reason":"RATE_LIMITED"`},
		{name: "限频-分析", kv: rate1(nil), preTake: ActionAnalysis, write: true, fn: func(h *handlers) handlerFn { return h.createAnalysis }, method: "POST", path: "/x",
			body: `{"sourceUrl":"https://x/video/7/20260101/a.mp4"}`,
			ops: func(sc *Scope) (any, error) {
				return sc.CreateAnalysis(context.Background(), "https://x/video/7/20260101/a.mp4")
			}, wantStatus: 429, wantSub: "操作太快了"},
		{name: "限频-出方案", kv: rate1(nil), preTake: ActionPlan, write: true, fn: func(h *handlers) handlerFn { return h.createPlan }, method: "POST", path: "/x",
			body: `{"analysisNo":"VRA1"}`,
			ops: func(sc *Scope) (any, error) {
				return sc.CreatePlan(context.Background(), core.PlanReq{AnalysisNo: "VRA1"})
			}, wantStatus: 429},
		{name: "限频-改标注", kv: rate1(nil), preTake: ActionAnnotate, write: true, fn: func(h *handlers) handlerFn { return h.patchAnnotations }, method: "PATCH", path: "/x",
			vars: map[string]string{"no": "VRPx"}, body: `{"annotations":{"dims":{}}}`,
			ops: func(sc *Scope) (any, error) {
				return sc.PatchAnnotations(context.Background(), core.PatchReq{PlanNo: "VRPx"}, true)
			}, wantStatus: 429},
		{name: "限频-再做一条与改标注同一计数", kv: rate1(nil), preTake: ActionAnnotate, write: true, fn: func(h *handlers) handlerFn { return h.reuseSavedPlan }, method: "POST", path: "/x",
			vars: map[string]string{"id": "5"}, body: `{}`, key: "k-r",
			ops: func(sc *Scope) (any, error) {
				return sc.ReuseSavedPlan(context.Background(), core.ReuseReq{SavedID: 5, IdempotencyKey: "k-r"})
			}, wantStatus: 429},
		// 重放短路：同幂等键已建过 job → 直接返回已有 job，不走限频、不占额度（限频额度已占满也不被挡）
		{name: "确认重放不限频", kv: rate1(nil), preTake: ActionConfirm, write: true, fn: func(h *handlers) handlerFn { return h.confirm }, method: "POST", path: "/x",
			vars: map[string]string{"no": "VRPx"}, body: `{"planHash":"h","expectedPoints":100}`, key: "k-replay", wantKicks: 1,
			ops: func(sc *Scope) (any, error) {
				return sc.Confirm(context.Background(), ConfirmArgs{PlanNo: "VRPx", PlanHash: "h", ExpectedPoints: i64(100), IdempotencyKey: "k-replay"})
			}, wantStatus: 200, wantSub: `"jobId":42`},
		// 只读：不 Kick
		{name: "读任务不kick", fn: func(h *handlers) handlerFn { return h.job }, method: "GET", path: "/x", vars: map[string]string{"id": "42"},
			ops: func(sc *Scope) (any, error) { return sc.JobView(context.Background(), 42) }, wantStatus: 200, wantSub: `"jobId":42`},
		// 已存定妆图（只读）：REST 与 Ops 同口径；方案不存在 → 4001；总开关关着照常可读（不被 7003 挡）；不 Kick
		{name: "读定妆图-方案不存在回4001", fn: func(h *handlers) handlerFn { return h.getPreview }, method: "GET", path: "/x", vars: map[string]string{"no": "VRPx"},
			ops: func(sc *Scope) (any, error) { return sc.GetPreview(context.Background(), "VRPx") }, wantSub: `"code":4001`},
		{name: "读定妆图-总开关关着不回7003", kv: off, fn: func(h *handlers) handlerFn { return h.getPreview }, method: "GET", path: "/x", vars: map[string]string{"no": "VRPx"},
			ops: func(sc *Scope) (any, error) { return sc.GetPreview(context.Background(), "VRPx") }, wantSub: `"code":4001`},
		{name: "读定妆图-方案号过长", fn: func(h *handlers) handlerFn { return h.getPreview }, method: "GET", path: "/x", vars: map[string]string{"no": strings.Repeat("x", 65)},
			ops: func(sc *Scope) (any, error) { return sc.GetPreview(context.Background(), strings.Repeat("x", 65)) }, wantSub: "no 不合法"},
		// 多比例导出清单（§N，只读）：REST 与 Ops 同口径；没导出过 / 成片未交付 → 空数组（别人的任务 → 4001，由内核测试覆盖）；总开关关着照常可读；任务号不合法 → 参数错
		{name: "读导出清单-没有导出回空数组", fn: func(h *handlers) handlerFn { return h.exports }, method: "GET", path: "/x", vars: map[string]string{"id": "42"},
			ops: func(sc *Scope) (any, error) { return sc.Exports(context.Background(), 42) }, wantStatus: 200, wantSub: `"exports":[]`},
		{name: "读导出清单-总开关关着照常可读", kv: off, fn: func(h *handlers) handlerFn { return h.exports }, method: "GET", path: "/x", vars: map[string]string{"id": "42"},
			ops: func(sc *Scope) (any, error) { return sc.Exports(context.Background(), 42) }, wantStatus: 200, wantSub: `"exports":[]`},
		{name: "读导出清单-任务号不合法", fn: func(h *handlers) handlerFn { return h.exports }, method: "GET", path: "/x", vars: map[string]string{"id": "0"},
			ops: func(sc *Scope) (any, error) { return sc.Exports(context.Background(), 0) }, wantSub: "id 不合法"},
		// 写成功才 Kick：停止任务走到内核的 Stop（stub 没实现 → 这里只验证「写入口开着时能走到内核」改由上面重放用例覆盖）
	}
}

func (c pcase) uidOrDefault() int64 {
	switch {
	case c.uid == 0:
		return 7
	case c.uid < 0:
		return 0
	}
	return c.uid
}

func (c pcase) setup() (*pEnv, *handlers) {
	e := newPEnv(cloneKV(c.kv))
	h := &handlers{m: e.m}
	if c.noModule {
		h = &handlers{}
	}
	if c.preTake != "" {
		if _, err := e.m.limiter.take(context.Background(), c.preTake, c.uidOrDefault(), e.m.rt.Rate()); err != nil {
			panic(err)
		}
	}
	return e, h
}

func cloneKV(kv map[string]string) map[string]string {
	if kv == nil {
		return nil
	}
	out := make(map[string]string, len(kv))
	for k, v := range kv {
		out[k] = v
	}
	return out
}

func (c pcase) runREST(t *testing.T) (rendered, int32, bool) {
	e, h := c.setup()
	r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	r = r.WithContext(ctxdata.WithUserID(r.Context(), c.uidOrDefault()))
	if c.vars != nil {
		r = pathvar.WithVars(r, c.vars)
	}
	if c.key != "" {
		r.Header.Set("Idempotency-Key", c.key)
	}
	w := httptest.NewRecorder()
	h.wrap(c.fn(h), c.write)(w, r)
	e.m.taskSync.Wait()
	return render(w), e.kicks.Load(), c.freshTakeOK(e)
}

func (c pcase) runOps(t *testing.T) (rendered, int32, bool) {
	e, h := c.setup()
	sc, err := h.m.Ops().Enter(context.Background(), c.uidOrDefault(), c.write)
	var out rendered
	if err != nil {
		out = renderOps(nil, err)
	} else {
		v, err := c.ops(sc)
		out = renderOps(v, err)
	}
	e.m.taskSync.Wait()
	return out, e.kicks.Load(), c.freshTakeOK(e)
}

// freshTakeOK preTake 的动作在调用之后，额度是否仍是「已占满」（被拒 / 重放都不该再多占或多退）：再 take 一次仍应被拒。
func (c pcase) freshTakeOK(e *pEnv) bool {
	if c.preTake == "" {
		return true
	}
	_, err := e.m.limiter.take(context.Background(), c.preTake, c.uidOrDefault(), e.m.rt.Rate())
	return err != nil
}

func TestRESTAndOpsAgree(t *testing.T) {
	result.Setup()
	for _, c := range pcases() {
		t.Run(c.name, func(t *testing.T) {
			rest, restKicks, restQuota := c.runREST(t)
			ops, opsKicks, opsQuota := c.runOps(t)
			if rest != ops {
				t.Fatalf("REST 与 Ops 响应不一致：\nREST=%+v\nOps =%+v", rest, ops)
			}
			if restKicks != opsKicks || int(restKicks) != c.wantKicks {
				t.Fatalf("任务中心 Kick 次数：REST=%d Ops=%d 期望=%d", restKicks, opsKicks, c.wantKicks)
			}
			if !restQuota || !opsQuota {
				t.Fatalf("限频额度消耗不一致（占满的额度不应被退回 / 被拒请求不占额度）：REST=%v Ops=%v", restQuota, opsQuota)
			}
			if c.wantStatus != 0 && rest.status != c.wantStatus {
				t.Fatalf("状态码=%d 期望=%d：%s", rest.status, c.wantStatus, rest.body)
			}
			if c.wantSub != "" && !strings.Contains(rest.body, c.wantSub) {
				t.Fatalf("响应应含 %q：%s", c.wantSub, rest.body)
			}
			if rest.status == http.StatusTooManyRequests && rest.retry == "" {
				t.Fatal("429 必须带 Retry-After 头")
			}
		})
	}
}

// 失败的写操作退回限频额度（REST 与 Ops 同口径）：每分钟只有 1 次改标注额度，连续三次都因「没装配 LibraryStore」失败，都不应 429。
func TestRESTAndOpsRefundOnFailure(t *testing.T) {
	result.Setup()
	kv := map[string]string{KeyRateAnnotatePerMinute: "1", KeyRateAnnotatePerDay: "100"}
	// REST
	e := newPEnv(cloneKV(kv))
	h := &handlers{m: e.m}
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodPatch, "/x", strings.NewReader(`{"annotations":{"dims":{}}}`))
		r = pathvar.WithVars(r.WithContext(ctxdata.WithUserID(r.Context(), 9)), map[string]string{"no": "VRPx"})
		w := httptest.NewRecorder()
		h.wrap(h.patchAnnotations, true)(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("REST 第 %d 次不应 429", i+1)
		}
	}
	// Ops
	e = newPEnv(cloneKV(kv))
	for i := 0; i < 3; i++ {
		sc, err := e.m.Ops().Enter(context.Background(), 9, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sc.PatchAnnotations(context.Background(), core.PatchReq{PlanNo: "VRPx"}, true)
		var rl *RateLimitError
		if err == nil || asRateLimit(err, &rl) {
			t.Fatalf("Ops 第 %d 次应失败但不应被限频：%v", i+1, err)
		}
	}
}

func asRateLimit(err error, target **RateLimitError) bool {
	rl, ok := err.(*RateLimitError)
	*target = rl
	return ok
}

// nil Module / nil Ops 都是安全的：回 7003，不 panic。
func TestOpsNilSafe(t *testing.T) {
	var m *Module
	if m.Ops() != nil {
		t.Fatal("nil Module 的 Ops 应为 nil")
	}
	if _, err := m.Ops().Enter(context.Background(), 7, false); err == nil || !strings.Contains(err.Error(), "功能暂未开放") {
		t.Fatalf("nil Ops.Enter 应回 7003：%v", err)
	}
	if (&Module{}).Ops() != nil {
		t.Fatal("没装配运行时的 Module 也不给 Ops")
	}
}

func TestPageQueryAndDefaults(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x?page=2&size=5", nil)
	if p, s, err := pageQuery(r); err != nil || p != 2 || s != 5 {
		t.Fatalf("%d %d %v", p, s, err)
	}
	for _, q := range []string{"page=0", "page=abc", "size=-1", "size=x"} {
		if _, _, err := pageQuery(httptest.NewRequest(http.MethodGet, "/x?"+q, nil)); err == nil || !strings.Contains(err.Error(), "不合法") {
			t.Fatalf("%s 应不合法：%v", q, err)
		}
	}
	if p, s, err := pageOf(0, 0); err != nil || p != 1 || s != 20 {
		t.Fatalf("默认 %d %d %v", p, s, err)
	}
	if _, _, err := pageOf(-1, 0); err == nil {
		t.Fatal("负数不合法")
	}
}

// 渠道透传：REST 的 Enter 记 web，智能体入口 EnterAs 记 agent；渠道只进 ctx，不改业务口径。
func TestEnterChannels(t *testing.T) {
	e := newPEnv(map[string]string{})
	sc, err := e.m.Ops().Enter(context.Background(), 9, false)
	if err != nil {
		t.Fatal(err)
	}
	if sc.channel != core.ChannelWeb || core.ChannelOf(sc.ctx(context.Background())) != core.ChannelWeb {
		t.Fatalf("REST 入口应是 web：%q", sc.channel)
	}
	sc, err = e.m.Ops().EnterAs(context.Background(), 9, false, core.ChannelAgent)
	if err != nil {
		t.Fatal(err)
	}
	if sc.channel != core.ChannelAgent || core.ChannelOf(sc.ctx(context.Background())) != core.ChannelAgent {
		t.Fatalf("智能体入口应是 agent：%q", sc.channel)
	}
}
