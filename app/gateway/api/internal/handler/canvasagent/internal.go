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
//     响应带 X-Jsoncase: off，原样输出。
//   - CanvasInternal.Keys 没配（或没有一把合格）时入口不注册，打 WARN。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/svcauth"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// InternalPrefix 服务间入口前缀。
const InternalPrefix = "/internal/v1/canvas"

// maxCallID call_id 上限（审计表 call_id / idem_key 均为 varchar(64)）。
const maxCallID = 64

// clientSvc 审计 client 标记（与 canvas-rpc logic.ClientSvc 一致：只有它的 agent 才认 svc: 前缀）。
const clientSvc = "svc"

// internalMaxBody 原始请求体上限（与 CanvasAgent.MaxBodyBytes 默认值一致；超出的请求在 handler 里拒绝）。
const internalMaxBody = 1 << 20

type rawBodyKey struct{}

type rawBody struct {
	b       []byte
	tooBig  bool
	readErr error
}

// PreserveInternalBody 全局中间件（须在 jsoncase.Middleware 之前 Use）：只对 /internal/v1/canvas/ 生效，
// 读出原始请求体放进 context（验签用），再把同样的字节放回 r.Body。其他路径原样放行。
func PreserveInternalBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, InternalPrefix+"/") {
			next(w, r)
			return
		}
		rb := &rawBody{}
		if r.Body != nil && r.Body != http.NoBody {
			b, err := io.ReadAll(io.LimitReader(r.Body, internalMaxBody+1))
			_ = r.Body.Close()
			rb.b, rb.readErr, rb.tooBig = b, err, len(b) > internalMaxBody
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		next(w, r.WithContext(context.WithValue(r.Context(), rawBodyKey{}, rb)))
	}
}

// RegisterInternal 注册服务间入口；没有可用密钥时不注册（WARN）；非开发模式（Mode 不是 dev / test）又没有会话 Redis 时
// 不注册（ERROR：进程内 nonce 在多实例下防不住重放）。返回是否注册。
func RegisterInternal(server *rest.Server, svcCtx *svc.ServiceContext) bool {
	nonces := nonceStore(svcCtx.Config.Mode, sessionRedis(svcCtx))
	if nonces == nil {
		return false
	}
	v := BuildVerifier(svcCtx.Config.CanvasInternal, nonces)
	if v == nil {
		return false
	}
	server.AddRoutes([]rest.Route{
		{Method: http.MethodPost, Path: "/agent/call", Handler: enabled(svcCtx, InternalCallHandler(svcCtx, v))},
		{Method: http.MethodGet, Path: "/agent/tools", Handler: enabled(svcCtx, InternalToolsHandler(v))},
	}, rest.WithPrefix(InternalPrefix))
	return true
}

// BuildVerifier 由配置建校验器（nil = 不注册入口）。
func BuildVerifier(c config.CanvasInternalConf, nonces svcauth.NonceStore) *svcauth.Verifier {
	keys := make([]svcauth.Key, 0, len(c.Keys))
	for _, k := range c.Keys {
		keys = append(keys, svcauth.Key{KeyID: k.KeyId, Secret: k.Secret})
	}
	v, skipped := svcauth.NewVerifier(keys, time.Duration(c.MaxSkewSeconds)*time.Second, time.Duration(c.NonceTTLSeconds)*time.Second, nonces)
	for _, id := range skipped {
		logx.Errorf("WARN gateway: CanvasInternal.Keys 里的 key %q 不合格（id 须 1–48 位 [A-Za-z0-9_.-]，secret ≥%d 字节），已跳过", id, svcauth.MinSecretLen)
	}
	if v == nil {
		logx.Errorf("WARN gateway: 未配置可用的 CanvasInternal.Keys，服务间画布入口 %s/agent/* 不注册（Java 画布工具将不可用）", InternalPrefix)
	}
	return v
}

func sessionRedis(svcCtx *svc.ServiceContext) redis.UniversalClient {
	if svcCtx.Session != nil {
		if rc := svcCtx.Session.Redis(); rc != nil {
			return rc
		}
	}
	return nil
}

