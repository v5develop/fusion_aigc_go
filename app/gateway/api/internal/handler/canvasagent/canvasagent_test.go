package canvasagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/userquota"

	"google.golang.org/grpc"
)

type fakeRPC struct {
	canvas_client.Canvas // 未用到的方法不实现
	got                  []*canvas_client.AgentCallReq
}

func (f *fakeRPC) AgentCall(_ context.Context, in *canvas_client.AgentCallReq, _ ...grpc.CallOption) (*canvas_client.AgentCallResp, error) {
	f.got = append(f.got, in)
	return &canvas_client.AgentCallResp{Result: `{"canvas_id":"cv_0123456789abcdef","revision":"1-a"}`}, nil
}

func testSvc(rpc *fakeRPC, perMin, genPerMin int) *svc.ServiceContext {
	s := &svc.ServiceContext{CanvasRpc: rpc}
	s.Config.CanvasAgent.Enabled = true
	s.CanvasAgentQuota = userquota.New("t:call", perMin, 0, nil)
	s.CanvasAgentGenQuota = userquota.New("t:gen", genPerMin, 0, nil)
	return s
}

func withUser(r *http.Request, uid int64) *http.Request {
	return r.WithContext(ctxdata.WithUserID(r.Context(), uid))
}

func TestCallHandlerUsesAuthUserAndRawJSON(t *testing.T) {
	result.Setup()
	rpc := &fakeRPC{}
	h := jsoncase.Middleware(CallHandler(testSvc(rpc, 10, 10)))
	body := `{"id":"call_9","type":"function","function":{"name":"canvas_get","arguments":"{\"canvas_id\":\"cv_0123456789abcdef\",\"user_id\":1}"}}`
	req := withUser(httptest.NewRequest(http.MethodPost, "/api/v1/canvas/agent/call", strings.NewReader(body)), 42)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Name", "unit")
	rec := httptest.NewRecorder()
	h(rec, req)
	if len(rpc.got) != 1 {
		t.Fatalf("应调一次 canvas-rpc: %s", rec.Body.String())
	}
	in := rpc.got[0]
	if in.UserId != 42 || in.Tool != agenttool.ToolGet || in.CallId != "call_9" || in.Client != "function" || in.Agent != "unit" {
		t.Fatalf("透传不对: %+v", in)
	}
	// 参数里的 user_id 原样交给 canvas-rpc 由 Schema 拒绝；身份只取鉴权结果
	if !strings.Contains(in.Arguments, "user_id") {
		t.Fatal("参数应原样透传")
	}
	var out struct {
		Code int `json:"code"`
		Data map[string]any
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Code != 0 {
		t.Fatalf("响应: %s", rec.Body.String())
	}
	if _, aliased := out.Data["callId"]; aliased {
		t.Fatal("工具响应不应被 jsoncase 补驼峰别名")
	}
	if out.Data["result"].(map[string]any)["revision"] != "1-a" || out.Data["is_error"] != false {
		t.Fatalf("结果: %v", out.Data)
	}
}

func TestCallerQuotaAndAuth(t *testing.T) {
	rpc := &fakeRPC{}
	call := Caller(testSvc(rpc, 2, 1))
	ctx := ctxdata.WithUserID(context.Background(), 7)
	if r, _ := call(context.Background(), agenttool.Call{Tool: agenttool.ToolGet}); !r.IsError || !strings.Contains(string(r.Data), agenttool.ErrUnauthorized) {
		t.Fatalf("未登录应拒绝: %s", r.Data)
	}
	if r, _ := call(ctx, agenttool.Call{Tool: "nope"}); !strings.Contains(string(r.Data), agenttool.ErrUnknownTool) {
		t.Fatalf("未知工具: %s", r.Data)
	}
	if r, _ := call(ctx, agenttool.Call{Tool: agenttool.ToolRunNode}); r.IsError {
		t.Fatalf("第一次生成应放行: %s", r.Data)
	}
	if r, _ := call(ctx, agenttool.Call{Tool: agenttool.ToolRunNode}); !r.IsError || !strings.Contains(string(r.Data), agenttool.ErrRateLimited) {
		t.Fatalf("生成配额 1/分钟，第二次应限流: %s", r.Data)
	}
	if r, _ := call(ctx, agenttool.Call{Tool: agenttool.ToolGet}); !r.IsError || !strings.Contains(string(r.Data), agenttool.ErrRateLimited) {
		t.Fatalf("总配额 2/分钟，第三次应限流: %s", r.Data)
	}
	if len(rpc.got) != 1 {
		t.Fatalf("被限流的调用不应到达 canvas-rpc: %d", len(rpc.got))
	}
}

func TestToolsHandlerFormats(t *testing.T) {
	result.Setup()
	h := jsoncase.Middleware(ToolsHandler)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/v1/canvas/agent/tools?format=anthropic", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"input_schema"`) || strings.Contains(body, `"inputSchema"`) || strings.Contains(body, `"canvasId"`) {
		t.Fatalf("anthropic 格式或别名不对: %.300s", body)
	}
}

// MCP 令牌按令牌限速：每个令牌独立计数、超出回 RATE_LIMITED（与用户级同形）；不影响登录令牌；被拒的不到达 canvas-rpc。
func TestCallerPerTokenRateLimit(t *testing.T) {
	rpc := &fakeRPC{}
	s := testSvc(rpc, 100, 100)
	s.Config.CanvasAgent.McpTokenCallsPerMinute = 3
	s.Config.CanvasAgent.McpTokenGeneratePerMinute = 1
	call := Caller(s)
	base := ctxdata.WithUserID(context.Background(), 7)
	tokA := mcptoken.WithGrant(base, mcptoken.Grant{TokenID: 1, UserID: 7, Scope: mcptoken.ScopeRWGenerate})
	tokB := mcptoken.WithGrant(base, mcptoken.Grant{TokenID: 2, UserID: 7, Scope: mcptoken.ScopeRWGenerate})
	limitedRes := func(ctx context.Context, tool string) bool {
		r, _ := call(ctx, agenttool.Call{Tool: tool})
		return r.IsError && strings.Contains(string(r.Data), agenttool.ErrRateLimited)
	}
	if limitedRes(tokA, agenttool.ToolRunNode) {
		t.Fatal("令牌 A 第一次生成应放行")
	}
	if !limitedRes(tokA, agenttool.ToolRunNode) {
		t.Fatal("令牌 A 生成 1/分钟，第二次应限流")
	}
	// 总数先于生成扣：上面两次生成已占 2 次，第三次读取放行，第四次限流
	if limitedRes(tokA, agenttool.ToolGet) {
		t.Fatal("令牌 A 总数 3/分钟，第三次应放行")
	}
	if !limitedRes(tokA, agenttool.ToolGet) {
		t.Fatal("令牌 A 总数 3/分钟，第四次应限流")
	}
	// 另一个令牌（同一用户）独立计数
	if limitedRes(tokB, agenttool.ToolGet) || limitedRes(tokB, agenttool.ToolRunNode) {
		t.Fatal("令牌 B 独立计数，应放行")
	}
	// 登录令牌（无 Grant）不受令牌级限速
	for i := 0; i < 10; i++ {
		if limitedRes(base, agenttool.ToolGet) {
			t.Fatal("登录令牌不受令牌级限速")
		}
	}
	if len(rpc.got) != 2+2+10 {
		t.Fatalf("被令牌限流的调用不应到达 canvas-rpc: %d", len(rpc.got))
	}
}
