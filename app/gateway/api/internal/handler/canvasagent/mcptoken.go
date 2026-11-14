package canvasagent

// MCP 访问令牌（Go_V1.0.42，设计见 docs/design/canvas-agent-tools.md §13）。
//
//	GET    /api/v1/canvas/mcp-tokens        列出我的令牌（未吊销的，含已过期）
//	POST   /api/v1/canvas/mcp-tokens        新建 {name, expires_in_days: 7|30|90, scope: canvas_rw|canvas_rw_generate}；明文只此一次
//	DELETE /api/v1/canvas/mcp-tokens/:id    吊销（即时生效：删网关校验缓存）
//
//   - 管理接口只认登录令牌：带 mcp_ 令牌一律 2005 · FORBIDDEN_SCOPE（不能用 MCP 令牌管理令牌）。
//   - MCP 入口与 /agent/* 用户入口（agentAuth）：Bearer 以 mcp_ 开头 → 按哈希校验（会话 Redis 缓存 ≤60 秒），
//     解析出 user 与作用域放进 context；否则照旧走登录态 Auth + CanvasBearer（登录令牌仍可用于 MCP）。
//   - MCP 令牌请求**不透传 Bearer** 给 canvas-rpc：它不是 Java 认的令牌，也不能进 canvas-rpc 的用户 Bearer 缓存。
//   - 作用域在 Caller 里拦：生成类工具（canvas_run_node）需要 canvas_rw_generate，否则 is_error FORBIDDEN_SCOPE。

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/clientip"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

// McpPath MCP 入口路径（客户端配置里的 url）。
const McpPath = "/api/v1/canvas/mcp"

// 校验缓存：正缓存 ≤60 秒（且不超过令牌过期时间），负缓存 30 秒（无效 / 已吊销 / 已过期；哈希猜不中，新建的令牌不会被负缓存挡住）。
const (
	tokenCacheTTL    = 60 * time.Second
	tokenNegCacheTTL = 30 * time.Second
	tokenCachePrefix = "canvas:mcp_token:"
)

// reasonForbiddenScope 与 agenttool.ErrForbiddenScope 同名（HTTP 层的 reason）。
const reasonForbiddenScope = "FORBIDDEN_SCOPE"

// TokenResolver 按明文解析 MCP 令牌（哈希后查 canvas-rpc，结果缓存在会话 Redis）。
type TokenResolver struct {
	RPC   canvas_client.Canvas
	Redis redis.UniversalClient // nil = 不缓存（每次查 canvas-rpc）
	Now   func() time.Time
}

type cachedGrant struct {
	TokenID int64  `json:"t,omitempty"`
	UserID  int64  `json:"u,omitempty"`
	Scope   string `json:"s,omitempty"`
	Expires int64  `json:"e,omitempty"`
	Reason  string `json:"x,omitempty"` // 负缓存：失败原因
	Msg     string `json:"m,omitempty"`
}

func (t *TokenResolver) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Resolve 校验明文令牌；失败返回 xerr（Unauthorized 带 reason，或 ServerError）。
func (t *TokenResolver) Resolve(ctx context.Context, raw, ip string) (mcptoken.Grant, error) {
	if !mcptoken.Looks(raw) {
		return mcptoken.Grant{}, xerr.NewReason(xerr.Unauthorized, "MCP_TOKEN_INVALID", "MCP 令牌无效或已吊销")
	}
	hash := mcptoken.Hash(raw)
	key := tokenCachePrefix + hash
	if t.Redis != nil {
		if v, err := t.Redis.Get(ctx, key).Bytes(); err == nil {
			var c cachedGrant
			if json.Unmarshal(v, &c) == nil {
				if c.Reason != "" {
					return mcptoken.Grant{}, xerr.NewReason(xerr.Unauthorized, c.Reason, c.Msg)
				}
				if c.UserID > 0 && c.Expires > t.now().UnixMilli() {
					return mcptoken.Grant{TokenID: c.TokenID, UserID: c.UserID, Scope: c.Scope, ExpiresAt: c.Expires}, nil
				}
			}
		} else if err != redis.Nil {
			logx.WithContext(ctx).Errorf("canvas mcp token: cache get: %v", err) // 缓存抖动：回落查库
		}
	}
	out, err := t.RPC.VerifyMcpToken(ctx, &canvas_client.VerifyMcpTokenReq{TokenHash: hash, ClientIp: ip})
	if err != nil {
		code, msg, ok := xerr.Parse(err)
		if ok && code == xerr.Unauthorized {
			reason := xerr.ReasonOf(err)
			if reason == "" {
				reason = "MCP_TOKEN_INVALID"
			}
			t.cache(ctx, key, cachedGrant{Reason: reason, Msg: msg}, tokenNegCacheTTL)
			return mcptoken.Grant{}, xerr.NewReason(xerr.Unauthorized, reason, msg)
		}
		logx.WithContext(ctx).Errorf("canvas mcp token: verify: %v", err)
		return mcptoken.Grant{}, xerr.New(xerr.ServerError, "令牌校验失败，请稍后重试")
	}
	g := mcptoken.Grant{TokenID: out.TokenId, UserID: out.UserId, Scope: out.Scope, ExpiresAt: out.ExpiresAt}
	ttl := tokenCacheTTL
	if left := time.UnixMilli(out.ExpiresAt).Sub(t.now()); left < ttl {
		ttl = left
	}
	if ttl > 0 && t.Redis != nil {
		// SET NX：吊销墓碑（或并发写入的负缓存）在时不覆盖
		b, _ := json.Marshal(cachedGrant{TokenID: g.TokenID, UserID: g.UserID, Scope: g.Scope, Expires: g.ExpiresAt})
		if err := t.Redis.SetNX(ctx, key, b, ttl).Err(); err != nil {
			logx.WithContext(ctx).Errorf("canvas mcp token: cache set: %v", err)
		}
	}
	return g, nil
}

