// Package internalauth 服务间内部入口（/internal/v1/**）的共享鉴权：画布工具入口（canvasagent）与复刻智能体入口（replicateagent）
// 共用同一把校验器（同一份 CanvasInternal.Keys）、同一个 nonce 存储、同一套原始请求体保存与验签错误响应，不各建一份。
//
//   - 鉴权只认服务间 HMAC（app/canvas/svcauth；签名契约见 docs/design/canvas-agent-tools.md §12），不挂 Auth：用户令牌访问不到。
//   - 签名覆盖原始请求体：PreserveInternalBody 在 jsoncase 之前把原始 body 存进 context，jsoncase 怎么改写 r.Body 都不影响验签与解析；
//     响应带 X-Jsoncase: off，原样输出。
//   - CanvasInternal.Keys 没配（或没有一把合格）时入口不注册，打 WARN。
package internalauth

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"aigc-local-life/app/canvas/svcauth"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// Prefix 服务间入口的公共前缀（PreserveInternalBody 对它下面的所有路径生效）。
const Prefix = "/internal/v1/"

// MaxBody 原始请求体上限（与 CanvasAgent.MaxBodyBytes 默认值一致；超出的请求在 handler 里拒绝）。
const MaxBody = 1 << 20

type rawBodyKey struct{}

type rawBody struct {
	b       []byte
	tooBig  bool
	readErr error
}

// PreserveInternalBody 全局中间件（须在 jsoncase.Middleware 之前 Use）：只对 /internal/v1/ 生效，
// 读出原始请求体放进 context（验签用），再把同样的字节放回 r.Body。其他路径原样放行。
func PreserveInternalBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, Prefix) {
			next(w, r)
			return
		}
		rb := &rawBody{}
		if r.Body != nil && r.Body != http.NoBody {
			b, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
			_ = r.Body.Close()
			rb.b, rb.readErr, rb.tooBig = b, err, len(b) > MaxBody
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		next(w, r.WithContext(context.WithValue(r.Context(), rawBodyKey{}, rb)))
	}
}

// NewVerifier 由服务上下文建共享校验器：会话 Redis 做 nonce 防重放；没有可用密钥 / 没有可用 nonce 存储时返回 nil（所有服务间入口都不注册）。
func NewVerifier(svcCtx *svc.ServiceContext) *svcauth.Verifier {
	nonces := NonceStore(svcCtx.Config.Mode, SessionRedis(svcCtx))
	if nonces == nil {
		return nil
	}
	return BuildVerifier(svcCtx.Config.CanvasInternal, nonces)
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
		logx.Errorf("WARN gateway: 未配置可用的 CanvasInternal.Keys，服务间入口 %s** 不注册（Java 画布 / 复刻智能体工具将不可用）", Prefix)
	}
	return v
}

// SessionRedis 会话 Redis（nonce 防重放、限频共用）；没配返回 nil。
func SessionRedis(svcCtx *svc.ServiceContext) redis.UniversalClient {
	if svcCtx.Session != nil {
		if rc := svcCtx.Session.Redis(); rc != nil {
			return rc
		}
	}
	return nil
}

// NonceStore 有 Redis 用 SETNX（多实例共享）；没有时只在开发 / 测试模式（与各服务开反射的判定一致：Mode=dev|test）退回进程内，
// 其余模式返回 nil（入口不注册并 ERROR）。
func NonceStore(mode string, rdb redis.UniversalClient) svcauth.NonceStore {
	if rdb != nil {
		return svcauth.RedisNonces{R: rdb}
	}
	if mode == service.DevMode || mode == service.TestMode {
		logx.Errorf("WARN gateway: 未配置会话 Redis，服务间入口的 nonce 防重放退回进程内（Mode=%s，仅开发 / 测试）", mode)
		return svcauth.NewMemoryNonces()
	}
	logx.Errorf("gateway: 服务间入口 %s** 不注册（check=internal.nonce_store 判据=Mode=%s 且未配置会话 Redis：进程内 nonce 在多实例下防不住重放）", Prefix, mode)
	return nil
}

// Auth 验签；失败已写响应并返回 nil。成功返回身份与原始请求体。
func Auth(w http.ResponseWriter, r *http.Request, v *svcauth.Verifier, requireUser bool) (*svcauth.Identity, []byte) {
	rb, _ := r.Context().Value(rawBodyKey{}).(*rawBody)
	if rb == nil {
		// PreserveInternalBody 没挂上：拿不到原始 body 就无法可靠验签，拒绝而不是退回读被改写过的 body
		logx.WithContext(r.Context()).Errorf("gateway: 服务间入口缺少原始请求体（check=internal.preserve_body：PreserveInternalBody 须在 jsoncase 之前 Use）")
		WriteErr(w, r, http.StatusInternalServerError, xerr.ServerError, "SVC_GATEWAY_MISCONFIGURED", "服务配置错误")
		return nil, nil
	}
	if rb.tooBig {
		WriteErr(w, r, http.StatusRequestEntityTooLarge, xerr.ParamError, "PARAM_INVALID", "请求体超过 1MB")
		return nil, nil
	}
	if rb.readErr != nil {
		WriteErr(w, r, http.StatusBadRequest, xerr.ParamError, "PARAM_INVALID", "请求体读取失败")
		return nil, nil
	}
	id, err := v.Verify(r.Context(), r, rb.b, requireUser)
	if err != nil {
		e, _ := err.(*svcauth.Error)
		reason, msg := svcauth.ReasonBadSignature, err.Error()
		if e != nil {
			reason, msg = e.Reason, e.Msg
		}
		logx.WithContext(r.Context()).Errorf("WARN gateway: 服务间入口拒绝 %s %s key=%q user=%q ip=%s（check=internal.auth reason=%s）: %s",
			r.Method, r.URL.Path, r.Header.Get(svcauth.HeaderKeyID), r.Header.Get(svcauth.HeaderUserID), httpx.GetRemoteAddr(r), reason, msg)
		if svcauth.IsServerSide(err) {
			WriteErr(w, r, http.StatusServiceUnavailable, xerr.ServerError, reason, "鉴权服务暂不可用，请稍后重试")
		} else {
			WriteErr(w, r, http.StatusUnauthorized, xerr.Unauthorized, reason, msg)
		}
		return nil, nil
	}
	return id, rb.b
}

// WriteErr 服务间入口的协议级错误：HTTP 状态码即重试契约（与 Java 调用方约定，见 canvas-agent-tools.md §12.5）——
// 401 签名被拒（不重试，查配置 / 时钟）、400 / 413 请求本身不对（不重试）、500 / 503 可重试。
// 体与网关统一信封同形，另带 message（= msg）便于 Java 按 {code, message, data} 解析。
func WriteErr(w http.ResponseWriter, r *http.Request, status, code int, reason, msg string) {
	w.Header().Set(jsoncase.SkipHeader, "off")
	httpx.WriteJsonCtx(r.Context(), w, status, map[string]any{"code": code, "reason": reason, "msg": msg, "message": msg, "data": nil})
}
