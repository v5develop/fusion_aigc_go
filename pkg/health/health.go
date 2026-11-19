// Package health 存活 / 就绪探针。
//
//	/healthz  只表示进程活着（liveness），不查任何依赖，永远 200 "ok"。
//	/readyz   依赖都能用才 200（readiness）：逐项检查，任何一项失败回 503，并在 JSON 里列出各项结果。
//
// 检查结果按 TTL 缓存（默认 3 秒），探针再密也不会放大对数据库 / Redis / 对象存储的压力。
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Check 一项依赖检查；返回 nil 表示可用。传入的 ctx 带超时。
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Options 就绪检查参数。
type Options struct {
	// Timeout 单次整体检查的超时（默认 2 秒）
	Timeout time.Duration
	// TTL 结果缓存时长（默认 3 秒）
	TTL time.Duration
}

// Ready 就绪检查器。
type Ready struct {
	checks  []Check
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time

	mu      sync.Mutex
	at      time.Time
	results map[string]string
	ok      bool
}

// NewReady 构造；checks 为空时 /readyz 恒 200（没有可查的依赖）。
func NewReady(o Options, checks ...Check) *Ready {
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Second
	}
	if o.TTL <= 0 {
		o.TTL = 3 * time.Second
	}
	return &Ready{checks: checks, timeout: o.Timeout, ttl: o.TTL, now: time.Now}
}

// Run 执行（或复用缓存的）检查，返回各项结果（"ok" 或错误文本）与总体是否就绪。
func (r *Ready) Run(ctx context.Context) (map[string]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results != nil && r.now().Sub(r.at) < r.ttl {
		return r.results, r.ok
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	res := make(map[string]string, len(r.checks))
	var rmu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range r.checks {
		wg.Add(1)
		go func(c Check) {
			defer wg.Done()
			msg := "ok"
			if err := c.Fn(cctx); err != nil {
				msg = err.Error()
			}
			rmu.Lock()
			res[c.Name] = msg
			rmu.Unlock()
		}(c)
	}
	wg.Wait()
	ok := true
	for _, m := range res {
		if m != "ok" {
			ok = false
		}
	}
	r.at, r.results, r.ok = r.now(), res, ok
	return res, ok
}

// Handler /readyz 的处理函数：全部通过 200，否则 503；body 为 {"status":..., "checks":{...}}。
func (r *Ready) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		res, ok := r.Run(req.Context())
		status, code := "ok", http.StatusOK
		if !ok {
			status, code = "unready", http.StatusServiceUnavailable
		}
		names := make([]string, 0, len(res))
		for n := range res {
			names = append(names, n)
		}
		sort.Strings(names)
		ordered := make(map[string]string, len(res))
		for _, n := range names {
			ordered[n] = res[n]
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "checks": ordered})
	}
}

// Healthz /healthz：纯存活探针。
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
