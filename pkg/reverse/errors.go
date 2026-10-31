package reverse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Kind 错误类别，决定上层是否重试。
type Kind int

const (
	// KindInvalid 4xx（400 JSON / 参数、404、413 超限、422 业务校验如「无人物 / 未成年 / 没有可识别的服装图」）
	// 与客户端预检失败。重试无意义。
	KindInvalid Kind = iota + 1
	// KindNotConfigured sidecar 未配置模型调用（502 且文案含「未配置 AI 网关」，兼容旧版「未配置 DeepSeek Key」）。重试无意义，需运维处理。
	KindNotConfigured
	// KindUpstream 其余 5xx（读图返回 429/5xx、连接失败、结果为空等）。可重试。
	KindUpstream
	// KindTransport 没拿到响应（连接失败、单次超时、响应不可解析）。可重试。
	KindTransport
)

func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindNotConfigured:
		return "not_configured"
	case KindUpstream:
		return "upstream"
	case KindTransport:
		return "transport"
	}
	return "unknown"
}

// Error sidecar 调用错误。Message 为 sidecar 的 error 文案（中文，可直接给用户看 422 的那类）。
type Error struct {
	Path    string
	Status  int // 0 = 没拿到响应
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("reverse %s: %s: %s", e.Path, e.Kind, e.Message)
	}
	return fmt.Sprintf("reverse %s: HTTP %d %s: %s", e.Path, e.Status, e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable 5xx（未配 Key 除外）与网络 / 超时可重试。
func (e *Error) Retryable() bool { return e.Kind == KindUpstream || e.Kind == KindTransport }

// IsRetryable err 是否值得重试。调用方取消（context.Canceled）不重试。
func IsRetryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retryable()
}

// IsNotConfigured sidecar 未配置模型调用（ai-gateway 地址或 secret 缺失）。
func IsNotConfigured(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindNotConfigured
}

// IsRejected 4xx / 预检失败：输入本身不可用（如 pose/source 422 无人物、疑似未成年）。
func IsRejected(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindInvalid
}

// notConfiguredMarks sidecar 未配置模型调用时的报错文案（502）。
// 当前 sidecar 经 ai-gateway 调 DeepSeek，文案是「未配置 AI 网关」；旧版直连 DeepSeek 时是「未配置 DeepSeek Key」，一并识别以兼容旧镜像。
var notConfiguredMarks = []string{"未配置 AI 网关", "AI 网关未配置", "未配置 DeepSeek Key"}

func isNotConfiguredMsg(msg string) bool {
	for _, m := range notConfiguredMarks {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

func classify(path string, status int, msg string) *Error {
	e := &Error{Path: path, Status: status, Message: msg}
	switch {
	case status >= 400 && status < 500:
		e.Kind = KindInvalid
	case status == http.StatusBadGateway && isNotConfiguredMsg(msg):
		e.Kind = KindNotConfigured
	default:
		e.Kind = KindUpstream
	}
	return e
}

func transportError(path string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return &Error{Path: path, Kind: KindTransport, Message: err.Error(), Err: err}
}

func invalid(path, msg string) *Error {
	return &Error{Path: path, Kind: KindInvalid, Message: msg}
}
