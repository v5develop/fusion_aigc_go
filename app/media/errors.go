package media

import (
	"errors"
	"net/http"
)

// 错误分类直接决定 HTTP 状态，调用方据此判断能不能重试（与 fusion 的重试契约一致）：
//   - bad_input（422）：输入本身有问题（地址不合规、体积超限、ffmpeg 解不开），永久失败，重试没有意义；
//   - unavailable（503）：超时、源站 5xx、并发满了，暂时的，稍后重试；
//   - logo_unavailable（409）：服务端没配水印 logo，属于部署问题，调用方应当跳过水印；
//   - internal（500）：服务自身的问题（缺 ffmpeg、写不了临时目录）。
type opKind int

const (
	kindBadInput opKind = iota
	kindUnavailable
	kindLogoUnavailable
	kindInternal
)

type opError struct {
	kind     opKind
	msg      string
	cause    error
	upstream int // 源站返回的 HTTP 状态（仅 fetch 的 4xx 设置），供 v2 区分素材不可用与暂时失败
	// sourceCheck 非空表示素材地址本身不可用（永久，422 source_unavailable），值是排障判据，如 source.redirect_too_many
	sourceCheck string
	// retryAfter 源站 / 内部限流给出的建议重试间隔（秒，已截到 maxRetryAfterSeconds）；0 表示没有，回落默认 5 秒
	retryAfter int
}

func (e *opError) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *opError) Unwrap() error { return e.cause }

func badInput(msg string) error { return &opError{kind: kindBadInput, msg: msg} }

func unavailable(msg string) error { return &opError{kind: kindUnavailable, msg: msg} }

func internal(cause error) error {
	return &opError{kind: kindInternal, msg: "服务内部错误", cause: cause}
}

func logoUnavailable() error {
	return &opError{kind: kindLogoUnavailable, msg: "服务端未配置水印 logo"}
}

// statusOf 把错误映射成 HTTP 状态与机器可读的 code。
func statusOf(err error) (int, string) {
	var oe *opError
	if !errors.As(err, &oe) {
		return http.StatusInternalServerError, "internal"
	}
	switch oe.kind {
	case kindBadInput:
		return http.StatusUnprocessableEntity, "bad_input"
	case kindUnavailable:
		return http.StatusServiceUnavailable, "unavailable"
	case kindLogoUnavailable:
		return http.StatusConflict, "logo_unavailable"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// sourceUnavailable 素材地址本身不可用（永久失败，422 source_unavailable），check 是排障判据。
func sourceUnavailable(check, msg string) error {
	return &opError{kind: kindBadInput, msg: msg, sourceCheck: check}
}

// unavailableRetryAfter 暂时失败，并带上源站给出的重试间隔（秒）。
func unavailableRetryAfter(msg string, retryAfter int) error {
	return &opError{kind: kindUnavailable, msg: msg, retryAfter: retryAfter}
}

// upstreamBadInput 源站 4xx：永久失败，同时记下源站状态码。
func upstreamBadInput(status int, msg string) error {
	return &opError{kind: kindBadInput, msg: msg, upstream: status}
}
