package aigateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// 网关 HTTP 层错误码（error.code），不是调用失败原因（那是 Call.Failure）。
const (
	CodeValidationFailed    = "VALIDATION_FAILED"
	CodeAuthFailed          = "AUTH_FAILED"
	CodeForbiddenScope      = "FORBIDDEN_SCOPE"
	CodeNotFound            = "NOT_FOUND"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	CodeRetryNotAllowed     = "RETRY_NOT_ALLOWED"
	// CodeCancelNotAllowed 409：本轮已有尝试发给供应商（且该供应商不能止损），取消被拒，调用状态不变、继续运行。
	CodeCancelNotAllowed  = "CANCEL_NOT_ALLOWED"
	CodeUnknownModelGroup = "UNKNOWN_MODEL_GROUP"
	CodeOperationMismatch = "OPERATION_MISMATCH"
	CodeOptionsInvalid    = "OPTIONS_INVALID"
	CodeBatchTooLarge     = "BATCH_TOO_LARGE"
	CodeRateLimited       = "RATE_LIMITED"
	CodeUnavailable       = "UNAVAILABLE"
	CodeInternal          = "INTERNAL"
	// CodeTransport 本地定义：没拿到 HTTP 响应（连不上、超时、读响应失败）。
	CodeTransport = "TRANSPORT"
)

// CheckCancelVendorSent 取消被拒的网关判据：请求已发给供应商，受理即扣费，取消省不下钱。
const CheckCancelVendorSent = "cancel.vendor_sent"

// 可用 errors.Is 判别的哨兵错误；实际返回的是 *Error。
var (
	ErrNotConfigured = errors.New("aigateway: 未配置 BaseURL")
	// ErrIdempotencyConflict 409：同一幂等键、不同请求内容。调用方逻辑不确定，绝不能重试。
	ErrIdempotencyConflict = errors.New("aigateway: idempotency conflict")
	// ErrRetryNotAllowed 409：retry_failed 用在不可重试的记录上。
	ErrRetryNotAllowed = errors.New("aigateway: retry not allowed")
	// ErrCancelNotAllowed 409 CANCEL_NOT_ALLOWED：请求已发给供应商，不能再取消；调用照常跑完，结果照常交付、按实际用量结算。
	ErrCancelNotAllowed = errors.New("aigateway: cancel not allowed")
	// ErrUnauthorized 401/403：签名、密钥或 scope 问题，属于部署配置错误。
	ErrUnauthorized = errors.New("aigateway: unauthorized")
	// ErrNotFound 404。
	ErrNotFound = errors.New("aigateway: not found")
)

// Error 网关错误响应或网络错误。
//
//   - HTTPStatus 为 0 表示没拿到响应（Code=TRANSPORT，原因在 Err）；
//   - Check 是网关判据名，如 hmac.timestamp_window、retry.unknown_needs_reconciliation；
//   - RetrySafe 表示原样重发同一请求是否有意义（网关 details.retry_safe 优先，否则 429/5xx/网络错误为真）。
//     本包不自动重试，由上层编排决定。
type Error struct {
	HTTPStatus int
	Code       string
	Check      string
	Message    string
	CallID     string
	RetrySafe  bool
	// Err 网络错误的原因
	Err error
}

func (e *Error) Error() string {
	if e.HTTPStatus == 0 {
		return fmt.Sprintf("aigateway: 请求失败: %v", e.Err)
	}
	return fmt.Sprintf("aigateway: HTTP %d %s check=%s: %s", e.HTTPStatus, e.Code, e.Check, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Is 让 errors.Is(err, ErrIdempotencyConflict / ErrRetryNotAllowed / ErrCancelNotAllowed / ErrUnauthorized / ErrNotFound) 生效。
func (e *Error) Is(target error) bool {
	switch target {
	case ErrIdempotencyConflict:
		return e.IsIdempotencyConflict()
	case ErrRetryNotAllowed:
		return e.HTTPStatus == http.StatusConflict && e.Code == CodeRetryNotAllowed
	case ErrCancelNotAllowed:
		return e.IsCancelNotAllowed()
	case ErrUnauthorized:
		return e.IsAuth()
	case ErrNotFound:
		return e.HTTPStatus == http.StatusNotFound
	}
	return false
}

// IsIdempotencyConflict 409 IDEMPOTENCY_CONFLICT。
func (e *Error) IsIdempotencyConflict() bool {
	return e.HTTPStatus == http.StatusConflict && e.Code == CodeIdempotencyConflict
}

// IsCancelNotAllowed 409 CANCEL_NOT_ALLOWED（取消被拒：已发给供应商）。
func (e *Error) IsCancelNotAllowed() bool {
	return e.HTTPStatus == http.StatusConflict && e.Code == CodeCancelNotAllowed
}

// IsCancelNotAllowed err 是否为「取消被拒：请求已发给供应商」（Cancel 返回的 409 CANCEL_NOT_ALLOWED）。
// 被拒时调用状态不变、继续运行：调用方不能当作已取消（不能退款），也不能重试取消。
func IsCancelNotAllowed(err error) bool {
	return errors.Is(err, ErrCancelNotAllowed)
}

// IsAuth 401 AUTH_FAILED 或 403 FORBIDDEN_SCOPE。
func (e *Error) IsAuth() bool {
	return e.HTTPStatus == http.StatusUnauthorized || e.HTTPStatus == http.StatusForbidden
}

// Temporary 网络错误、429 与 5xx 为暂时性错误；其余 4xx 重试无意义。
func (e *Error) Temporary() bool {
	return e.HTTPStatus == 0 || e.HTTPStatus == http.StatusTooManyRequests || e.HTTPStatus >= 500
}

// parseError 解析错误信封 {"error":{code,check,message,call_id,details}}；非 JSON（如 nginx 502 页面）只保留截断的正文。
func parseError(status int, body []byte) *Error {
	e := &Error{HTTPStatus: status}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Check   string         `json:"check"`
			Message string         `json:"message"`
			CallID  string         `json:"call_id"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		e.Code, e.Check, e.Message, e.CallID = env.Error.Code, env.Error.Check, env.Error.Message, env.Error.CallID
		if v, ok := env.Error.Details["retry_safe"].(bool); ok {
			e.RetrySafe = v
			return e
		}
	} else {
		e.Message = truncate(string(body), 300)
	}
	e.RetrySafe = e.Temporary()
	return e
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
