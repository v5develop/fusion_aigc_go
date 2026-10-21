package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// 契约错误码（media.api / 草稿第 7 节）。unauthorized 是契约外的补充，用于鉴权失败。
const (
	codeSourceUnavailable = "source_unavailable"
	codeSourceRejected    = "source_rejected"
	codeSourceTooShort    = "source_too_short"
	codeForbiddenPurpose  = "forbidden_purpose"
	codeTransient         = "transient"
	codeInvalidRequest    = "invalid_request"
	codeUnauthorized      = "unauthorized"
	codeInternal          = "internal"
	codeNotFound          = "not_found"
	codeNoPublicLocation  = "no_public_location"
	codeResourceDeleted   = "resource_deleted"
	codeGraceExpired      = "grace_expired"
	codeConflict          = "conflict"
)

// apiError 契约错误。Message 只能是固定的分类说明，不能含地址、路径、凭证。
type apiError struct {
	Code      string
	Class     string
	Retryable bool
	Check     string
	Message   string
	status    int
	// cause 仅内部错误保留原始错误，由 writeAPIError 带追踪号写日志（不回给调用方）
	cause error
}

func (e *apiError) Error() string { return e.Code + ": " + e.Check }

// newAPIError 按错误码补全 class、retryable、HTTP 状态与说明。check 是排障判据。
//
// 状态：source_* 为 422（永久失败，调用方不应重试）；transient 为 503（同一 Idempotency-Key 可重试）。
func newAPIError(code, check string) *apiError {
	e := &apiError{Code: code, Check: check}
	switch code {
	case codeSourceUnavailable:
		e.Class, e.Message, e.status = "source", "素材地址无法访问", http.StatusUnprocessableEntity
	case codeSourceRejected:
		e.Class, e.Message, e.status = "source", "素材不是可处理的合法媒体", http.StatusUnprocessableEntity
	case codeSourceTooShort:
		e.Class, e.Message, e.status = "source", "参考视频比请求的时长短", http.StatusUnprocessableEntity
	case codeForbiddenPurpose:
		e.Class, e.Message, e.status = "request", "该用途不允许此操作", http.StatusForbidden
	case codeTransient:
		e.Class, e.Retryable, e.Message, e.status = "transient", true, "服务暂时不可用，请用同一 Idempotency-Key 稍后重试", http.StatusServiceUnavailable
	case codeInvalidRequest:
		e.Class, e.Message, e.status = "request", "请求参数不合法", http.StatusBadRequest
	case codeUnauthorized:
		e.Class, e.Message, e.status = "request", "鉴权失败", http.StatusUnauthorized
	case codeNotFound:
		e.Class, e.Message, e.status = "request", "对象不存在", http.StatusNotFound
	case codeNoPublicLocation:
		e.Class, e.Message, e.status = "request", "该资源没有公有地址，请先由调用方发起复制", http.StatusConflict
	case codeResourceDeleted:
		// 410：资源已进入待删除（软删除宽限期内）。永久失败，不可读取；撤销后才能再用。
		e.Class, e.Message, e.status = "request", "资源已进入待删除状态，不可读取", http.StatusGone
	case codeConflict:
		// 409：目标已存在，不能覆盖。永久失败，不可重试（换新的 idem_key 才会得到新目标）。
		e.Class, e.Message, e.status = "request", "目标已存在，不能覆盖", http.StatusConflict
	case codeGraceExpired:
		e.Class, e.Message, e.status = "request", "删除宽限期已过，资源即将被清理，不能撤销", http.StatusConflict
	default:
		e.Code, e.Class, e.Retryable, e.Message, e.status = codeInternal, "internal", true, "服务内部错误", http.StatusInternalServerError
	}
	return e
}

// classifyError 把任意错误映射为契约错误。
//
// 注意：源站 4xx 默认是永久失败（source_unavailable），但 429 / 408 / 403 在 fetch 阶段已转为 transient；
// 源站 5xx、超时、网络错误也是 transient。与 Java 侧"4xx 永久、5xx/超时暂时"的重试契约一致。
func classifyError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, errObjectNotFound):
		return newAPIError(codeSourceUnavailable, "source.object_missing")
	case errors.Is(err, errInvalidObjectKey):
		return newAPIError(codeInvalidRequest, "request.object_key_invalid")
	case errors.Is(err, errUserInputProtected):
		return newAPIError(codeForbiddenPurpose, "purpose.user_input_protected")
	case errors.Is(err, context.DeadlineExceeded):
		return newAPIError(codeTransient, "media.processing_timeout")
	}
	var oe *opError
	if errors.As(err, &oe) {
		switch oe.kind {
		case kindBadInput:
			if oe.upstream > 0 {
				return newAPIError(codeSourceUnavailable, fmt.Sprintf("source.fetch_status=%d", oe.upstream))
			}
			return newAPIError(codeSourceRejected, "media.input_rejected")
		case kindUnavailable:
			return newAPIError(codeTransient, "media.processing_or_fetch_transient")
		case kindLogoUnavailable:
			e := newAPIError(codeInternal, "config.watermark_logo_missing")
			e.Retryable = false
			return e
		}
	}
	e := newAPIError(codeInternal, "internal.unexpected")
	e.cause = err // 原始错误只进日志（writeAPIError 带追踪号写），不回给调用方
	return e
}

// writeAPIError 写出 {"error": {code, class, retryable, check, message}}。
func writeAPIError(w http.ResponseWriter, err error) {
	e := classifyError(err)
	setTraceError(w, e.Code, e.Check)
	if e.cause != nil {
		log.Printf("media internal error trace_id=%s: %v", traceIDOf(w), e.cause)
	}
	if e.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, e.status, map[string]any{"error": map[string]any{
		"code":      e.Code,
		"class":     e.Class,
		"retryable": e.Retryable,
		"check":     e.Check,
		"message":   e.Message,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
