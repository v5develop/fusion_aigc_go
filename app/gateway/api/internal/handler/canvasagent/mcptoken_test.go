package canvasagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/middleware"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/rest/pathvar"
	"google.golang.org/grpc"
)

// tokenRPC 假 canvas-rpc：内存里的令牌表（按哈希），模拟 Verify / Revoke / List / Create。
type tokenRPC struct {
	fakeRPC
	tokens   map[string]*fakeToken // hash → token
	verifies int
	created  []*canvas_client.CreateMcpTokenReq
	revoked  []*canvas_client.RevokeMcpTokenReq
}

type fakeToken struct {
	id, uid int64
	scope   string
	exp     time.Time
	revoked bool
}

func newTokenRPC() *tokenRPC { return &tokenRPC{tokens: map[string]*fakeToken{}} }

func (f *tokenRPC) add(uid int64, scope string, ttl time.Duration) (plain string, tk *fakeToken) {
	plain, hash, _, _ := mcptoken.Generate()
	tk = &fakeToken{id: int64(len(f.tokens) + 1), uid: uid, scope: scope, exp: time.Now().Add(ttl)}
	f.tokens[hash] = tk
	return plain, tk
}

func (f *tokenRPC) VerifyMcpToken(_ context.Context, in *canvas_client.VerifyMcpTokenReq, _ ...grpc.CallOption) (*canvas_client.VerifyMcpTokenResp, error) {
	f.verifies++
	tk := f.tokens[in.TokenHash]
	switch {
	case tk == nil || tk.revoked:
		return nil, xerr.NewReason(xerr.Unauthorized, "MCP_TOKEN_INVALID", "MCP 令牌无效或已吊销")
	case !tk.exp.After(time.Now()):
		return nil, xerr.NewReason(xerr.Unauthorized, "MCP_TOKEN_EXPIRED", "MCP 令牌已过期")
	}
	return &canvas_client.VerifyMcpTokenResp{TokenId: tk.id, UserId: tk.uid, Scope: tk.scope, ExpiresAt: tk.exp.UnixMilli()}, nil
}

func (f *tokenRPC) RevokeMcpToken(_ context.Context, in *canvas_client.RevokeMcpTokenReq, _ ...grpc.CallOption) (*canvas_client.RevokeMcpTokenResp, error) {
	f.revoked = append(f.revoked, in)
	for h, tk := range f.tokens {
		if tk.id == in.Id && tk.uid == in.UserId {
			tk.revoked = true
			return &canvas_client.RevokeMcpTokenResp{TokenHash: h}, nil
		}
	}
	return nil, xerr.NotFoundf("令牌")
}

func (f *tokenRPC) ListMcpTokens(_ context.Context, in *canvas_client.ListMcpTokensReq, _ ...grpc.CallOption) (*canvas_client.ListMcpTokensResp, error) {
	return &canvas_client.ListMcpTokensResp{MaxActive: 10}, nil
}

func (f *tokenRPC) CreateMcpToken(_ context.Context, in *canvas_client.CreateMcpTokenReq, _ ...grpc.CallOption) (*canvas_client.CreateMcpTokenResp, error) {
	f.created = append(f.created, in)
	return &canvas_client.CreateMcpTokenResp{Token: &canvas_client.McpTokenInfo{Id: 9, Name: in.Name, Scope: in.Scope}, PlainToken: "mcp_x"}, nil
}

// fakeLoginAuth 模拟登录态 Auth：只认 "login-<uid>"。
func fakeLoginAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "login-7" {
			next(w, r.WithContext(ctxdata.WithUserID(r.Context(), 7)))
			return
		}
		result.Error(w, r, xerr.Unauthorized, "未登录")
	}
}

func tokenSvc(t *testing.T, rpc *tokenRPC) (*svc.ServiceContext, *TokenResolver, *miniredis.Miniredis) {
	t.Helper()
	result.Setup()
	mr := miniredis.RunT(t)
	s := testSvc(&rpc.fakeRPC, 100, 100)
	s.CanvasRpc = rpc
	s.Auth = fakeLoginAuth
	s.CanvasBearer = middleware.NewCanvasBearerMiddleware().Handle
	res := &TokenResolver{RPC: rpc, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}
	return s, res, mr
}

