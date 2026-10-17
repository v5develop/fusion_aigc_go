// Package result 网关统一响应，与 Java 的 CommonResult 对齐：
//
//	成功：{"code":0,"msg":"ok","data":...}
//	失败：{"code":2001,"reason":"UNAUTHORIZED","msg":"...","data":null}
//
// 失败时带 reason（Java ErrorCode 常量名），客户端按 reason 分支，不按数字。
// HTTP 状态：未登录（2001）为 401，与 Java 一致；其它业务失败保持 200（不按 HTTP 状态码判业务）。
package result

import (
	"context"
	"net/http"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest/httpx"
	"google.golang.org/grpc/status"
)

type Body struct {
	Code   int    `json:"code"`
	Reason string `json:"reason,omitempty"`
	Msg    string `json:"msg"`
	Data   any    `json:"data,omitempty"`
}

// ok 构造成功响应：不带 reason。
func ok(v any) Body {
	return Body{Code: xerr.OK, Msg: "ok", Data: v}
}

// fail 构造失败响应：reason 由码派生；待定的码没有 reason。
func fail(code int, msg string) Body {
	return Body{Code: code, Reason: xerr.Reason(code), Msg: msg}
}

// httpStatus 返回业务码对应的 HTTP 状态：未登录为 401，其余保持 200。
func httpStatus(code int) int {
	if code == xerr.Unauthorized {
		return http.StatusUnauthorized
	}
	return http.StatusOK
}

// Setup 注册全局响应处理，网关 main 中调用一次。
func Setup() {
	httpx.SetOkHandler(func(_ context.Context, v any) any {
		if v == nil {
			v = struct{}{}
		}
		return ok(v)
	})
	httpx.SetErrorHandlerCtx(func(ctx context.Context, err error) (int, any) {
		if code, msg, isBiz := xerr.Parse(err); isBiz {
			return httpStatus(code), fail(code, msg)
		}
		if _, isGrpc := status.FromError(err); isGrpc {
			// 下游服务的非业务错误，不暴露细节
			logx.WithContext(ctx).Errorf("rpc error: %v", err)
			return http.StatusOK, fail(xerr.ServerError, "服务繁忙，请稍后再试")
		}
		// 其余为请求解析 / 校验错误
		return http.StatusOK, fail(xerr.ParamError, "参数错误："+err.Error())
	})
}

func Error(w http.ResponseWriter, r *http.Request, code int, msg string) {
	httpx.WriteJsonCtx(r.Context(), w, httpStatus(code), fail(code, msg))
}
