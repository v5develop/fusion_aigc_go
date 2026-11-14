// Package canvasagent 画布智能体工具的网关入口：同一份工具定义（app/canvas/agenttool）以两种形式暴露。
//
//	POST /api/v1/canvas/mcp          MCP（Streamable HTTP，JSON-RPC 2.0；initialize / tools/list / tools/call）
//	GET  /api/v1/canvas/agent/tools  Function Call 定义（?format=openai|anthropic|mcp）
//	POST /api/v1/canvas/agent/call   Function Call 执行（{name, arguments, call_id} 或 OpenAI tool_call / Anthropic tool_use 原样）
//
// 三个入口都在 agentAuth 之后：登录令牌走 Auth（会话令牌）+ CanvasBearer；Bearer mcp_… 走 MCP 访问令牌（mcptoken.go，带作用域）。
// 身份只取鉴权结果（ctxdata.UserID），工具参数里没有 user_id。令牌管理 /api/v1/canvas/mcp-tokens 只认登录令牌。
// 按用户频率配额（userquota，会话 Redis）在这里扣；执行在 canvas-rpc AgentCall（校验、幂等、审计、计费都在那边）。
// 不走 goctl 生成的 routes.go（gateway.api 是共享文件），由 gateway.go 显式调用 Register。设计见 docs/design/canvas-agent-tools.md。
package canvasagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/middleware"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/clientip"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
	"google.golang.org/grpc/metadata"
)

// bearerMetadataKey 与 canvas-rpc logic.BearerMetadataKey 一致。
const bearerMetadataKey = "x-user-authorization"

// mcpInstructions initialize 时给客户端的使用说明。
const mcpInstructions = "操作当前登录用户自己的画布。先 canvas_list / canvas_create 拿到 canvas_id，canvas_get 读结构；" +
	"用 canvas_add_nodes / canvas_connect 搭建（文字节点 → 模型 prompt 口，图片 → 模型 image 口），canvas_run_node 执行生成或工具节点（会扣点：执行前先用 canvas_quote_node 报价并告诉用户点数），" +
	"canvas_get_run 查询结果。写操作可带 Idempotency-Key 头或 _meta.idempotency_key 防重。"

// Register 注册三个入口。
func Register(server *rest.Server, svcCtx *svc.ServiceContext) {
	mcp := &agenttool.MCPServer{
		Name: "fusion-canvas", Version: "1.0.0", Instructions: mcpInstructions,
		Call: Caller(svcCtx), AllowedOrigins: svcCtx.Config.CanvasAgent.AllowedOrigins, MaxBodyBytes: svcCtx.Config.CanvasAgent.MaxBodyBytes,
		// MCP 访问令牌只列作用域内的工具（canvas_rw 不列生成类）
		ToolFilter: func(r *http.Request, t agenttool.Tool) bool { return toolVisible(r.Context(), t.Generates) },
	}
	// 鉴权：Bearer mcp_… → MCP 访问令牌（作用域进 context，不透传 Bearer）；其余 → 登录态 Auth + CanvasBearer（见 mcptoken.go）
	tokens := newTokenResolver(svcCtx)
	mw := []rest.Middleware{agentAuth(svcCtx, tokens)}
	server.AddRoutes(rest.WithMiddlewares(mw, []rest.Route{
		{Method: http.MethodPost, Path: "/mcp", Handler: enabled(svcCtx, mcp.ServeHTTP)},
		// MCP 规范：不提供服务端推送流时 GET 回 405（不经 Auth 也无妨，但统一挂在同一组里）
		{Method: http.MethodGet, Path: "/mcp", Handler: enabled(svcCtx, mcp.ServeHTTP)},
		{Method: http.MethodDelete, Path: "/mcp", Handler: enabled(svcCtx, mcp.ServeHTTP)},
		{Method: http.MethodGet, Path: "/agent/tools", Handler: enabled(svcCtx, ToolsHandler)},
		{Method: http.MethodPost, Path: "/agent/call", Handler: enabled(svcCtx, CallHandler(svcCtx))},
	}...), rest.WithPrefix("/api/v1/canvas"))
	// MCP 访问令牌管理（只认登录令牌）
	registerMcpTokens(server, svcCtx, tokens)
}

func enabled(svcCtx *svc.ServiceContext, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !svcCtx.Config.CanvasAgent.Enabled {
			http.NotFound(w, r)
			return
		}
		// 审计 IP：只信最近一跳可信代理写的头（X-Forwarded-For 首段可伪造），与其他入口同一取法
		h(w, r.WithContext(context.WithValue(r.Context(), ipKey{}, clientip.From(r))))
	}
}

// ToolsHandler Function Call 工具定义。
func ToolsHandler(w http.ResponseWriter, r *http.Request) {
	f := agenttool.ParseFormat(r.URL.Query().Get("format"))
	w.Header().Set(jsoncase.SkipHeader, "off") // JSON Schema 的键名是协议的一部分，不补驼峰别名
	tools := agenttool.RenderFiltered(f, func(t agenttool.Tool) bool { return toolVisible(r.Context(), t.Generates) })
	httpx.OkJsonCtx(r.Context(), w, map[string]any{"format": string(f), "tools": tools})
}

