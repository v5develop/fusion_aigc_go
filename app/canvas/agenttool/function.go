package agenttool

import (
	"encoding/json"
	"strings"
)

// ParseFunctionCall 解析 Function Call 执行请求体，接受三种形状：
//
//	{"name": "canvas_get", "arguments": {...} | "<json>", "call_id": "..."}          // 通用
//	{"id": "call_abc", "type": "function", "function": {"name": "...", "arguments": "<json>"}}  // OpenAI tool_call 原样
//	{"id": "toolu_01", "type": "tool_use", "name": "...", "input": {...}}               // Anthropic tool_use 原样
//
// call_id 缺省取 tool_call.id / tool_use.id（同一次模型输出里稳定，适合当幂等键）。
func ParseFunctionCall(body []byte) (Call, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		CallID    string          `json:"call_id"`
		ID        string          `json:"id"`
		Function  *struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Call{}, &ValidationError{"", "请求体必须是 JSON 对象"}
	}
	c := Call{Tool: p.Name, Arguments: p.Arguments, CallID: strings.TrimSpace(p.CallID), Client: "function"}
	if p.Function != nil {
		c.Tool, c.Arguments = p.Function.Name, p.Function.Arguments
	}
	if len(c.Arguments) == 0 && len(p.Input) > 0 {
		c.Arguments = p.Input
	}
	if c.CallID == "" {
		c.CallID = strings.TrimSpace(p.ID)
	}
	c.Tool = strings.TrimSpace(c.Tool)
	if c.Tool == "" {
		return Call{}, &ValidationError{"name", "缺少工具名"}
	}
	if len([]rune(c.CallID)) > 64 {
		return Call{}, &ValidationError{"call_id", "最长 64 字符"}
	}
	return c, nil
}
