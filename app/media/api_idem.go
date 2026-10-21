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
	"sync"
	"time"
)

// 写操作的幂等结果缓存（契约 §2）。进程内存，按 路由 + Idempotency-Key 索引：
//   - 已完成且请求体一致：原样回放上次的状态码和响应体（不重复处理）；
//   - 已完成但请求体不同：422，同一个键不能用于不同的请求；
//   - 同一个键正在处理中：503 + Retry-After: 5（瞬时语义），5 秒后用同一键重试；
//   - 只缓存成功（2xx）的结果。错误不缓存，调用方用同一个键重试会重新执行。
//
// 注意：只在本实例内有效；多实例之间不共享（与输入缓存的限制相同）。
type idemStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	done     map[string]idemEntry
	inflight map[string][32]byte
}

type idemEntry struct {
	digest      [32]byte
	status      int
	contentType string
	body        []byte
	at          time.Time
}

// sweepAt 已完成的条目超过这个数量时，写入时顺便清理过期的条目。
const sweepAt = 1024

func newIdemStore(ttl time.Duration) *idemStore {
	return &idemStore{ttl: ttl, now: time.Now, done: map[string]idemEntry{}, inflight: map[string][32]byte{}}
}

type idemState int

const (
	idemFresh idemState = iota
	idemReplay
	idemConflict
	idemBusy
)

// begin 查询并占用一个键。idemFresh 表示占用成功，调用方必须再调用 finish。
func (s *idemStore) begin(key string, digest [32]byte) (idemEntry, idemState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.done[key]; ok && s.now().Sub(e.at) < s.ttl {
		if e.digest != digest {
			return idemEntry{}, idemConflict
		}
		return e, idemReplay
	}
	if _, busy := s.inflight[key]; busy {
		return idemEntry{}, idemBusy
	}
	s.inflight[key] = digest
	return idemEntry{}, idemFresh
}

// finish 释放占用；2xx 结果写入缓存。
func (s *idemStore) finish(key string, digest [32]byte, status int, contentType string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
	if status < 200 || status > 299 {
		return
	}
	now := s.now()
	if len(s.done) >= sweepAt {
		for k, e := range s.done {
			if now.Sub(e.at) >= s.ttl {
				delete(s.done, k)
			}
		}
	}
	s.done[key] = idemEntry{digest: digest, status: status, contentType: contentType, body: body, at: now}
}

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

		entry, state := s.idem.begin(k, digest)
		switch state {
		case idemReplay:
			writeReplay(w, entry)
			return
		case idemConflict:
			writeAPIError(w, invalid("request.idempotency_key_reused"))
			return
		case idemBusy:
			w.Header().Set("Retry-After", "5")
			writeAPIError(w, newAPIError(codeTransient, "request.idempotency_in_flight"))
			return
		}

		// 占用的"处理中"标记必须释放（defer 保证，含 panic）：panic 按 500 失败结束、不缓存，
		// 同一个键之后可以重试，而不是一直返回 503 in_flight。
		finished := false
		defer settleIdem(w, s.idem, k, digest, &finished)

		cw := &captureWriter{header: http.Header{}}
		next(cw, r.WithContext(withIdemKey(r.Context(), key)))
		status := cw.status
		if status == 0 {
			status = http.StatusOK
		}
		contentType := cw.header.Get("Content-Type")
		body := cw.body.Bytes()
		s.idem.finish(k, digest, status, contentType, append([]byte(nil), body...))
		finished = true

		for name, values := range cw.header {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// settleIdem 在 defer 中调用，释放幂等键的"处理中"标记（非 2xx 不缓存）。
// 正常完成（*finished 为 true）时什么也不做；handler panic 时记录栈、写出 500 internal（可重试），不再向上抛。
// 必须直接作为 defer 的函数调用，recover 才能生效。
func settleIdem(w http.ResponseWriter, idem *idemStore, key string, digest [32]byte, finished *bool) {
	p := recover()
	if p == nil && *finished {
		return
	}
	idem.finish(key, digest, http.StatusInternalServerError, "", nil)
	if p != nil {
		log.Printf("media handler panic trace_id=%s key=%q: %v\n%s", traceIDOf(w), key, p, debug.Stack())
		writeAPIError(w, internal(fmt.Errorf("handler panic: %v", p)))
	}
}

func writeReplay(w http.ResponseWriter, e idemEntry) {
	w.Header().Set("Idempotent-Replayed", "true")
	if e.contentType != "" {
		w.Header().Set("Content-Type", e.contentType)
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}
