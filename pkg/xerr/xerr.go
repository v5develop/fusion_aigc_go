// Package xerr 定义业务错误码。码值与 reason 以 Java 的 ErrorCode 为准（Go 是 Java 的延伸，契约以 Java 为主）。
// rpc 层用 gRPC status 传递，网关层还原成 {code,reason,msg}。
package xerr

import (
	"errors"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 与 Java ErrorCode 一一对应。常量名沿用 Go 旧名，调用点不用改。
const (
	OK                 = 0    // SUCCESS
	ServerError        = 5001 // SYSTEM_ERROR
	ParamError         = 1003 // PARAM_INVALID：参数格式无效
	ParamMissing       = 1002 // PARAM_MISSING：缺少必填参数
	Unauthorized       = 2001 // UNAUTHORIZED：未登录或登录已过期（HTTP 401）
	NotFound           = 4001 // RESOURCE_NOT_FOUND
	Forbidden          = 2005 // ACCESS_DENIED
	Conflict           = 4002 // RESOURCE_CONFLICT
	InsufficientPoints = 3003 // INSUFFICIENT_POINTS：全链路唯一的余额不足码
	AccountDisabled    = 2007 // ACCOUNT_DISABLED：账号已被禁用
	ThirdPartyError    = 6001 // THIRD_PARTY_ERROR：第三方（如联盟）服务不可用

	// 以下两个码登记在 Java ErrorCode：AGREEMENT_REQUIRED 放 3xxx（业务规则），FEATURE_NOT_READY 放 7xxx（功能/配置）。
	NotReady     = 7003 // FEATURE_NOT_READY：功能暂未开放
	AgreementReq = 3011 // AGREEMENT_REQUIRED：需先同意用户协议
)

// reasons 是 Java ErrorCode 的常量名。每个码都必须与 Java ErrorCode 一一对应（码值与名字都一致）。
var reasons = map[int]string{
	OK:                 "SUCCESS",
	ServerError:        "SYSTEM_ERROR",
	ParamError:         "PARAM_INVALID",
	ParamMissing:       "PARAM_MISSING",
	Unauthorized:       "UNAUTHORIZED",
	NotFound:           "RESOURCE_NOT_FOUND",
	Forbidden:          "ACCESS_DENIED",
	Conflict:           "RESOURCE_CONFLICT",
	InsufficientPoints: "INSUFFICIENT_POINTS",
	AccountDisabled:    "ACCOUNT_DISABLED",
	ThirdPartyError:    "THIRD_PARTY_ERROR",
	NotReady:           "FEATURE_NOT_READY",
	AgreementReq:       "AGREEMENT_REQUIRED",
}

// 区域蒙版（POST /api/v1/studio/masks）「图片来源不可用」的细分 reason（add-only，码不变）：
// 前端按 reason 区分来源问题与「没识别到区域（RESOURCE_NOT_FOUND）/ 识别太频繁（RESOURCE_CONFLICT）」，不再靠 msg 文案。
const (
	ReasonMaskSourceNotFound = "MASK_SOURCE_NOT_FOUND" // 来源任务 / 画布执行不存在、不属于当前用户或已删除（4001；task_id 路径沿用 1003）
	ReasonMaskSourceNotReady = "MASK_SOURCE_NOT_READY" // 来源画布执行还没有成功的图片（4002）
)

// Reason 返回码对应的 Java ErrorCode 常量名；没有对应的返回空串。
func Reason(code int) string { return reasons[code] }

// 业务码起点。Java 的业务码都 ≥ 1000（0 为成功）；小于它的 gRPC code 视为框架错误。
const bizCodeBase = 1000

type CodeError struct {
	Code int
	Msg  string
	// Reason 细分原因（可空）：同一个码下区分不同场景，只增不改；空则按码取 Reason(code)。
	// 经 gRPC status 的 ErrorInfo detail 透传到网关信封的 reason。
	Reason string
}

func (e *CodeError) Error() string { return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg) }

// GRPCStatus 让 status.FromError 能直接识别，rpc 返回即可透传到网关。
func (e *CodeError) GRPCStatus() *status.Status {
	st := status.New(codes.Code(e.Code), e.Msg)
	if e.Reason != "" {
		if withDetail, err := st.WithDetails(&errdetails.ErrorInfo{Reason: e.Reason, Domain: reasonDomain}); err == nil {
			return withDetail
		}
	}
	return st
}

// reasonDomain 标记 ErrorInfo 是本项目的细分 reason。
const reasonDomain = "fusion-aigc"

// NewReason 同 New，但带细分 reason（码不变）。reason 须是稳定常量名，登记在 api 文档的错误表。
func NewReason(code int, reason, msg string) error {
	return &CodeError{Code: code, Msg: msg, Reason: reason}
}

// ReasonOf 取错误的 reason：优先细分 reason（本地 CodeError 或 gRPC status 的 ErrorInfo），否则按码派生。
// 非业务错误返回空串。
func ReasonOf(err error) string {
	var ce *CodeError
	if errors.As(err, &ce) {
		if ce.Reason != "" {
			return ce.Reason
		}
		return Reason(ce.Code)
	}
	if st, ok := status.FromError(err); ok && int(st.Code()) >= bizCodeBase {
		for _, d := range st.Details() {
			if info, isInfo := d.(*errdetails.ErrorInfo); isInfo && info.Domain == reasonDomain && info.Reason != "" {
				return info.Reason
			}
		}
		return Reason(int(st.Code()))
	}
	return ""
}

// WithReason 给已有业务错误换上细分 reason（码、文案不变）；非业务错误原样返回。
func WithReason(err error, reason string) error {
	if code, msg, ok := Parse(err); ok {
		return NewReason(code, reason, msg)
	}
	return err
}

func New(code int, msg string) error { return &CodeError{Code: code, Msg: msg} }

func Newf(code int, format string, args ...any) error {
	return &CodeError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

func Param(msg string) error      { return New(ParamError, msg) }
func NotFoundf(what string) error { return New(NotFound, what+"不存在") }

// Parse 从任意错误（本地或 rpc 返回）中提取业务码。非业务错误返回 ok=false。
func Parse(err error) (code int, msg string, ok bool) {
	var ce *CodeError
	if errors.As(err, &ce) {
		return ce.Code, ce.Msg, true
	}
	if st, isSt := status.FromError(err); isSt && int(st.Code()) >= bizCodeBase {
		return int(st.Code()), st.Message(), true
	}
	return 0, "", false
}
