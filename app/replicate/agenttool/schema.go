// Package agenttool 视频复刻智能体的工具定义：唯一的一份工具清单（名称 / 标题 / 给规划模型看的中文说明 / 入参 JSON Schema / 注解），
// 渲染成 Function Call（OpenAI / Anthropic）与 MCP（tools/list）三种形式，并提供入参校验。
//
// 约定见 fusion-aigc/.scratch/video-replicate-agent/CONTRACT.md §B。形状与画布 app/canvas/agenttool 同构：
// Schema / Props / Validate / ParseFunctionCall / Call / Result / Format 直接复用画布包导出的（同一份校验器、同一份调用形状），
// 其余小工具函数（str / obj / arr …）画布包没导出，这里留了一份很小的本地副本。
// 执行在网关内部入口（app/gateway/api/internal/handler/replicateagent），经 app/replicate/httpapi.Ops 走与 REST 同一套口径。
package agenttool

import (
	canvas "aigc-local-life/app/canvas/agenttool"
)

// 复用画布包的类型与校验器。
type (
	Schema = canvas.Schema
	Props  = canvas.Props
	Format = canvas.Format
	Call   = canvas.Call
	Result = canvas.Result
)

// 工具输出格式。
const (
	FormatOpenAI    = canvas.FormatOpenAI
	FormatAnthropic = canvas.FormatAnthropic
	FormatMCP       = canvas.FormatMCP
)

var (
	// ParseFormat 不认识的按 openai。
	ParseFormat = canvas.ParseFormat
	// ParseFunctionCall 解析 Function Call 执行请求体（{name|tool?, arguments, call_id} 等三种形状）。
	ParseFunctionCall = canvas.ParseFunctionCall
	// Validate 按 Schema 校验入参（含 camelCase → snake_case 归一）。
	Validate = canvas.Validate
)

func str(desc string) Schema { return Schema{"type": "string", "description": desc} }

func strMax(desc string, n int) Schema {
	return Schema{"type": "string", "description": desc, "maxLength": n}
}

func enum(desc string, vals ...string) Schema {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return Schema{"type": "string", "description": desc, "enum": out}
}

func integer(desc string, lo, hi int) Schema {
	return Schema{"type": "integer", "description": desc, "minimum": lo, "maximum": hi}
}

func number(desc string, lo, hi float64) Schema {
	return Schema{"type": "number", "description": desc, "minimum": lo, "maximum": hi}
}

func arr(desc string, items Schema, lo, hi int) Schema {
	s := Schema{"type": "array", "description": desc, "items": items}
	if lo > 0 {
		s["minItems"] = lo
	}
	if hi > 0 {
		s["maxItems"] = hi
	}
	return s
}

// obj 封闭对象（additionalProperties=false）。
func obj(desc string, props Props, required ...string) Schema {
	s := Schema{"type": "object", "properties": props, "additionalProperties": false}
	if desc != "" {
		s["description"] = desc
	}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, k := range required {
			r[i] = k
		}
		s["required"] = r
	}
	return s
}

// out 出参对象（开放：不加 additionalProperties=false，便于只增字段）。
func out(desc string, props Props) Schema {
	s := Schema{"type": "object", "properties": props}
	if desc != "" {
		s["description"] = desc
	}
	return s
}
