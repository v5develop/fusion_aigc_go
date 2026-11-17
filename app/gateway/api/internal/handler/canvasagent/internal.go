package canvasagent

// 服务间内部入口（P0-7）：Java 智能体的 canvas_* 工具执行器经 HMAC 调用，代用户操作画布。
//
//	POST /internal/v1/canvas/agent/call    Function Call 执行（同 /api/v1/canvas/agent/call 的入参出参）
//	GET  /internal/v1/canvas/agent/tools   工具定义（?format=openai|anthropic|mcp）
//
//   - 鉴权只认服务间 HMAC（app/canvas/svcauth；签名契约见 docs/design/canvas-agent-tools.md §12），不挂 Auth：用户令牌访问不到；
//     网关不透传任何 Bearer（canvas-rpc 取消 Java 工具 run 时用的是它自己缓存的该用户本人 Bearer，见文档 §12.1）。
//   - body 必须带 call_id（≤64，签名覆盖）；Idempotency-Key / X-Request-Id 头不在签名里，忽略。
//   - 身份 = X-Svc-User-Id（签名覆盖），执行完全复用 Caller → canvas-rpc AgentCall：user 隔离、revision、call_id 幂等、限速、审计、计费不变；
//     审计 client=svc、agent=svc:<keyId>，画布事件 / 快照历史里 actor=svc:<keyId>。
//   - 签名覆盖原始请求体：PreserveInternalBody 在 jsoncase 之前把原始 body 存进 context，jsoncase 怎么改写 r.Body 都不影响验签与解析；
//     响应带 X-Jsoncase: off，原样输出。校验器 / nonce 存储 / 原始体保存与复刻智能体入口共用（internalauth）。
//   - CanvasInternal.Keys 没配（或没有一把合格）时入口不注册，打 WARN。

import (
	"fmt"
	"net/http"
	"unicode/utf8"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/svcauth"
	"aigc-local-life/app/gateway/api/internal/handler/internalauth"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// InternalPrefix 服务间画布入口前缀（共用的 /internal/v1/ 原始请求体保存与验签见 internalauth）。
const InternalPrefix = "/internal/v1/canvas"

// maxCallID call_id 上限（审计表 call_id / idem_key 均为 varchar(64)）。
const maxCallID = 64

// clientSvc 审计 client 标记（与 canvas-rpc logic.ClientSvc 一致：只有它的 agent 才认 svc: 前缀）。
const clientSvc = "svc"

// PreserveInternalBody 全局中间件（须在 jsoncase.Middleware 之前 Use）：对 /internal/v1/ 下所有服务间入口生效（实现见 internalauth）。
var PreserveInternalBody = internalauth.PreserveInternalBody

// BuildVerifier 由配置建校验器（nil = 不注册入口）；两个服务间入口共用 internalauth 里的实现。
var BuildVerifier = internalauth.BuildVerifier

// RegisterInternal 注册服务间画布入口。v 是与复刻智能体入口共用的校验器（internalauth.NewVerifier 建；nil = 不注册，返回 false）。
func RegisterInternal(server *rest.Server, svcCtx *svc.ServiceContext, v *svcauth.Verifier) bool {
	if v == nil {
		return false
	}
	server.AddRoutes([]rest.Route{
		{Method: http.MethodPost, Path: "/agent/call", Handler: enabled(svcCtx, InternalCallHandler(svcCtx, v))},
		{Method: http.MethodGet, Path: "/agent/tools", Handler: enabled(svcCtx, InternalToolsHandler(v))},
	}, rest.WithPrefix(InternalPrefix))
	return true
}

func sessionRedis(svcCtx *svc.ServiceContext) redis.UniversalClient {
	return internalauth.SessionRedis(svcCtx)
}

func authInternal(w http.ResponseWriter, r *http.Request, v *svcauth.Verifier, requireUser bool) (*svcauth.Identity, []byte) {
	return internalauth.Auth(w, r, v, requireUser)
}

func writeInternalErr(w http.ResponseWriter, r *http.Request, status, code int, reason, msg string) {
	internalauth.WriteErr(w, r, status, code, reason, msg)
}

// InternalCallHandler 服务间 Function Call 执行。
func InternalCallHandler(svcCtx *svc.ServiceContext, v *svcauth.Verifier) http.HandlerFunc {
	call := Caller(svcCtx)
	return func(w http.ResponseWriter, r *http.Request) {
		id, body := authInternal(w, r, v, true)
		if id == nil {
			return
		}
		c, err := agenttool.ParseFunctionCall(body)
		if err != nil {
			writeInternalErr(w, r, http.StatusBadRequest, xerr.ParamError, "PARAM_INVALID", err.Error())
			return
		}
		// 服务间调用必须在 body 里带 call_id（签名覆盖）：Idempotency-Key / X-Request-Id 头不在签名里，一律忽略
		switch n := utf8.RuneCountInString(c.CallID); {
		case n == 0:
			writeInternalErr(w, r, http.StatusBadRequest, xerr.ParamMissing, "CALL_ID_REQUIRED", "缺少 call_id（服务间调用必须带幂等键）")
			return
		case n > maxCallID:
			writeInternalErr(w, r, http.StatusBadRequest, xerr.ParamError, "PARAM_INVALID", fmt.Sprintf("call_id 最长 %d 字符", maxCallID))
			return
		}
		c.Client, c.Agent = clientSvc, "svc:"+id.KeyID
		r = r.WithContext(ctxdata.WithUserID(r.Context(), id.UserID))
		res, err := call(r.Context(), c)
		if err != nil { // Caller 只在极端情况下返回错误（业务错误都在 is_error 里）
			writeInternalErr(w, r, http.StatusInternalServerError, xerr.ServerError, "SERVER_ERROR", "服务繁忙，请稍后重试")
			return
		}
		writeResult(w, r, c, res)
	}
}

// InternalToolsHandler 服务间工具定义（同 /api/v1/canvas/agent/tools）。
func InternalToolsHandler(v *svcauth.Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id, _ := authInternal(w, r, v, false); id == nil {
			return
		}
		f := agenttool.ParseFormat(r.URL.Query().Get("format"))
		w.Header().Set(jsoncase.SkipHeader, "off")
		httpx.OkJsonCtx(r.Context(), w, map[string]any{"format": string(f), "tools": agenttool.Render(f)})
	}
}
