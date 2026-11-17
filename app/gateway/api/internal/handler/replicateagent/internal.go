// Package replicateagent 复刻智能体的服务间内部入口：Java 智能体的 replicate_* 工具执行器经 HMAC 调用，代用户操作「视频做同款」。
//
//	POST /internal/v1/replicate/agent/call    工具执行 {call_id, tool, arguments}
//	GET  /internal/v1/replicate/agent/tools   工具定义（?format=openai|anthropic|mcp）
//
// 与画布内部入口（canvasagent/internal.go）同一套协议：鉴权、请求头、body、响应形状、同一把 CanvasInternal.Keys / 同一个校验器
// （internalauth，由 gateway.go 建一次共用）、X-Jsoncase: off。契约见 fusion-aigc/.scratch/video-replicate-agent/CONTRACT.md §A–§B。
//   - 身份 = X-Svc-User-Id（签名覆盖）；body 必须带 call_id（≤64，签名覆盖），写工具的幂等键就是它（不读未签名的头）。
//   - 工具都落到 app/replicate/httpapi.Ops：总开关、限频、requirePoints、重放短路、写后任务中心同步与 REST 完全同一套口径。
//   - 扣点工具（confirm / continue / redo_segment）expected_points 必填且 >0（不走 core「0 = 不比对」）。
//   - 错误统一为工具错误 {is_error:true, result:{error_code, message, retryable, gaps?}}；复刻模块没装配 / 总开关关着 → FEATURE_NOT_READY。
package replicateagent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	"aigc-local-life/app/canvas/svcauth"
	"aigc-local-life/app/gateway/api/internal/handler/internalauth"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/replicate/agenttool"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// InternalPrefix 服务间复刻入口前缀。
const InternalPrefix = "/internal/v1/replicate"

// maxCallID call_id 上限（与画布入口一致；同时是 core 幂等键 ≤64 的上限）。
const maxCallID = 64

// RegisterInternal 注册服务间复刻入口。v 是与画布入口共用的校验器（internalauth.NewVerifier；nil = 不注册，返回 false）。
// 复刻模块没装配（没配 DataSource）时入口照常注册，工具调用回 FEATURE_NOT_READY。
func RegisterInternal(server *rest.Server, svcCtx *svc.ServiceContext, v *svcauth.Verifier) bool {
	if v == nil {
		return false
	}
	be := OpsBackend(svcCtx.Replicate.Ops())
	server.AddRoutes([]rest.Route{
		{Method: http.MethodPost, Path: "/agent/call", Handler: CallHandler(be, v)},
		{Method: http.MethodGet, Path: "/agent/tools", Handler: ToolsHandler(v)},
	}, rest.WithPrefix(InternalPrefix))
	return true
}

// CallHandler 服务间工具执行。
func CallHandler(be Backend, v *svcauth.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, body := internalauth.Auth(w, r, v, true)
		if id == nil {
			return
		}
		c, err := agenttool.ParseCall(body)
		if err != nil {
			logx.WithContext(r.Context()).Infof("replicate agent: 请求被拒 判据=BODY_INVALID user=%d: %v", id.UserID, err)
			internalauth.WriteErr(w, r, http.StatusBadRequest, xerr.ParamError, agenttool.ErrParam, err.Error())
			return
		}
		// 服务间调用必须在 body 里带 call_id（签名覆盖）：Idempotency-Key / X-Request-Id 头不在签名里，一律忽略
		switch n := utf8.RuneCountInString(c.CallID); {
		case n == 0:
			logx.WithContext(r.Context()).Infof("replicate agent: 请求被拒 判据=CALL_ID_REQUIRED user=%d tool=%s", id.UserID, c.Tool)
			internalauth.WriteErr(w, r, http.StatusBadRequest, xerr.ParamMissing, agenttool.ErrCallIDMissing, "缺少 call_id（服务间调用必须带幂等键）")
			return
		case n > maxCallID:
			logx.WithContext(r.Context()).Infof("replicate agent: 请求被拒 判据=CALL_ID_TOO_LONG user=%d tool=%s 长度=%d 上限=%d", id.UserID, c.Tool, n, maxCallID)
			internalauth.WriteErr(w, r, http.StatusBadRequest, xerr.ParamError, agenttool.ErrParam, fmt.Sprintf("call_id 最长 %d 字符", maxCallID))
			return
		}
		ctx := ctxdata.WithUserID(r.Context(), id.UserID)
		res := Execute(ctx, be, id.UserID, c)
		writeResult(w, r.WithContext(ctx), c, res)
	}
}

// ToolsHandler 服务间工具定义。
func ToolsHandler(v *svcauth.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id, _ := internalauth.Auth(w, r, v, false); id == nil {
			return
		}
		f := agenttool.ParseFormat(r.URL.Query().Get("format"))
		w.Header().Set(jsoncase.SkipHeader, "off") // JSON Schema 的键名是协议的一部分，不补驼峰别名
		httpx.OkJsonCtx(r.Context(), w, map[string]any{"format": string(f), "tools": agenttool.Render(f)})
	}
}

// writeResult 按画布入口同形的出参写回：{name, tool, call_id, is_error, replayed, result, content}（经统一信封包成 data）。
func writeResult(w http.ResponseWriter, r *http.Request, c agenttool.Call, res *agenttool.Result) {
	var result any
	if json.Unmarshal(res.Data, &result) != nil {
		result = map[string]any{}
	}
	w.Header().Set(jsoncase.SkipHeader, "off")
	httpx.OkJsonCtx(r.Context(), w, map[string]any{
		"name": c.Tool, "tool": c.Tool, "call_id": c.CallID, "is_error": res.IsError, "replayed": res.Replayed,
		"result": result, "content": string(res.Data), // content：可直接作为 tool 消息内容回填给模型
	})
}
