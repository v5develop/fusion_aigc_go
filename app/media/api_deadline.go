package media

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// deadlineFor 从请求体里取可选的 deadline_ms（契约 §2）。没有或非法时返回 0。
func deadlineFor(raw []byte) time.Duration {
	var v struct {
		DeadlineMs int64 `json:"deadline_ms"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.DeadlineMs <= 0 {
		return 0
	}
	return time.Duration(v.DeadlineMs) * time.Millisecond
}

// withDeadline 按请求体里的 deadline_ms 给本次请求设置超时。上限是服务端的 OpTimeoutSeconds，超过则截到上限。
// 超时后处理被取消，调用方收到 503 transient（可用同一个 Idempotency-Key 重试）。
// 请求体读出后会原样放回，后面的 handler 照常读取。
func (s *Server) withDeadline(next http.HandlerFunc) http.HandlerFunc {
	ceiling := s.opCeiling
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.ContentLength == 0 {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil {
			writeAPIError(w, invalid("request.body_read"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if len(raw) > maxRequestBody {
			next(w, r) // 体积超限交给 handler 的 readJSON 拒绝
			return
		}
		d := deadlineFor(raw)
		if d <= 0 {
			next(w, r)
			return
		}
		if ceiling > 0 && d > ceiling {
			d = ceiling
		}
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

// bearerOrToken 从请求里取调用方令牌：Authorization: Bearer <token> 或 X-Media-Token。
func bearerOrToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return r.Header.Get(TokenHeader)
}