// echo 记录 handler 看到的身份 / 授权 / 透传的 Bearer。
type seen struct {
	uid    int64
	grant  mcptoken.Grant
	has    bool
	bearer string
}

func echo(out *seen) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out.uid = ctxdata.UserID(r.Context())
		out.grant, out.has = mcptoken.GrantFrom(r.Context())
		out.bearer = middleware.Bearer(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}
}

func authed(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, McpPath, strings.NewReader("{}"))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAgentAuthMcpTokenAndLoginToken(t *testing.T) {
	rpc := newTokenRPC()
	s, res, _ := tokenSvc(t, rpc)
	plain, tk := rpc.add(42, mcptoken.ScopeRW, time.Hour)

	var got seen
	h := agentAuth(s, res)(echo(&got))
	rec := httptest.NewRecorder()
	h(rec, authed(plain))
	if rec.Code != http.StatusNoContent || got.uid != 42 || !got.has || got.grant.Scope != mcptoken.ScopeRW || got.grant.TokenID != tk.id {
		t.Fatalf("MCP 令牌应解析出用户与作用域: %d %+v %s", rec.Code, got, rec.Body.String())
	}
	if got.bearer != "" {
		t.Fatalf("MCP 令牌不应透传给 canvas-rpc: %q", got.bearer)
	}

	// 登录令牌照旧可用，且透传 Bearer、没有 Grant（不受作用域限制）
	got = seen{}
	rec = httptest.NewRecorder()
	h(rec, authed("login-7"))
	if rec.Code != http.StatusNoContent || got.uid != 7 || got.has || got.bearer != "Bearer login-7" {
		t.Fatalf("登录令牌: %d %+v", rec.Code, got)
	}

	// 无效 / 形状不对 / 无凭证 → 401
	for _, bad := range []string{"mcp_" + strings.Repeat("A", 43), "mcp_short", ""} {
		rec = httptest.NewRecorder()
		h(rec, authed(bad))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%q 应 401: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	if rec := httptest.NewRecorder(); true {
		h(rec, authed("mcp_"+strings.Repeat("A", 43)))
		if rec.Header().Get("WWW-Authenticate") == "" || !strings.Contains(rec.Body.String(), "MCP_TOKEN_INVALID") {
			t.Fatalf("无效 MCP 令牌应带 WWW-Authenticate 与 reason: %v %s", rec.Header(), rec.Body.String())
		}
	}
}

func TestAgentAuthExpiredAndCache(t *testing.T) {
	rpc := newTokenRPC()
	s, res, mr := tokenSvc(t, rpc)
	h := agentAuth(s, res)(echo(&seen{}))

	expired, _ := rpc.add(42, mcptoken.ScopeRW, -time.Second)
	rec := httptest.NewRecorder()
	h(rec, authed(expired))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "MCP_TOKEN_EXPIRED") {
		t.Fatalf("过期应 401 MCP_TOKEN_EXPIRED: %d %s", rec.Code, rec.Body.String())
	}

	plain, _ := rpc.add(42, mcptoken.ScopeRW, time.Hour)
	rpc.verifies = 0
	for i := 0; i < 3; i++ {
		rec = httptest.NewRecorder()
		h(rec, authed(plain))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("第 %d 次: %d", i, rec.Code)
		}
	}
	if rpc.verifies != 1 {
		t.Fatalf("缓存命中后不应再查 canvas-rpc: %d", rpc.verifies)
	}
	if ttl := mr.TTL(tokenCachePrefix + mcptoken.Hash(plain)); ttl <= 0 || ttl > tokenCacheTTL {
		t.Fatalf("缓存 TTL 不对: %s", ttl)
	}
	// 缓存过期后重新校验（记最后使用）
	mr.FastForward(tokenCacheTTL + time.Second)
	h(httptest.NewRecorder(), authed(plain))
	if rpc.verifies != 2 {
		t.Fatalf("缓存过期后应重新校验: %d", rpc.verifies)
	}

	// 快过期的令牌：缓存 TTL 不超过剩余有效期
	soon, _ := rpc.add(42, mcptoken.ScopeRW, 5*time.Second)
	h(httptest.NewRecorder(), authed(soon))
	if ttl := mr.TTL(tokenCachePrefix + mcptoken.Hash(soon)); ttl <= 0 || ttl > 5*time.Second {
		t.Fatalf("缓存不应活过令牌: %s", ttl)
	}
}