func (t *TokenResolver) cache(ctx context.Context, key string, c cachedGrant, ttl time.Duration) {
	if t.Redis == nil {
		return
	}
	b, _ := json.Marshal(c)
	if err := t.Redis.Set(ctx, key, b, ttl).Err(); err != nil {
		logx.WithContext(ctx).Errorf("canvas mcp token: cache set: %v", err)
	}
}

// Invalidate 吊销后写墓碑：用负缓存（tokenCacheTTL，与正缓存同长）覆盖校验缓存，而不只是删键——
// 吊销前已发出、吊销后才返回的校验不会把正缓存写回（正缓存用 SET NX，墓碑在就写不进），吊销即时生效。
func (t *TokenResolver) Invalidate(ctx context.Context, hash string) {
	if t.Redis == nil || hash == "" {
		return
	}
	b, _ := json.Marshal(cachedGrant{Reason: "MCP_TOKEN_INVALID", Msg: "MCP 令牌无效或已吊销"})
	if err := t.Redis.Set(ctx, tokenCachePrefix+hash, b, tokenCacheTTL).Err(); err != nil {
		// 写不进墓碑：正缓存最长 tokenCacheTTL 后自然失效
		logx.WithContext(ctx).Errorf("canvas mcp token: 吊销墓碑写入失败（check=mcp_token.invalidate，最长 %s 后自然失效）: %v", tokenCacheTTL, err)
	}
}

func newTokenResolver(svcCtx *svc.ServiceContext) *TokenResolver {
	return &TokenResolver{RPC: svcCtx.CanvasRpc, Redis: sessionRedis(svcCtx)}
}

// bearerToken Authorization 里的令牌（Bearer 前缀可省略，与网关 Auth 同一取法）。
func bearerToken(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// agentAuth MCP 入口与 /agent/* 用户入口的鉴权：mcp_ 令牌走 MCP 令牌校验，其余走登录态（Auth + CanvasBearer）。
func agentAuth(svcCtx *svc.ServiceContext, res *TokenResolver) rest.Middleware {
	return func(next http.HandlerFunc) http.HandlerFunc {
		var login http.HandlerFunc
		if svcCtx.Auth != nil && svcCtx.CanvasBearer != nil {
			login = svcCtx.Auth(svcCtx.CanvasBearer(next))
		}
		return func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if !mcptoken.HasPrefix(raw) {
				if login == nil {
					httpx.ErrorCtx(r.Context(), w, xerr.New(xerr.Unauthorized, "未登录"))
					return
				}
				login(w, r)
				return
			}
			g, err := res.Resolve(r.Context(), raw, clientip.From(r))
			if err != nil {
				if code, _, _ := xerr.Parse(err); code == xerr.Unauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				}
				httpx.ErrorCtx(r.Context(), w, err)
				return
			}
			ctx := ctxdata.WithUserID(mcptoken.WithGrant(r.Context(), g), g.UserID)
			next(w, r.WithContext(ctx))
		}
	}
}

// scopeDenied 作用域不允许该工具时返回原因（空 = 放行）。登录令牌 / 服务间入口没有 Grant，不受限。
func scopeDenied(ctx context.Context, generates bool) string {
	g, ok := mcptoken.GrantFrom(ctx)
	if !ok {
		return ""
	}
	if !mcptoken.AllowsTool(g.Scope, generates) {
		return "该 MCP 令牌只有画布读写权限，不能生成；请在画布页新建带「生成」权限的令牌"
	}
	return ""
}

// toolVisible 工具清单（MCP tools/list、GET /agent/tools）是否列出该工具：MCP 令牌只列作用域内的。
func toolVisible(ctx context.Context, generates bool) bool { return scopeDenied(ctx, generates) == "" }

// authSource 本次调用的鉴权来源与作用域（传给 canvas-rpc：mcp_token 时不借用 Bearer 缓存、二次校验作用域）。
func authSource(ctx context.Context, client string) (src, scope string) {
	if g, ok := mcptoken.GrantFrom(ctx); ok {
		return mcptoken.AuthSourceMcpToken, g.Scope
	}
	if client == clientSvc {
		return mcptoken.AuthSourceSvc, ""
	}
	return mcptoken.AuthSourceUser, ""
}

