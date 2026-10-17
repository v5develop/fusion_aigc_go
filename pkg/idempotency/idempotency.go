// Package idempotency 处理提交类接口的幂等键。
//
// 口径与 AIGC（Java）一致：幂等键放在请求头 Idempotency-Key（UUID，重试沿用同一个值）。
// X-Request-Id 只用于链路追踪，不参与幂等：
//   - 只带 Idempotency-Key：用它做幂等键。
//   - 只带 X-Request-Id（旧 App 版本）：用它补成幂等键，保证旧版本的重试仍然幂等。
//   - 两个都带：优先 Idempotency-Key，X-Request-Id 原样保留用于链路追踪，不报错。
//
// 业务代码只读 Idempotency-Key，X-Request-Id 从不被中间件改写。
package idempotency

import (
	"net/http"
	"strings"
)

const (
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderRequestID      = "X-Request-Id" // 链路追踪；旧版本的幂等键来源
)

// Resolve 返回幂等键：优先 Idempotency-Key，没有则用 X-Request-Id（旧版本）。两个都没有返回空串。
func Resolve(idemKey, requestID string) string {
	if k := strings.TrimSpace(idemKey); k != "" {
		return k
	}
	return strings.TrimSpace(requestID)
}

// Middleware 确保 Idempotency-Key 有值：缺失时用 X-Request-Id 补上。X-Request-Id 本身不改。
func Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := Resolve(r.Header.Get(HeaderIdempotencyKey), r.Header.Get(HeaderRequestID)); key != "" {
			r.Header.Set(HeaderIdempotencyKey, key)
		}
		next(w, r)
	}
}