func TestRevokeInvalidatesCacheImmediately(t *testing.T) {
	rpc := newTokenRPC()
	s, res, _ := tokenSvc(t, rpc)
	h := agentAuth(s, res)(echo(&seen{}))
	plain, tk := rpc.add(7, mcptoken.ScopeRWGenerate, time.Hour)
	h(httptest.NewRecorder(), authed(plain)) // 进缓存

	revoke := enabled(s, rejectMcpToken(s.Auth(RevokeTokenHandler(s, res))))
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/canvas/mcp-tokens/1", nil)
	r.Header.Set("Authorization", "Bearer login-7")
	r = pathvar.WithVars(r, map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	revoke(rec, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":0`) || !tk.revoked {
		t.Fatalf("吊销: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h(rec, authed(plain))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("吊销后应立即失效（缓存已删）: %d", rec.Code)
	}
}

func TestMcpTokenCannotManageTokens(t *testing.T) {
	rpc := newTokenRPC()
	s, res, _ := tokenSvc(t, rpc)
	plain, _ := rpc.add(7, mcptoken.ScopeRWGenerate, time.Hour)
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return enabled(s, rejectMcpToken(s.Auth(h))) }
	cases := []struct {
		name string
		h    http.HandlerFunc
		r    *http.Request
	}{
		{"list", wrap(ListTokensHandler(s)), httptest.NewRequest(http.MethodGet, "/api/v1/canvas/mcp-tokens", nil)},
		{"create", wrap(CreateTokenHandler(s)), httptest.NewRequest(http.MethodPost, "/api/v1/canvas/mcp-tokens", strings.NewReader(`{"name":"x","expires_in_days":7,"scope":"canvas_rw"}`))},
		{"revoke", wrap(RevokeTokenHandler(s, res)), pathvar.WithVars(httptest.NewRequest(http.MethodDelete, "/api/v1/canvas/mcp-tokens/1", nil), map[string]string{"id": "1"})},
	}
	for _, c := range cases {
		c.r.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		c.h(rec, c.r)
		if !strings.Contains(rec.Body.String(), reasonForbiddenScope) {
			t.Fatalf("%s: MCP 令牌管理令牌应被拒: %s", c.name, rec.Body.String())
		}
	}
	if len(rpc.created) != 0 || len(rpc.revoked) != 0 {
		t.Fatal("被拒的请求不应到达 canvas-rpc")
	}
	// 纵深防御：handler 自己也拒带 Grant 的请求
	r := httptest.NewRequest(http.MethodGet, "/api/v1/canvas/mcp-tokens", nil)
	r = r.WithContext(mcptoken.WithGrant(ctxdata.WithUserID(r.Context(), 7), mcptoken.Grant{UserID: 7, Scope: mcptoken.ScopeRWGenerate}))
	rec := httptest.NewRecorder()
	ListTokensHandler(s)(rec, r)
	if !strings.Contains(rec.Body.String(), reasonForbiddenScope) {
		t.Fatalf("带 Grant 的请求应被拒: %s", rec.Body.String())
	}

	// 登录令牌可以建：身份取登录态，参数原样交给 canvas-rpc；响应 no-store
	r = httptest.NewRequest(http.MethodPost, "/api/v1/canvas/mcp-tokens", strings.NewReader(`{"name":"Cursor","expires_in_days":30,"scope":"canvas_rw_generate"}`))
	r.Header.Set("Authorization", "Bearer login-7")
	rec = httptest.NewRecorder()
	wrap(CreateTokenHandler(s))(rec, r)
	if len(rpc.created) != 1 || rpc.created[0].UserId != 7 || rpc.created[0].ExpiresInDays != 30 || rpc.created[0].Scope != mcptoken.ScopeRWGenerate {
		t.Fatalf("创建透传不对: %s %+v", rec.Body.String(), rpc.created)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"plain_token":"mcp_x"`) || !strings.Contains(rec.Body.String(), McpPath) {
		t.Fatalf("创建响应: %v %s", rec.Header(), rec.Body.String())
	}
}

func TestRevokeOthersTokenNotFound(t *testing.T) {
	rpc := newTokenRPC()
	s, res, _ := tokenSvc(t, rpc)
	plain, tk := rpc.add(99, mcptoken.ScopeRW, time.Hour) // 别人的
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/canvas/mcp-tokens/1", nil)
	r.Header.Set("Authorization", "Bearer login-7")
	r = pathvar.WithVars(r, map[string]string{"id": "1"})
	rec := httptest.NewRecorder()
	enabled(s, rejectMcpToken(s.Auth(RevokeTokenHandler(s, res))))(rec, r)
	if !strings.Contains(rec.Body.String(), `"code":4001`) || tk.revoked {
		t.Fatalf("吊销他人令牌应 NotFound 且不生效: %s", rec.Body.String())
	}
	var got seen
	rec = httptest.NewRecorder()
	agentAuth(s, res)(echo(&got))(rec, authed(plain))
	if got.uid != 99 {
		t.Fatal("他人令牌应仍可用（且只代表其本人）")
	}
}

func TestCallerScope(t *testing.T) {
	rpc := newTokenRPC()
	s, _, _ := tokenSvc(t, rpc)
	call := Caller(s)
	base := ctxdata.WithUserID(context.Background(), 42)
	rw := mcptoken.WithGrant(base, mcptoken.Grant{UserID: 42, Scope: mcptoken.ScopeRW})
	gen := mcptoken.WithGrant(base, mcptoken.Grant{UserID: 42, Scope: mcptoken.ScopeRWGenerate})

	if r, _ := call(rw, agenttool.Call{Tool: agenttool.ToolRunNode}); !r.IsError || !strings.Contains(string(r.Data), agenttool.ErrForbiddenScope) {
		t.Fatalf("读写令牌调生成应 FORBIDDEN_SCOPE: %s", r.Data)
	}
	if len(rpc.got) != 0 {
		t.Fatal("越权调用不应到达 canvas-rpc")
	}
	for _, tool := range []string{agenttool.ToolGet, agenttool.ToolAddNodes, agenttool.ToolCancelRun, agenttool.ToolGetRun} {
		if r, _ := call(rw, agenttool.Call{Tool: tool}); r.IsError {
			t.Fatalf("读写令牌调 %s 应放行: %s", tool, r.Data)
		}
	}
	if r, _ := call(gen, agenttool.Call{Tool: agenttool.ToolRunNode}); r.IsError {
		t.Fatalf("生成令牌调生成应放行: %s", r.Data)
	}
	if r, _ := call(base, agenttool.Call{Tool: agenttool.ToolRunNode}); r.IsError {
		t.Fatalf("登录令牌不受作用域限制: %s", r.Data)
	}
}

// MCP 协议层：读写令牌经 MCP tools/call 调生成 → 结果 isError + FORBIDDEN_SCOPE（不是协议错误）。
func TestMcpEndToEndScope(t *testing.T) {
	rpc := newTokenRPC()
	s, res, _ := tokenSvc(t, rpc)
	plain, _ := rpc.add(42, mcptoken.ScopeRW, time.Hour)
	mcp := &agenttool.MCPServer{Name: "t", Version: "1", Call: Caller(s)}
	h := agentAuth(s, res)(enabled(s, mcp.ServeHTTP))
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canvas_run_node","arguments":{"canvas_id":"cv_0123456789abcdef","node_id":"m_1"}}}`
	r := httptest.NewRequest(http.MethodPost, McpPath, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	h(rec, r)
	if !strings.Contains(rec.Body.String(), `"isError":true`) || !strings.Contains(rec.Body.String(), "FORBIDDEN_SCOPE") {
		t.Fatalf("MCP 越权应返回 isError FORBIDDEN_SCOPE: %s", rec.Body.String())
	}
}

// 低：吊销写墓碑——吊销前发出、吊销后才返回的校验不能把正缓存写回。
func TestRevokeTombstoneBlocksStalePositiveCache(t *testing.T) {
	rpc := newTokenRPC()
	_, res, mr := tokenSvc(t, rpc)
	plain, tk := rpc.add(7, mcptoken.ScopeRW, time.Hour)
	hash := mcptoken.Hash(plain)
	res.Invalidate(context.Background(), hash) // 吊销：写墓碑
	tk.revoked = false                         // 模拟「吊销前就已在途的校验」拿到旧结果
	if _, err := res.Resolve(context.Background(), plain, ""); err == nil {
		t.Fatal("墓碑在时应直接拒绝（不查 canvas-rpc）")
	}
	if ttl := mr.TTL(tokenCachePrefix + hash); ttl <= 0 || ttl > tokenCacheTTL {
		t.Fatalf("墓碑 TTL 不对: %s", ttl)
	}
	// 在途校验返回后写正缓存（SET NX）也写不进墓碑
	mr.Del(tokenCachePrefix + hash)
	res.Invalidate(context.Background(), hash)
	rpcVerifies := rpc.verifies
	if ok, _ := res.Redis.SetNX(context.Background(), tokenCachePrefix+hash, "x", time.Minute).Result(); ok {
		t.Fatal("墓碑在时 SET NX 不应成功")
	}
	if rpc.verifies != rpcVerifies {
		t.Fatal("不应查 canvas-rpc")
	}
}

// 低：canvas_rw 令牌的 tools/list 与 GET /agent/tools 不列生成类工具；生成令牌 / 登录令牌列全部。
func TestToolListFilteredByScope(t *testing.T) {
	rpc := newTokenRPC()
	s, res, _ := tokenSvc(t, rpc)
	rw, _ := rpc.add(42, mcptoken.ScopeRW, time.Hour)
	gen, _ := rpc.add(42, mcptoken.ScopeRWGenerate, time.Hour)
	mcp := &agenttool.MCPServer{Name: "t", Version: "1", Call: Caller(s),
		ToolFilter: func(r *http.Request, tl agenttool.Tool) bool { return toolVisible(r.Context(), tl.Generates) }}
	h := agentAuth(s, res)(enabled(s, mcp.ServeHTTP))
	list := func(token string) string {
		r := httptest.NewRequest(http.MethodPost, McpPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Body.String()
	}
	named := func(n string) string { return `"name":"` + n + `"` }
	if b := list(rw); strings.Contains(b, named(agenttool.ToolRunNode)) || !strings.Contains(b, named(agenttool.ToolGet)) {
		t.Fatalf("canvas_rw 不应列生成类工具: %.200s", b)
	}
	for _, tok := range []string{gen, "login-7"} {
		if b := list(tok); !strings.Contains(b, named(agenttool.ToolRunNode)) {
			t.Fatalf("%s 应列全部工具", tok)
		}
	}
	tools := agentAuth(s, res)(enabled(s, ToolsHandler))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/canvas/agent/tools?format=openai", nil)
	r.Header.Set("Authorization", "Bearer "+rw)
	rec := httptest.NewRecorder()
	tools(rec, r)
	if strings.Contains(rec.Body.String(), named(agenttool.ToolRunNode)) || !strings.Contains(rec.Body.String(), named(agenttool.ToolGet)) {
		t.Fatalf("GET /agent/tools 也应按作用域过滤: %.200s", rec.Body.String())
	}
}

// 中1 / 低：鉴权来源与作用域下传 canvas-rpc。
func TestCallerPassesAuthSource(t *testing.T) {
	rpc := newTokenRPC()
	s, _, _ := tokenSvc(t, rpc)
	call := Caller(s)
	base := ctxdata.WithUserID(context.Background(), 42)
	_, _ = call(mcptoken.WithGrant(base, mcptoken.Grant{UserID: 42, Scope: mcptoken.ScopeRWGenerate}), agenttool.Call{Tool: agenttool.ToolGet})
	_, _ = call(base, agenttool.Call{Tool: agenttool.ToolGet, Client: "function"})
	_, _ = call(base, agenttool.Call{Tool: agenttool.ToolGet, Client: clientSvc})
	want := [][2]string{{mcptoken.AuthSourceMcpToken, mcptoken.ScopeRWGenerate}, {mcptoken.AuthSourceUser, ""}, {mcptoken.AuthSourceSvc, ""}}
	if len(rpc.got) != 3 {
		t.Fatalf("调用数: %d", len(rpc.got))
	}
	for i, w := range want {
		if rpc.got[i].AuthSource != w[0] || rpc.got[i].TokenScope != w[1] {
			t.Fatalf("第 %d 次: %s/%s want %v", i, rpc.got[i].AuthSource, rpc.got[i].TokenScope, w)
		}
	}
}
