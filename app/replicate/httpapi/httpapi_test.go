package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"
)

func TestApplyParams(t *testing.T) {
	base := core.DefaultConfig()
	p := applyParams(base, true, map[string]string{
		KeyEnabled: "true", KeyLongformEnabled: "false", KeyAutoRetryMax: "2", KeyRedoMaxPerSegment: "99",
		KeyContinueHours: "48", KeyMaxSegments: "4", KeyOverlapMs: "500", KeyMaxCostCNY: "abc", KeyVisionModel: "vlm.x",
		KeyFontRid: "font-1", KeyPlaceholderText: "稍等，这一段还在重做",
	})
	if !p.Enabled || *p.Longform || p.Cfg.AutoRetryMax != 2 || p.Cfg.ContinueWindow != 48*time.Hour || p.Cfg.VisionModelGroup != "vlm.x" {
		t.Fatalf("合法项应覆盖：%+v", p)
	}
	if p.Cfg.RedoMaxPerSegment != base.RedoMaxPerSegment || p.Cfg.MaxCostCNY != base.MaxCostCNY {
		t.Fatal("越界 / 非法项应忽略")
	}
	if p.Cfg.Limits.MaxSegments != 4 || p.Cfg.Limits.OverlapSec != 0.5 || p.Cfg.FontRid != "font-1" || p.Cfg.PlaceholderText == "" {
		t.Fatalf("分段与字体：%+v", p.Cfg)
	}
	if q := applyParams(base, true, nil); q.Enabled || !*q.Longform {
		t.Fatal("没有任何行：总开关默认关，长视频跟 yaml")
	}
}

func TestRuntimeCachesAndRebuilds(t *testing.T) {
	kv := map[string]string{KeyEnabled: "false"}
	loads, builds := 0, 0
	rt := newRuntime(core.DefaultConfig(), false, func(context.Context) (map[string]string, error) { loads++; return kv, nil },
		func(cfg core.Config, lf bool) *core.Service { builds++; return core.New(cfg, nil, nil) })
	now := time.Unix(1_800_000_000, 0)
	rt.now = func() time.Time { return now }
	s1, on := rt.Service(context.Background())
	if s1 == nil || on {
		t.Fatal("默认关")
	}
	rt.Service(context.Background())
	if loads != 1 {
		t.Fatal("60 秒内走缓存")
	}
	kv = map[string]string{KeyEnabled: "true"}
	now = now.Add(61 * time.Second)
	s2, on := rt.Service(context.Background())
	if !on || s2 != s1 || builds != 1 {
		t.Fatal("只改总开关不重建内核")
	}
	kv = map[string]string{KeyEnabled: "true", KeyAutoRetryMax: "0"}
	now = now.Add(61 * time.Second)
	if s3, _ := rt.Service(context.Background()); s3 == s1 || builds != 2 {
		t.Fatal("参数变了重建内核")
	}
	// 查库失败沿用
	rt.load = func(context.Context) (map[string]string, error) { return nil, errors.New("db down") }
	now = now.Add(61 * time.Second)
	if _, on := rt.Service(context.Background()); !on {
		t.Fatal("查库失败沿用上一次")
	}
}

func TestNotReadyAndGapsEnvelope(t *testing.T) {
	result.Setup()
	h := &handlers{}
	w := httptest.NewRecorder()
	h.wrap(h.limits, false)(w, httptest.NewRequest(http.MethodGet, "/api/v1/replicate/limits", nil))
	var env struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
		Data   struct {
			Gaps []core.Gap `json:"gaps"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != xerr.NotReady {
		t.Fatalf("未开启回 7003：%s", w.Body)
	}
	w = httptest.NewRecorder()
	writeErr(w, httptest.NewRequest(http.MethodPost, "/", nil), &core.GapsError{Gaps: []core.Gap{{Code: "REF_MISSING", Field: "dims.product", Message: "要一张商品图"}}})
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != xerr.ParamError || len(env.Data.Gaps) != 1 || env.Data.Gaps[0].Field != "dims.product" {
		t.Fatalf("缺项：code=1003 + data.gaps：%s", w.Body)
	}
	w = httptest.NewRecorder()
	writeErr(w, httptest.NewRequest(http.MethodPost, "/", nil), xerr.NewReason(xerr.Conflict, "PLAN_STALE", "方案刚更新了"))
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != xerr.Conflict || env.Reason != "PLAN_STALE" {
		t.Fatalf("409 reason 原样：%s", w.Body)
	}
}

func TestIdemKeyHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Request-Id", "old")
	if idemKey(r) != "old" {
		t.Fatal("兼容旧头")
	}
	r.Header.Set("Idempotency-Key", "new")
	if idemKey(r) != "new" {
		t.Fatal("优先 Idempotency-Key")
	}
}