// nonceStore 有 Redis 用 SETNX（多实例共享）；没有时只在开发 / 测试模式（与各服务开反射的判定一致：Mode=dev|test）退回进程内，
// 其余模式返回 nil（入口不注册并 ERROR）。
func nonceStore(mode string, rdb redis.UniversalClient) svcauth.NonceStore {
	if rdb != nil {
		return svcauth.RedisNonces{R: rdb}
	}
	if mode == service.DevMode || mode == service.TestMode {
		logx.Errorf("WARN gateway: 未配置会话 Redis，服务间入口的 nonce 防重放退回进程内（Mode=%s，仅开发 / 测试）", mode)
		return svcauth.NewMemoryNonces()
	}
	logx.Errorf("gateway: 服务间画布入口 %s/agent/* 不注册（check=canvas_internal.nonce_store 判据=Mode=%s 且未配置会话 Redis：进程内 nonce 在多实例下防不住重放）", InternalPrefix, mode)
	return nil
}

// authInternal 验签；失败已写响应并返回 nil。
func authInternal(w http.ResponseWriter, r *http.Request, v *svcauth.Verifier, requireUser bool) (*svcauth.Identity, []byte) {
	rb, _ := r.Context().Value(rawBodyKey{}).(*rawBody)
	if rb == nil {
		// PreserveInternalBody 没挂上：拿不到原始 body 就无法可靠验签，拒绝而不是退回读被改写过的 body
		logx.WithContext(r.Context()).Errorf("gateway: 服务间入口缺少原始请求体（check=canvas_internal.preserve_body：PreserveInternalBody 须在 jsoncase 之前 Use）")
		writeInternalErr(w, r, http.StatusInternalServerError, xerr.ServerError, "SVC_GATEWAY_MISCONFIGURED", "服务配置错误")
		return nil, nil
	}
	if rb.tooBig {
		writeInternalErr(w, r, http.StatusRequestEntityTooLarge, xerr.ParamError, "PARAM_INVALID", "请求体超过 1MB")
		return nil, nil
	}
	if rb.readErr != nil {
		writeInternalErr(w, r, http.StatusBadRequest, xerr.ParamError, "PARAM_INVALID", "请求体读取失败")
		return nil, nil
	}
	id, err := v.Verify(r.Context(), r, rb.b, requireUser)
	if err != nil {
		e, _ := err.(*svcauth.Error)
		reason, msg := svcauth.ReasonBadSignature, err.Error()
		if e != nil {
			reason, msg = e.Reason, e.Msg
		}
		logx.WithContext(r.Context()).Errorf("WARN gateway: 服务间入口拒绝 %s %s key=%q user=%q ip=%s（check=canvas_internal.auth reason=%s）: %s",
			r.Method, r.URL.Path, r.Header.Get(svcauth.HeaderKeyID), r.Header.Get(svcauth.HeaderUserID), httpx.GetRemoteAddr(r), reason, msg)
		if svcauth.IsServerSide(err) {
			writeInternalErr(w, r, http.StatusServiceUnavailable, xerr.ServerError, reason, "鉴权服务暂不可用，请稍后重试")
		} else {
			writeInternalErr(w, r, http.StatusUnauthorized, xerr.Unauthorized, reason, msg)
		}
		return nil, nil
	}
	return id, rb.b
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

// writeInternalErr 服务间入口的协议级错误：HTTP 状态码即重试契约（与 Java 调用方约定，见 canvas-agent-tools.md §12.5）——
// 401 签名被拒（不重试，查配置 / 时钟）、400 / 413 请求本身不对（不重试）、500 / 503 可重试。
// 体与网关统一信封同形，另带 message（= msg）便于 Java 按 {code, message, data} 解析。
func writeInternalErr(w http.ResponseWriter, r *http.Request, status, code int, reason, msg string) {
	w.Header().Set(jsoncase.SkipHeader, "off")
	httpx.WriteJsonCtx(r.Context(), w, status, map[string]any{"code": code, "reason": reason, "msg": msg, "message": msg, "data": nil})
}