// rejectMcpToken 管理接口：不能用 MCP 令牌管理令牌。
func rejectMcpToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mcptoken.HasPrefix(bearerToken(r)) {
			httpx.ErrorCtx(r.Context(), w, xerr.NewReason(xerr.Forbidden, reasonForbiddenScope, "MCP 访问令牌不能用来管理令牌，请登录网页端操作"))
			return
		}
		next(w, r)
	}
}

// registerMcpTokens 注册令牌管理接口（登录令牌鉴权）。
func registerMcpTokens(server *rest.Server, svcCtx *svc.ServiceContext, res *TokenResolver) {
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		inner := h
		if svcCtx.Auth != nil {
			inner = svcCtx.Auth(h)
		}
		return enabled(svcCtx, rejectMcpToken(inner))
	}
	server.AddRoutes([]rest.Route{
		{Method: http.MethodGet, Path: "/mcp-tokens", Handler: wrap(ListTokensHandler(svcCtx))},
		{Method: http.MethodPost, Path: "/mcp-tokens", Handler: wrap(CreateTokenHandler(svcCtx))},
		{Method: http.MethodDelete, Path: "/mcp-tokens/:id", Handler: wrap(RevokeTokenHandler(svcCtx, res))},
	}, rest.WithPrefix("/api/v1/canvas"))
}

func tokenView(t *canvas_client.McpTokenInfo) map[string]any {
	return map[string]any{
		"id": strconv.FormatInt(t.Id, 10), "name": t.Name, "token_prefix": t.TokenPrefix, "scope": t.Scope,
		"expires_at": t.ExpiresAt, "last_used_at": t.LastUsedAt, "last_used_ip": t.LastUsedIp,
		"created_at": t.CreatedAt, "expired": t.Expired,
	}
}

// ListTokensHandler GET /api/v1/canvas/mcp-tokens。
func ListTokensHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rejectGrant(w, r) {
			return
		}
		out, err := svcCtx.CanvasRpc.ListMcpTokens(r.Context(), &canvas_client.ListMcpTokensReq{UserId: ctxdata.UserID(r.Context())})
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}
		items := make([]map[string]any, 0, len(out.Tokens))
		for _, t := range out.Tokens {
			items = append(items, tokenView(t))
		}
		httpx.OkJsonCtx(r.Context(), w, map[string]any{
			"tokens": items, "active_count": out.ActiveCount, "max_active": out.MaxActive, "mcp_path": McpPath,
		})
	}
}

// CreateTokenHandler POST /api/v1/canvas/mcp-tokens。
func CreateTokenHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rejectGrant(w, r) {
			return
		}
		var in struct {
			Name          string `json:"name"`
			ExpiresInDays int32  `json:"expires_in_days"`
			Scope         string `json:"scope"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			httpx.ErrorCtx(r.Context(), w, xerr.Param("请求体不是合法 JSON"))
			return
		}
		out, err := svcCtx.CanvasRpc.CreateMcpToken(r.Context(), &canvas_client.CreateMcpTokenReq{
			UserId: ctxdata.UserID(r.Context()), Name: in.Name, ExpiresInDays: in.ExpiresInDays, Scope: in.Scope, ClientIp: clientip.From(r),
		})
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store") // 明文令牌：任何缓存都不许留
		httpx.OkJsonCtx(r.Context(), w, map[string]any{"token": tokenView(out.Token), "plain_token": out.PlainToken, "mcp_path": McpPath})
	}
}

// RevokeTokenHandler DELETE /api/v1/canvas/mcp-tokens/:id。
func RevokeTokenHandler(svcCtx *svc.ServiceContext, res *TokenResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rejectGrant(w, r) {
			return
		}
		id, err := strconv.ParseInt(pathvar.Vars(r)["id"], 10, 64)
		if err != nil || id <= 0 {
			httpx.ErrorCtx(r.Context(), w, xerr.Param("令牌 id 不正确"))
			return
		}
		out, err := svcCtx.CanvasRpc.RevokeMcpToken(r.Context(), &canvas_client.RevokeMcpTokenReq{UserId: ctxdata.UserID(r.Context()), Id: id})
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
			return
		}
		res.Invalidate(r.Context(), out.TokenHash)
		httpx.OkJsonCtx(r.Context(), w, map[string]any{"id": strconv.FormatInt(id, 10), "revoked": true})
	}
}

// rejectGrant 纵深防御：即便路由挂错了中间件、请求带着 MCP 令牌授权进来，管理接口也拒绝。
func rejectGrant(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := mcptoken.GrantFrom(r.Context()); ok {
		httpx.ErrorCtx(r.Context(), w, xerr.NewReason(xerr.Forbidden, reasonForbiddenScope, "MCP 访问令牌不能用来管理令牌，请登录网页端操作"))
		return true
	}
	if ctxdata.UserID(r.Context()) <= 0 {
		httpx.ErrorCtx(r.Context(), w, xerr.New(xerr.Unauthorized, "未登录"))
		return true
	}
	return false
}
