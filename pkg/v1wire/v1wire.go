// Package v1wire 按 Java CommonResult 的形状写响应：{code, reason?, msg, data, requestId?, timestamp}。
//
// 与 pkg/result（Go 网关默认信封 {code,reason?,msg,data?}）的差异，也是本包存在的原因：
//   - 总带 timestamp（毫秒），requestId 取 X-Request-Id（缺省生成）；
//   - data 为空时输出 null（不省略键）；成功不带 reason，msg 为 "success"；
//   - 不经 jsoncase 改写（jsoncase 对 /api/v1/plaza/ 放行），字段一律 camelCase；
//   - 参数错误（缺必填 PARAM_MISSING / 非法 PARAM_INVALID）HTTP 400，限流 429，未登录 401，其余业务错误 HTTP 200 + 非 0 code。
//
// 设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 1.4、5 节。
package v1wire

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ReasonTooFrequent 限流的细分 reason（xerr.Conflict 下），映射为 HTTP 429。
const ReasonTooFrequent = "TOO_FREQUENT"

// Envelope 响应体。
type Envelope struct {
	Code      int    `json:"code"`
	Reason    string `json:"reason,omitempty"`
	Msg       string `json:"msg"`
	Data      any    `json:"data"`
	RequestID string `json:"requestId,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// RequestID 取 X-Request-Id，缺省生成 16 字节十六进制。
func RequestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Status 业务错误对应的 HTTP 状态。
func Status(code int, reason string) int {
	switch {
	case code == xerr.Unauthorized:
		return http.StatusUnauthorized
	case code == xerr.ParamMissing || code == xerr.ParamError:
		return http.StatusBadRequest
	case reason == ReasonTooFrequent:
		return http.StatusTooManyRequests
	}
	return http.StatusOK
}

// OK 写成功响应。
func OK(w http.ResponseWriter, r *http.Request, data any) {
	write(w, http.StatusOK, Envelope{Code: xerr.OK, Msg: "success", Data: data, RequestID: RequestID(r), Timestamp: time.Now().UnixMilli()})
}

// Fail 写失败响应：业务错误按码与 reason；其余当作系统错误（不暴露细节）。
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	env := Envelope{RequestID: RequestID(r), Timestamp: time.Now().UnixMilli()}
	var status int
	if code, msg, ok := xerr.Parse(err); ok {
		env.Code, env.Msg, env.Reason = code, msg, xerr.ReasonOf(err)
		status = Status(code, env.Reason)
	} else {
		logx.WithContext(r.Context()).Errorf("v1wire: 系统错误 %s %s: %v", r.Method, r.URL.Path, err)
		env.Code, env.Msg, env.Reason = xerr.ServerError, "服务繁忙，请稍后再试", xerr.Reason(xerr.ServerError)
		status = http.StatusOK
	}
	write(w, status, env)
}

// Public 给响应加 Cache-Control: public, max-age=N（目录、报价这类公开读）。
func Public(w http.ResponseWriter, maxAgeSeconds int) {
	w.Header().Set("Cache-Control", "public, max-age="+itoa(maxAgeSeconds))
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func write(w http.ResponseWriter, status int, env Envelope) {
	body, err := json.Marshal(env)
	if err != nil { // data 里有不可序列化的值：按系统错误兜底，别写出半截 JSON
		logx.Errorf("v1wire: 响应序列化失败: %v", err)
		status = http.StatusOK
		body, _ = json.Marshal(Envelope{Code: xerr.ServerError, Reason: xerr.Reason(xerr.ServerError), Msg: "服务繁忙，请稍后再试", Timestamp: env.Timestamp, RequestID: env.RequestID})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
