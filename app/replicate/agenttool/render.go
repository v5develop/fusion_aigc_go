package agenttool

import (
	"encoding/json"
	"strings"

	canvas "aigc-local-life/app/canvas/agenttool"
)

// Render 按格式渲染全部工具。
func Render(f Format) []map[string]any {
	out := make([]map[string]any, 0, len(registry))
	for _, t := range registry {
		out = append(out, t.render(f))
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
				"openWorldHint":   t.Charges, // 扣点工具会调用外部模型供应商生成
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

// ParseArguments 解析入参为对象并按工具 Schema 校验（含 camelCase 归一）。
// 入参可以是 JSON 对象，也可以是「JSON 对象的字符串」（OpenAI tool_call.function.arguments 的形状）。
func ParseArguments(t Tool, raw json.RawMessage) (map[string]any, error) {
	var v any = map[string]any{}
	bad := &canvasValidation{msg: "arguments 必须是 JSON 对象"}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, bad
		}
		if s, ok := v.(string); ok {
			if strings.TrimSpace(s) == "" {
				v = map[string]any{}
			} else if err := json.Unmarshal([]byte(s), &v); err != nil {
				return nil, bad
			}
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, bad
	}
	if err := Validate(t.Input, m); err != nil {
		return nil, err
	}
	return m, nil
}

type canvasValidation struct{ msg string }

func (e *canvasValidation) Error() string { return e.msg }

// Gap 方案缺项（与 core.Gap 同形，工具错误里原样带给规划模型）。
type Gap struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ErrorData 工具错误结果的统一形状（CONTRACT §A）：{error_code, message, retryable, gaps?}。
// message 是给用户看的中文，不含内部码、供应商原文。
type ErrorData struct {
	ErrorCode         string `json:"error_code"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	Gaps              []Gap  `json:"gaps,omitempty"`
}

// ErrorResult 组装错误结果。
func ErrorResult(e ErrorData) *Result {
	b, _ := json.Marshal(e)
	return &Result{Data: b, IsError: true}
}

// 入口自己产生的错误码（其余取 Go reason，原样透传：QUOTE_CHANGED / PLAN_STALE / PLAN_EXPIRED / NOT_REDOABLE /
// MODERATION_REJECTED …）。
const (
	ErrParam         = "PARAM_INVALID"
	ErrUnknownTool   = "UNKNOWN_TOOL"
	ErrUnauthorized  = "UNAUTHORIZED"
	ErrNotReady      = "FEATURE_NOT_READY"
	ErrRateLimited   = "RATE_LIMITED"
	ErrServer        = "SYSTEM_ERROR"
	ErrCallIDMissing = "CALL_ID_REQUIRED"
)

// ParseCall 解析内部入口的请求体。契约形状 {"call_id","tool","arguments"}；同时接受画布入口的 {"name",…} 与
// OpenAI tool_call / Anthropic tool_use 原样（交给画布包的解析器）。同时给了 tool 与 name 时以 tool 为准。
func ParseCall(body []byte) (Call, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) == nil {
		var tool string
		if raw, ok := m["tool"]; ok && json.Unmarshal(raw, &tool) == nil && strings.TrimSpace(tool) != "" {
			nm, _ := json.Marshal(strings.TrimSpace(tool))
			m["name"] = nm
			delete(m, "tool")
			body, _ = json.Marshal(m)
		}
	}
	return canvas.ParseFunctionCall(body)
}
