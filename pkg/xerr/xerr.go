// Package xerr 定义业务错误码。rpc 层用 gRPC status 传递，网关层还原成 {code,msg}。
package xerr

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	OK                 = 0
	ServerError        = 1000
	ParamError         = 1001
	Unauthorized       = 1002
	NotFound           = 1003
	Forbidden          = 1004
	Conflict           = 1005
	NotReady           = 1006
	AgreementReq       = 1007
	InsufficientPoints = 1008 // 积分不足
)

// 业务码起点。小于它的 gRPC code 视为框架错误。
const bizCodeBase = 1000

type CodeError struct {
	Code int
	Msg  string
}

func (e *CodeError) Error() string { return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg) }

// GRPCStatus 让 status.FromError 能直接识别，rpc 返回即可透传到网关。
func (e *CodeError) GRPCStatus() *status.Status {
	return status.New(codes.Code(e.Code), e.Msg)
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
