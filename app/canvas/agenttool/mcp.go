package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

// MCP 服务端：JSON-RPC 2.0 over Streamable HTTP（MCP 2025-06-18 / 2025-03-26）。
//
//   - 只实现工具能力：initialize / ping / tools/list / tools/call，以及 notifications/*（回 202）。
//   - 无会话状态：不发 Mcp-Session-Id，不开服务端推送流（GET / DELETE 回 405）；每个 POST 返回一条 application/json 响应。
//   - 鉴权在外层（网关 Auth 中间件）；本处理器只校验 Origin（防 DNS rebinding）、协议版本与请求体大小。
//   - 工具业务错误以 tools/call 结果 isError=true 返回（模型能看到并自我修正），只有协议层错误才用 JSON-RPC error。

// 支持的协议版本（新的在前）。
var ProtocolVersions = []string{"2025-06-18", "2025-03-26"}

// Caller 执行一次工具调用。
type Caller func(ctx context.Context, call Call) (*Result, error)

// MCPServer MCP 处理器。
type MCPServer struct {
	Name, Version string
	Instructions  string
	Call          Caller
	// AllowedOrigins 浏览器来源白名单；请求带 Origin 且不在白名单 → 403。空 = 只接受不带 Origin 的（服务端 / 桌面）客户端。
	AllowedOrigins []string
	MaxBodyBytes   int64 // <=0 按 1MB
	// AgentName 从请求取客户端名（审计用）；nil 时取 X-Agent-Name / User-Agent。
	AgentName func(r *http.Request) string
	// ToolFilter tools/list 只列返回 true 的工具（如 MCP 访问令牌的作用域不含生成时不列生成类工具）；nil = 全部。
	// 只影响列表；调用时的作用域校验仍在 Call 里做。
	ToolFilter func(r *http.Request, t Tool) bool
}

// JSON-RPC 错误码。
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternal       = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// RawJSONHeader 响应头：告诉网关的 jsoncase 中间件不要给本响应补驼峰别名（JSON-RPC / JSON Schema 的键是协议的一部分）。
const RawJSONHeader = "X-Jsoncase"

func (s *MCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(RawJSONHeader, "off")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !slices.Contains(s.AllowedOrigins, o) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(ProtocolVersions, v) {
		http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
		return
	}
	limit := s.MaxBodyBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeRPC(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: rpcParseError, Message: "读取请求失败"}})
		return
	}
	if int64(len(body)) > limit {
		writeRPC(w, http.StatusRequestEntityTooLarge, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: rpcInvalidRequest, Message: "请求体过大"}})
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		writeRPC(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: rpcInvalidRequest, Message: "不支持 JSON-RPC 批量请求"}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: rpcParseError, Message: "JSON 解析失败"}})
		return
	}
	// 通知 / 客户端发来的响应：没有 id（或没有 method）→ 202，无响应体
	if len(req.ID) == 0 || string(req.ID) == "null" || req.Method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPC(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: rpcInvalidRequest, Message: "jsonrpc 必须是 2.0"}})
		return
	}
	res, rerr := s.dispatch(r, req)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr}
	if rerr == nil && res == nil {
		resp.Result = map[string]any{}
	}
	writeRPC(w, http.StatusOK, resp)
}

func (s *MCPServer) dispatch(r *http.Request, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		ver := ProtocolVersions[0]
		if slices.Contains(ProtocolVersions, p.ProtocolVersion) {
			ver = p.ProtocolVersion
		}
		out := map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}
		if s.Instructions != "" {
			out["instructions"] = s.Instructions
		}
		return out, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var keep func(Tool) bool
		if s.ToolFilter != nil {
			keep = func(t Tool) bool { return s.ToolFilter(r, t) }
		}
		return map[string]any{"tools": RenderFiltered(FormatMCP, keep)}, nil
	case "tools/call":
		return s.toolsCall(r, req.Params)
	}
	return nil, &rpcError{Code: rpcMethodNotFound, Message: "不支持的方法：" + req.Method}
}

func (s *MCPServer) toolsCall(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      map[string]any  `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "tools/call 缺少 name"}
	}
	if _, ok := Find(p.Name); !ok {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "未知工具：" + p.Name}
	}
	callID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	for _, k := range []string{"idempotency_key", "idempotencyKey", "fusion/idempotency_key"} {
		if v, ok := p.Meta[k].(string); ok && strings.TrimSpace(v) != "" {
			callID = strings.TrimSpace(v)
		}
	}
	agent := ""
	if s.AgentName != nil {
		agent = SanitizeAgentName(s.AgentName(r))
	} else {
		agent = DefaultAgentName(r)
	}
	res, err := s.Call(r.Context(), Call{Tool: p.Name, Arguments: p.Arguments, CallID: callID, Client: "mcp", Agent: agent})
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			res = ErrorResult(ErrorData{ErrorCode: ErrParam, Message: ve.Error()})
		} else {
			return nil, &rpcError{Code: rpcInternal, Message: "工具执行失败，请稍后重试"}
		}
	}
	return MCPToolResult(res), nil
}

// MCPToolResult 工具结果 → MCP CallToolResult（structuredContent + 同内容的 text，兼容只读 content 的客户端）。
func MCPToolResult(res *Result) map[string]any {
	if res == nil {
		res = ErrorResult(ErrorData{ErrorCode: ErrServer, Message: "工具没有返回结果"})
	}
	data := res.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	var structured any
	if json.Unmarshal(data, &structured) != nil {
		structured = map[string]any{}
	}
	if _, ok := structured.(map[string]any); !ok {
		structured = map[string]any{"value": structured}
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(data)}},
		"structuredContent": structured,
		"isError":           res.IsError,
	}
}

// DefaultAgentName X-Agent-Name，其次 User-Agent（客户端自报，只作审计展示）：去控制字符、转义保留前缀、截 48 字。
func DefaultAgentName(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("X-Agent-Name"))
	if v == "" {
		v = strings.TrimSpace(r.Header.Get("User-Agent"))
	}
	return SanitizeAgentName(v)
}

// MaxAgentNameLen 客户端自报名的最大长度（审计 agent 列 64，留出 client 前缀的余量）。
const MaxAgentNameLen = 48

// SanitizeAgentName 客户端自报名清洗：
//   - 「svc:」是服务间入口（HMAC 验过的 key id）专用的执行者前缀，客户端自报的同名前缀转义成「svc_」，
//     审计 / 画布事件里的 actor 不会被冒充成服务调用（大小写、前后空白、重复前缀都转义）；
//   - 去掉控制字符（防日志注入），截 MaxAgentNameLen 字。
func SanitizeAgentName(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	for len(v) >= 4 && strings.EqualFold(v[:4], "svc:") {
		v = "svc_" + strings.TrimSpace(v[4:])
	}
	if rs := []rune(v); len(rs) > MaxAgentNameLen {
		v = string(rs[:MaxAgentNameLen])
	}
	return v
}

func writeRPC(w http.ResponseWriter, status int, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
