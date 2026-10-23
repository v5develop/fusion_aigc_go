package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

// 写操作的幂等结果（契约 §2）。存储与语义见 idem_store.go：
//   - 已完成且请求体一致：原样回放上次的状态码、响应头和响应体（不重复处理）；
//   - 已完成但请求体不同：400 invalid_request，同一个键不能用于不同的请求；
//   - 同一个键正在处理中：503 + Retry-After: 5（瞬时语义），5 秒后用同一键重试；
//   - 幂等存储不可用：503 transient（check=idem.store_unavailable），不执行、不降级；
//   - 只缓存成功（2xx）的结果。错误不缓存，调用方用同一个键重试会重新执行。

// captureWriter 记录 handler 的输出，以便结果既能回给调用方，也能缓存。
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

// idemCtxKey 幂等键放进 context，供 storeFile 派生 rid。
type idemCtxKey struct{}

func withIdemKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idemCtxKey{}, key)
}

// idemKeyOf 返回 context 里的幂等键；没有则返回空串。
func idemKeyOf(ctx context.Context) string {
	k, _ := ctx.Value(idemCtxKey{}).(string)
	return k
}

// idempotent 包装写操作：读出请求体（用于比对摘要），再按上面的规则回放、拒绝或执行。
// 没带 Idempotency-Key 时直接交给 handler，由它返回 request.idempotency_key_missing。
func (s *Server) idempotent(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyHeader)
		if key == "" {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil {
			writeAPIError(w, invalid("request.body_read"))
			return
		}
		if len(raw) > maxRequestBody {
			writeAPIError(w, invalid("request.body_too_large"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		digest := sha256.Sum256(raw)
		k := route + "|" + key

		timings := stageTimingsOf(r.Context())
		idemStart := time.Now()
		hold, ok := s.beginIdem(r.Context(), w, k, digest)
		if timings != nil {
			// 只有装了收集器的路由（ingest、resources/copy）记这两个阶段，其它路由的日志不变
			logStage(r.Context(), "idem_begin", idemStart)
		}
		if !ok {
			return
		}

		// 占用的"处理中"标记必须释放（defer 保证，含 panic）：panic 按 500 失败结束、不缓存，
		// 同一个键之后可以重试，而不是一直返回 503 in_flight。
		finished := false
		defer settleIdem(r.Context(), w, s.idem, hold, &finished)

		cw := &captureWriter{header: http.Header{}}
		next(cw, r.WithContext(withIdemKey(r.Context(), key)))
		status := cw.status
		if status == 0 {
			status = http.StatusOK
		}
		body := cw.body.Bytes()
		finishStart := time.Now()
		s.finishIdem(r.Context(), hold, status, cw.header, append([]byte(nil), body...))
		finished = true
		// 幂等记录存的是不带 timings 的响应体（回放不带耗时）；本次实际执行的响应附上各阶段耗时（契约 C1，只增字段）
		if timings != nil {
			logStage(r.Context(), "idem_finish", finishStart)
			if status >= 200 && status < 300 {
				body = appendTimings(body, timings.snapshot())
			}
		}

		for name, values := range cw.header {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// beginIdem 查询并占用幂等键。ok 为 false 时已写出响应（回放、同键异体 400、处理中 503、存储不可用 503），
// 调用方直接返回；ok 为 true 时调用方必须 defer settleIdem 并在完成后 finishIdem。
func (s *Server) beginIdem(ctx context.Context, w http.ResponseWriter, key string, digest [32]byte) (*idemHold, bool) {
	entry, state, hold, err := s.idem.begin(ctx, key, digest)
	if err != nil {
		// 不降级到内存、不放行执行：别的实例可能已经有这个键的结果或正在处理，放行就可能重复调用供应商
		log.Printf("media idempotency store unavailable trace_id=%s check=idem.store_unavailable: %v", traceIDOf(w), err)
		writeAPIError(w, newAPIError(codeTransient, "idem.store_unavailable"))
		return nil, false
	}
	switch state {
	case idemReplay:
		writeReplay(w, entry)
		return nil, false
	case idemConflict:
		writeAPIError(w, invalid("request.idempotency_key_reused"))
		return nil, false
	case idemBusy:
		writeAPIError(w, newAPIError(codeTransient, "request.idempotency_in_flight"))
		return nil, false
	}
	return hold, true
}

// finishIdem 写入结果并释放占用。存储失败只记日志：结果已经算出来了，照常回给调用方；
// 这次没存下的结果在标记租约到期后，同一个键会被重新执行（不会比返回 503 让调用方立刻重试更糟）。
func (s *Server) finishIdem(ctx context.Context, hold *idemHold, status int, header http.Header, body []byte) {
	if err := s.idem.finish(ctx, hold, status, header, body); err != nil {
		log.Printf("media idempotency finish failed check=idem.store_unavailable status=%d: %v", status, err)
	}
}

// settleIdem 在 defer 中调用，释放幂等键的"处理中"标记（非 2xx 不缓存）。
// 正常完成（*finished 为 true）时什么也不做；handler panic 时记录栈、写出 500 internal（可重试），不再向上抛。
// 必须直接作为 defer 的函数调用，recover 才能生效。
func settleIdem(ctx context.Context, w http.ResponseWriter, idem idemStore, hold *idemHold, finished *bool) {
	p := recover()
	if p == nil && *finished {
		return
	}
	if err := idem.finish(ctx, hold, http.StatusInternalServerError, nil, nil); err != nil {
		log.Printf("media idempotency release failed check=idem.store_unavailable: %v", err)
	}
	if p != nil {
		log.Printf("media handler panic trace_id=%s key=%q: %v\n%s", traceIDOf(w), hold.key, p, debug.Stack())
		writeAPIError(w, internal(fmt.Errorf("handler panic: %v", p)))
	}
}

// writeReplay 回放已完成的结果：状态码、handler 写的响应头、响应体。
func writeReplay(w http.ResponseWriter, e idemEntry) {
	for name, values := range e.header {
		w.Header().Del(name)
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}
