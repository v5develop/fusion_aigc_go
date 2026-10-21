package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"
)

// TraceHeader 跨仓追踪号请求头（Java → 媒体服务 → 网关一致）。值是创作任务的 taskId；没有任务时由调用方给 UUID。
const TraceHeader = "X-Trace-Id"

// maxTraceIDLen 追踪号长度上限；超出或含非法字符的头一律视为没有，换成本服务生成的号。
const maxTraceIDLen = 128

type traceCtxKey struct{}

// traceState 挂在请求上下文与响应写入器上：访问日志与错误日志读它。
type traceState struct {
	id       string
	errCode  string
	errCheck string
}

// TraceIDFromContext 返回请求上下文里的追踪号；不在请求内（后台任务）返回空串。
func TraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if st, ok := ctx.Value(traceCtxKey{}).(*traceState); ok {
		return st.id
	}
	return ""
}

// traceWriter 记录状态码，并让 writeAPIError 把错误码写回访问日志。
type traceWriter struct {
	http.ResponseWriter
	state  *traceState
	status int
}

func (w *traceWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *traceWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap 让 http.ResponseController 仍能拿到底层writer（Flush 等）。
func (w *traceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withTrace 读取 X-Trace-Id（没有或不合法则生成），放入请求上下文，回写响应头，
// 并在请求结束时打一行访问日志：trace_id、方法、路径（不含 query）、状态码、耗时毫秒、错误码。
func withTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(TraceHeader))
		if !validTraceID(id) {
			id = newTraceID()
		}
		st := &traceState{id: id}
		w.Header().Set(TraceHeader, id)
		tw := &traceWriter{ResponseWriter: w, state: st}
		start := time.Now()
		next.ServeHTTP(tw, r.WithContext(context.WithValue(r.Context(), traceCtxKey{}, st)))
		status := tw.status
		if status == 0 {
			status = http.StatusOK
		}
		code, check := st.errCode, st.errCheck
		if code == "" {
			code = "-"
		}
		if check == "" {
			check = "-"
		}
		log.Printf("media access trace_id=%s method=%s path=%s status=%d dur_ms=%d code=%s check=%s",
			id, r.Method, r.URL.Path, status, time.Since(start).Milliseconds(), code, check)
	})
}

// traceIDOf 取响应写入器上的追踪号；不是经 withTrace 包装的 writer 返回空串。
func traceIDOf(w http.ResponseWriter) string {
	if tw, ok := w.(*traceWriter); ok {
		return tw.state.id
	}
	return ""
}

// setTraceError 把错误码记到当前请求的访问日志上（不是经 withTrace 的 writer 时无操作）。
func setTraceError(w http.ResponseWriter, code, check string) {
	if tw, ok := w.(*traceWriter); ok {
		tw.state.errCode = code
		tw.state.errCheck = check
	}
}

func validTraceID(id string) bool {
	if id == "" || len(id) > maxTraceIDLen {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "trace-unavailable"
	}
	return hex.EncodeToString(b[:])
}
