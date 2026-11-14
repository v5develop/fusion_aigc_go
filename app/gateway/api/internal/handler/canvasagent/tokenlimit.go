package canvasagent

// MCP 令牌按令牌限速（设计见 docs/design/canvas-agent-tools.md §13）：
// 每个 mcp_ 令牌（按令牌 id）独立每分钟计数——默认 60 次调用 / 10 次生成，Config.CanvasAgent.McpTokenCallsPerMinute /
// McpTokenGeneratePerMinute 可配，<=0 不限。在按用户限速（CanvasAgentQuota / CanvasAgentGenQuota）之外再叠一层，
// 超出回 is_error RATE_LIMITED（与用户级限速同形）。计数在会话 Redis（多实例共享）；没有 Redis 退回进程内计数并 WARN。

import (
	"sync"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/userquota"

	"github.com/zeromicro/go-zero/core/logx"
)

type tokenLimiters struct {
	call *userquota.Limiter
	gen  *userquota.Limiter
}

var tokenLimiterCache sync.Map // *svc.ServiceContext → *tokenLimiters（几个入口各建一次 Caller，要共用同一份进程内计数）

func tokenLimitersFor(svcCtx *svc.ServiceContext) *tokenLimiters {
	if v, ok := tokenLimiterCache.Load(svcCtx); ok {
		return v.(*tokenLimiters)
	}
	c := svcCtx.Config.CanvasAgent
	rc := sessionRedis(svcCtx)
	tl := &tokenLimiters{
		call: userquota.New("canvas_agent:mcp_token:call", c.McpTokenCallsPerMinute, 0, rc),
		gen:  userquota.New("canvas_agent:mcp_token:gen", c.McpTokenGeneratePerMinute, 0, rc),
	}
	actual, loaded := tokenLimiterCache.LoadOrStore(svcCtx, tl)
	if !loaded && rc == nil && (c.McpTokenCallsPerMinute > 0 || c.McpTokenGeneratePerMinute > 0) {
		logx.Errorf("canvas agent: 没有会话 Redis，MCP 令牌限速退回进程内计数（check=mcp_token.rate_limit；多实例各算各的，仅适用于开发）")
	}
	return actual.(*tokenLimiters)
}