// CallHandler Function Call 执行。
func CallHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	call := Caller(svcCtx)
	return func(w http.ResponseWriter, r *http.Request) {
		limit := svcCtx.Config.CanvasAgent.MaxBodyBytes
		if limit <= 0 {
			limit = 1 << 20
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil || int64(len(body)) > limit {
			httpx.ErrorCtx(r.Context(), w, xerr.Param("请求体过大或读取失败"))
			return
		}
		c, err := agenttool.ParseFunctionCall(body)
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, xerr.Param(err.Error()))
			return
		}
		if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" && c.CallID == "" {
			c.CallID = k
		}
		c.Agent = agenttool.DefaultAgentName(r)
		writeCallResult(w, r, c, call)
	}
}

// writeCallResult 执行并按 Function Call 出参写回（用户入口与服务间入口共用）。
func writeCallResult(w http.ResponseWriter, r *http.Request, c agenttool.Call, call agenttool.Caller) {
	res, err := call(r.Context(), c)
	if err != nil {
		httpx.ErrorCtx(r.Context(), w, err)
		return
	}
	writeResult(w, r, c, res)
}

// writeResult 按 Function Call 出参写回：{name, call_id, is_error, replayed, result, content}（经统一信封包成 data）。
func writeResult(w http.ResponseWriter, r *http.Request, c agenttool.Call, res *agenttool.Result) {
	var result any
	if json.Unmarshal(res.Data, &result) != nil {
		result = map[string]any{}
	}
	w.Header().Set(jsoncase.SkipHeader, "off")
	httpx.OkJsonCtx(r.Context(), w, map[string]any{
		"name": c.Tool, "call_id": c.CallID, "is_error": res.IsError, "replayed": res.Replayed,
		"result": result, "content": string(res.Data), // content：可直接作为 tool 消息内容回填给模型
	})
}

// Caller 网关侧执行：配额 → canvas-rpc AgentCall。
func Caller(svcCtx *svc.ServiceContext) agenttool.Caller {
	return func(ctx context.Context, c agenttool.Call) (*agenttool.Result, error) {
		uid := ctxdata.UserID(ctx)
		if uid <= 0 {
			return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrUnauthorized, Message: "未登录"}), nil
		}
		t, ok := agenttool.Find(c.Tool)
		if !ok {
			return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrUnknownTool, Message: "没有这个工具：" + c.Tool}), nil
		}
		// MCP 访问令牌的作用域（登录令牌 / 服务间入口不受限）：先于配额，越权调用不占配额
		if msg := scopeDenied(ctx, t.Generates); msg != "" {
			return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrForbiddenScope, Message: msg}), nil
		}
		// MCP 令牌自己的每分钟上限（按令牌计数，先于按用户的配额：一个失控的令牌先被它自己的上限挡住，不拖垮同用户的其它令牌与网页）
		if g, isMcp := mcptoken.GrantFrom(ctx); isMcp {
			tl := tokenLimitersFor(svcCtx)
			if res := limited(ctx, tl.call, g.TokenID, "该令牌的工具调用"); res != nil {
				return res, nil
			}
			if t.Generates {
				if res := limited(ctx, tl.gen, g.TokenID, "该令牌的生成"); res != nil {
					return res, nil
				}
			}
		}
		if res := limited(ctx, svcCtx.CanvasAgentQuota, uid, "工具调用"); res != nil {
			return res, nil
		}
		if t.Generates {
			if res := limited(ctx, svcCtx.CanvasAgentGenQuota, uid, "生成"); res != nil {
				return res, nil
			}
		}
		args := string(c.Arguments)
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		rctx := ctx
		if b := middleware.Bearer(ctx); b != "" {
			rctx = metadata.AppendToOutgoingContext(ctx, bearerMetadataKey, b)
		}
		src, scope := authSource(ctx, c.Client)
		out, err := svcCtx.CanvasRpc.AgentCall(rctx, &canvas_client.AgentCallReq{
			UserId: uid, Tool: t.Name, Arguments: args, CallId: c.CallID, Client: c.Client, Agent: c.Agent, ClientIp: clientIP(ctx),
			AuthSource: src, TokenScope: scope,
		})
		if err != nil {
			if code, msg, ok := xerr.Parse(err); ok {
				return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: xerr.Reason(code), Message: msg}), nil
			}
			logx.WithContext(ctx).Errorf("canvas agent: AgentCall user=%d tool=%s: %v", uid, t.Name, err)
			return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrServer, Message: "服务繁忙，请稍后重试", Retryable: true}), nil
		}
		return &agenttool.Result{Data: json.RawMessage(out.Result), IsError: out.IsError, Replayed: out.Replayed}, nil
	}
}

func limited(ctx context.Context, l *userquota.Limiter, uid int64, what string) *agenttool.Result {
	switch l.Take(ctx, uid) {
	case userquota.MinuteExceed:
		return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrRateLimited, Message: what + "太频繁，请稍后再试", Retryable: true, RetryAfterSeconds: 60})
	case userquota.DayExceed:
		return agenttool.ErrorResult(agenttool.ErrorData{ErrorCode: agenttool.ErrRateLimited, Message: "今天的" + what + "次数已用完，请明天再试", Retryable: false})
	}
	return nil
}

type ipKey struct{}

// clientIP 审计用调用方 IP（enabled 包装时放进 ctx）。
func clientIP(ctx context.Context) string {
	v, _ := ctx.Value(ipKey{}).(string)
	return v
}
