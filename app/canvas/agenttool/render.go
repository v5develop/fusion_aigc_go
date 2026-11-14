package agenttool

import "encoding/json"

// 三种对外格式都从 registry 渲染：改工具只改 tools.go 一处。

// Format 工具定义的输出格式。
type Format string

const (
	FormatOpenAI    Format = "openai"    // Chat Completions / Responses 的 tools：[{type:function, function:{name, description, parameters}}]
	FormatAnthropic Format = "anthropic" // Messages API 的 tools：[{name, description, input_schema}]
	FormatMCP       Format = "mcp"       // MCP tools/list：[{name, title, description, inputSchema, outputSchema, annotations}]
)

// ParseFormat 不认识的按 openai。
func ParseFormat(s string) Format {
	switch Format(s) {
	case FormatAnthropic, FormatMCP:
		return Format(s)
	}
	return FormatOpenAI
}

// Render 按格式渲染全部工具。
func Render(f Format) []map[string]any {
	out := make([]map[string]any, 0, len(registry))
	for _, t := range registry {
		out = append(out, t.render(f))
	}
	return out
}

// RenderFiltered 按格式渲染 keep 返回 true 的工具（keep 为 nil = 全部）。
func RenderFiltered(f Format, keep func(Tool) bool) []map[string]any {
	if keep == nil {
		return Render(f)
	}
	out := make([]map[string]any, 0, len(registry))
	for _, t := range registry {
		if keep(t) {
			out = append(out, t.render(f))
		}
	}
	return out
}

func (t Tool) render(f Format) map[string]any {
	switch f {
	case FormatAnthropic:
		return map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Input}
	case FormatMCP:
		return map[string]any{
			"name": t.Name, "title": t.Title, "description": t.Description,
			"inputSchema": t.Input, "outputSchema": t.Output,
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
				"idempotentHint":  t.Idempotent,
				"openWorldHint":   t.Generates, // 生成类会调用外部模型供应商
			},
		}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Input,
		},
	}
}

// ---------------------------------------------------------------- 调用

// Call 一次工具调用（网关 → canvas-rpc）。
type Call struct {
	Tool      string
	Arguments json.RawMessage // JSON 对象；空 = {}
	CallID    string          // 幂等键（可空）
	Client    string          // mcp / function
	Agent     string          // 客户端自报的名字（审计用）
}

// Result 工具执行结果。IsError=true 时 Data 为 {error_code, message, retryable, ...}。
type Result struct {
	Data     json.RawMessage
	IsError  bool
	Replayed bool
}

// ErrorData 工具错误结果的统一形状。
type ErrorData struct {
	ErrorCode         string `json:"error_code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	Revision          string `json:"revision,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	// QuotedPoints / MaxPoints BUDGET_EXCEEDED 时的报价与调用方给的上限
	QuotedPoints int64 `json:"quoted_points,omitempty"`
	MaxPoints    int64 `json:"max_points,omitempty"`
}

// ErrorResult 组装错误结果。
func ErrorResult(e ErrorData) *Result {
	b, _ := json.Marshal(e)
	return &Result{Data: b, IsError: true}
}

// 错误码（工具结果里的 error_code）。
const (
	ErrParam               = "PARAM_INVALID"
	ErrNotFound            = "NOT_FOUND"
	ErrConflict            = "CANVAS_CONFLICT"
	ErrIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	ErrInProgress          = "CALL_IN_PROGRESS"
	ErrRateLimited         = "RATE_LIMITED"
	ErrInsufficientPoints  = "INSUFFICIENT_POINTS"
	ErrNotReady            = "NOT_READY"
	ErrForbidden           = "FORBIDDEN"
	ErrUnauthorized        = "UNAUTHORIZED"
	ErrUnknownTool         = "UNKNOWN_TOOL"
	ErrServer              = "SERVER_ERROR"
	// ErrForbiddenScope MCP 访问令牌的权限范围不含该工具（如只读写令牌调 canvas_run_node），换带生成权限的令牌
	ErrForbiddenScope = "FORBIDDEN_SCOPE"
	// ErrMcpTokenJavaToolUnsupported MCP 访问令牌请求碰到需要用户 Java 登录令牌的操作（如取消 Java 工具生成）
	ErrMcpTokenJavaToolUnsupported = "MCP_TOKEN_JAVA_TOOL_UNSUPPORTED"
	// ErrLoginRequired 服务间入口（Java 画布助手）执行需要用户 Java 登录令牌的工具，但缓存里没有该用户的令牌：请用户在网页登录后再试
	ErrLoginRequired = "LOGIN_REQUIRED"
	// ErrBudgetExceeded canvas_run_node 带 max_points 且冻结前报价超出：不冻结、不提交（结果带 quoted_points / max_points）
	ErrBudgetExceeded = "BUDGET_EXCEEDED"
	// ErrPriceUnknownNeedsConfirm 带 max_points 但报不出准确点数（Java 计费工具没给 expected_points，或报价暂不可用）：先征得用户确认
	ErrPriceUnknownNeedsConfirm = "PRICE_UNKNOWN_NEEDS_CONFIRM"
)

// ParseArguments 解析入参为对象并按工具 Schema 校验（含 camelCase 归一），返回归一后的 JSON。
// 入参可以是 JSON 对象，也可以是「JSON 对象的字符串」（OpenAI tool_call.function.arguments 的形状）。
func ParseArguments(t Tool, raw json.RawMessage) (map[string]any, error) {
	var v any = map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, &ValidationError{"", "arguments 必须是 JSON 对象"}
		}
		if s, ok := v.(string); ok {
			if s == "" {
				v = map[string]any{}
			} else if err := json.Unmarshal([]byte(s), &v); err != nil {
				return nil, &ValidationError{"", "arguments 必须是 JSON 对象"}
			}
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &ValidationError{"", "arguments 必须是 JSON 对象"}
	}
	if err := Validate(t.Input, m); err != nil {
		return nil, err
	}
	return m, nil
}
