package logic

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"google.golang.org/grpc/metadata"
)

func authTestCore() *core {
	return &core{s: &svc.ServiceContext{Bearers: svc.NewBearerCache(time.Hour), Java: javatool.New("http://java.invalid", time.Second)}, now: time.Now}
}

// 中1：MCP 令牌请求不借用 Bearers 缓存里该用户网页登录留下的令牌，也不写入。
func TestRememberBearerMcpTokenNoBorrow(t *testing.T) {
	c := authTestCore()
	c.s.Bearers.Put(42, "Bearer web-session")
	mcp := withAuthSource(context.Background(), mcptoken.AuthSourceMcpToken)
	if b := c.rememberBearer(mcp, 42); b != "" {
		t.Fatalf("MCP 令牌请求不应借用缓存: %q", b)
	}
	// 即便 metadata 里带了 Bearer（网关不会这么做），也不记、不用
	withMD := metadata.NewIncomingContext(mcp, metadata.Pairs(BearerMetadataKey, "Bearer mcp_leak"))
	if b := c.rememberBearer(withMD, 42); b != "" || c.s.Bearers.Get(42) != "Bearer web-session" {
		t.Fatalf("MCP 令牌请求不应写缓存: %q / %q", b, c.s.Bearers.Get(42))
	}
	for _, src := range []string{"", mcptoken.AuthSourceUser, mcptoken.AuthSourceSvc} {
		if b := c.rememberBearer(withAuthSource(context.Background(), src), 42); b != "Bearer web-session" {
			t.Fatalf("%q 来源应照旧使用缓存: %q", src, b)
		}
	}
}

// 中1：需要 Java 令牌的后端在 MCP 令牌请求下直接回 MCP_TOKEN_JAVA_TOOL_UNSUPPORTED。
func TestReadyJavaBackendMcpToken(t *testing.T) {
	c := authTestCore()
	err := c.ready(withAuthSource(context.Background(), mcptoken.AuthSourceMcpToken), &runSpec{Backend: model.BackendJava}, "")
	var ae *agentError
	if !errors.As(err, &ae) || ae.ErrorCode != agenttool.ErrMcpTokenJavaToolUnsupported || ae.Message != "这个操作需要在网页里登录后使用" {
		t.Fatalf("应回 MCP_TOKEN_JAVA_TOOL_UNSUPPORTED: %v", err)
	}
	if err := c.ready(context.Background(), &runSpec{Backend: model.BackendJava}, ""); errors.As(err, &ae) {
		t.Fatalf("登录令牌请求不应回 MCP 专用错误: %v", err)
	}
}

// 低：scope 下传后 canvas-rpc 二次校验生成类工具（网关漏拦也挡得住），且不触库。
func TestAgentCallTokenScopeSecondCheck(t *testing.T) {
	c := authTestCore()
	args := `{"canvas_id":"cv_0123456789abcdef","node_id":"m_1"}`
	for _, scope := range []string{mcptoken.ScopeRW, "", "bogus"} {
		resp, err := c.AgentCall(context.Background(), &canvas.AgentCallReq{UserId: 42, Tool: agenttool.ToolRunNode, Arguments: args,
			AuthSource: mcptoken.AuthSourceMcpToken, TokenScope: scope})
		if err != nil {
			t.Fatal(err)
		}
		var out agenttool.ErrorData
		_ = json.Unmarshal([]byte(resp.Result), &out)
		if !resp.IsError || out.ErrorCode != agenttool.ErrForbiddenScope {
			t.Fatalf("scope=%q 调生成应 FORBIDDEN_SCOPE: %s", scope, resp.Result)
		}
	}
}
