// Package traceid 跨服务追踪号：读 X-Trace-Id（没有或不合法就生成），放进请求上下文与日志字段，并回写响应头。
// 出站调用（AI 网关、媒体服务）从 ctx 取同一个号带出去，Java → Go → 网关 / 媒体服务的日志可按它串起来。
package traceid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
)

// Header 追踪号请求 / 响应头。
const Header = "X-Trace-Id"

// LogField 日志字段名（避开 go-zero 自带的 trace 字段）。
const LogField = "x_trace_id"

const maxLen = 128

type ctxKey struct{}

// With 把追踪号写进 ctx，同时挂到 logx 字段上（该 ctx 下的 logx.WithContext 日志都带它）。
func With(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	ctx = context.WithValue(ctx, ctxKey{}, id)
	return logx.ContextWithFields(ctx, logx.Field(LogField, id))
}

// From 取 ctx 里的追踪号；没有返回空串。
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Valid 追踪号是否合法：非空、不超长、只含字母数字与 _-.:（防日志注入 / 头注入）。
func Valid(id string) bool {
	if id == "" || len(id) > maxLen {
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

// New 生成一个新的追踪号（32 位十六进制）。
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "trace-unavailable"
	}
	return hex.EncodeToString(b[:])
}

// Middleware go-zero rest 中间件：rest.Server.Use(traceid.Middleware)。
func Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(Header))
		if !Valid(id) {
			id = New()
		}
		w.Header().Set(Header, id)
		next(w, r.WithContext(With(r.Context(), id)))
	}
}

// Inject 出站请求没有显式追踪号头时，从 ctx 补上。
func Inject(ctx context.Context, h http.Header) {
	if h.Get(Header) != "" {
		return
	}
	if id := From(ctx); id != "" {
		h.Set(Header, id)
	}
}
