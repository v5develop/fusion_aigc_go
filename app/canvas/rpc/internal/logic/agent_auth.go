package logic

// 智能体调用的鉴权来源（AgentCallReq.auth_source，2026-11-14）：
//
//   - mcp_token：MCP 访问令牌。网关不透传 Bearer，canvas-rpc 也**不借用** Bearers 缓存里该用户网页登录留下的令牌
//     （rememberBearer 返回空）——否则一枚只读写画布的外部令牌就能借网页会话代用户调 Java（放大 / 扩图 / 重绘的取消、任务查询）。
//     需要 Java 令牌的操作直接回 MCP_TOKEN_JAVA_TOOL_UNSUPPORTED；生成类工具按 token_scope 二次校验（网关已拦一次）。
//   - svc：服务间入口，沿用 Bearers 缓存（canvas-agent-tools.md §12.1：取消 Java 工具 run 用该用户本人缓存的登录令牌；
//     §6.1：执行放大 / 扩图 / 重绘用该用户本人缓存的登录令牌，缓存里没有 → LOGIN_REQUIRED「这个操作需要你在网页里登录后再试」）。
//   - user / 空：登录令牌，照旧。

import (
	"context"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
)

type authSourceKey struct{}

func withAuthSource(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, authSourceKey{}, src)
}

// viaMcpToken 本次调用是否经 MCP 访问令牌鉴权。
func viaMcpToken(ctx context.Context) bool {
	v, _ := ctx.Value(authSourceKey{}).(string)
	return v == mcptoken.AuthSourceMcpToken
}

// viaSvc 本次调用是否来自服务间入口（Java 画布助手）。
func viaSvc(ctx context.Context) bool {
	v, _ := ctx.Value(authSourceKey{}).(string)
	return v == mcptoken.AuthSourceSvc
}

// msgLoginRequired 服务间入口需要用户 Java 登录令牌、但缓存里没有（或已过期）时给用户的话。
const msgLoginRequired = "这个操作需要你在网页里登录后再试"

// loginRequired 服务间入口（auth_source=svc）执行 Java 计费的工具节点，Bearers 缓存里没有该用户的登录令牌：
// 不代用户调用 Java，回明确的「去网页登录」（canvas-agent-tools.md §6.1）。
func loginRequired() *agentError {
	return agentErr(agenttool.ErrLoginRequired, msgLoginRequired, false)
}

// javaToolUnsupported MCP 令牌请求碰到需要用户 Java 登录令牌的操作。
func javaToolUnsupported() *agentError {
	return agentErr(agenttool.ErrMcpTokenJavaToolUnsupported, "这个操作需要在网页里登录后使用", false)
}
