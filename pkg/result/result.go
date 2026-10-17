// Package result 网关统一响应：{"code":0,"msg":"ok","data":...}。
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
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

// Setup 注册全局响应处理，网关 main 中调用一次。
func Setup() {
	httpx.SetOkHandler(func(_ context.Context, v any) any {
		if v == nil {
			v = struct{}{}
		}
		return Body{Code: xerr.OK, Msg: "ok", Data: v}
	})
	httpx.SetErrorHandlerCtx(func(ctx context.Context, err error) (int, any) {
		if code, msg, ok := xerr.Parse(err); ok {
			return http.StatusOK, Body{Code: code, Msg: msg}
		}
		if _, isGrpc := status.FromError(err); isGrpc {
			// 下游服务的非业务错误，不暴露细节
			logx.WithContext(ctx).Errorf("rpc error: %v", err)
			return http.StatusOK, Body{Code: xerr.ServerError, Msg: "服务繁忙，请稍后再试"}
		}
		// 其余为请求解析 / 校验错误
		return http.StatusOK, Body{Code: xerr.ParamError, Msg: "参数错误：" + err.Error()}
	})
}

func Error(w http.ResponseWriter, r *http.Request, code int, msg string) {
	httpx.WriteJsonCtx(r.Context(), w, http.StatusOK, Body{Code: code, Msg: msg})
}
