package middleware

import (
	"context"
	"net/http"
	"strings"
)

type bearerKey struct{}

// CanvasBearerMiddleware 把请求的 Authorization 原文放进 context，画布 logic 再经 gRPC metadata（x-user-authorization）
// 透传给 canvas-rpc：canvas-rpc 调 Java 兼容工具（放大 / 扩图 / 重绘）、任务查询与价目接口时代用户调用，
// Java 按该用户鉴权、计费、记任务中心。只透传，不解析（鉴权仍由前面的 Auth 中间件完成）。
type CanvasBearerMiddleware struct{}

func NewCanvasBearerMiddleware() *CanvasBearerMiddleware {
	return &CanvasBearerMiddleware{}
}

func (m *CanvasBearerMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
			r = r.WithContext(context.WithValue(r.Context(), bearerKey{}, v))
		}
		next(w, r)
	}
}

// Bearer 取 CanvasBearer 中间件放进 context 的 Authorization 原文；没有返回空串。
func Bearer(ctx context.Context) string {
	v, _ := ctx.Value(bearerKey{}).(string)
	return v
}
