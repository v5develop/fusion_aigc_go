package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest/pathvar"
)

// 示例列表 / 试看不走限频；改标注、再做一条走独立计数（不占「方案」额度）。
func TestSampleRoutesBypassRateLimit(t *testing.T) {
	result.Setup()
	kv := map[string]string{KeyEnabled: "true", KeyRatePlanPerMinute: "1", KeyRatePlanPerDay: "100",
		KeyRateAnnotatePerMinute: "1", KeyRateAnnotatePerDay: "100"}
	rt := newRuntime(core.DefaultConfig(), false, func(context.Context) (map[string]string, error) { return kv, nil },
		func(cfg core.Config, _ bool) *core.Service { return core.New(cfg, nil, nil) })
	m := &Module{rt: rt, limiter: newRateLimiter(nil, nil)}
	h := &handlers{m: m}
	call := func(fn func(http.ResponseWriter, *http.Request, *core.Service, int64) (any, error), write bool, method, path, body string, vars map[string]string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r = r.WithContext(ctxdata.WithUserID(r.Context(), 7))
		if vars != nil {
			r = pathvar.WithVars(r, vars)
		}
		w := httptest.NewRecorder()
		h.wrap(fn, write)(w, r)
		var env struct {
			Code   int    `json:"code"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w.Code, env.Reason
	}
	rt.Service(context.Background()) // 先读一次运行参数，限频参数才生效
	if p := rt.Rate(); p.AnnotatePerMinute != 1 || DefaultRate().AnnotatePerMinute != 20 || DefaultRate().AnnotatePerDay != 300 {
		t.Fatalf("改标注独立计数参数：%+v", p)
	}
	// 「方案」额度用完不影响改标注（失败退回，所以连续调用也不会 429）
	if _, err := m.limiter.take(context.Background(), ActionPlan, 7, rt.Rate()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if st, _ := call(h.patchAnnotations, true, http.MethodPatch, "/x", `{"annotations":{"dims":{}}}`, map[string]string{"no": "VRPx"}); st == http.StatusTooManyRequests {
			t.Fatal("改标注不占「方案」额度")
		}
	}
	// 用掉本分钟的改标注额度
	if _, err := m.limiter.take(context.Background(), ActionAnnotate, 7, rt.Rate()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if st, _ := call(h.trySample, false, http.MethodPost, "/api/v1/replicate/samples/3/try", "", map[string]string{"id": "3"}); st == http.StatusTooManyRequests {
			t.Fatal("示例试看不应限频")
		}
		if st, _ := call(h.samples, false, http.MethodGet, "/api/v1/replicate/samples?industry=food", "", nil); st == http.StatusTooManyRequests {
			t.Fatal("示例列表不应限频")
		}
	}
	if st, _ := call(h.patchAnnotations, true, http.MethodPatch, "/api/v1/replicate/plans/VRPx/annotations", `{"annotations":{"dims":{}}}`, map[string]string{"no": "VRPx"}); st != http.StatusTooManyRequests {
		t.Fatalf("改标注额度用完应 429：%d", st)
	}
	if st, _ := call(h.reuseSavedPlan, true, http.MethodPost, "/api/v1/replicate/saved-plans/5/reuse", `{}`, map[string]string{"id": "5"}); st != http.StatusTooManyRequests {
		t.Fatalf("再做一条与改标注同一计数，额度用完应 429：%d", st)
	}
	// 缺 annotations：参数错误，不碰限频
	if st, _ := call(h.patchAnnotations, true, http.MethodPatch, "/api/v1/replicate/plans/VRPx/annotations", `{}`, map[string]string{"no": "VRPx"}); st == http.StatusTooManyRequests {
		t.Fatal("参数错误先于限频")
	}
}

// 没装配 LibraryStore：示例 / 我的方案回 7003（不 panic）；失败的改标注退回额度。
func TestLibraryNotReadyRefunds(t *testing.T) {
	result.Setup()
	kv := map[string]string{KeyEnabled: "true", KeyRateAnnotatePerMinute: "1", KeyRateAnnotatePerDay: "100"}
	rt := newRuntime(core.DefaultConfig(), false, func(context.Context) (map[string]string, error) { return kv, nil },
		func(cfg core.Config, _ bool) *core.Service { return core.New(cfg, nil, nil) })
	m := &Module{rt: rt, limiter: newRateLimiter(nil, nil)}
	h := &handlers{m: m}
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodPatch, "/x", strings.NewReader(`{"annotations":{"dims":{}}}`))
		r = pathvar.WithVars(r.WithContext(ctxdata.WithUserID(r.Context(), 9)), map[string]string{"no": "VRPx"})
		w := httptest.NewRecorder()
		h.wrap(h.patchAnnotations, true)(w, r)
		var env struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != xerr.NotReady {
			t.Fatalf("第 %d 次：失败退回额度，不应 429：%d %s", i+1, w.Code, w.Body)
		}
	}
}
