// Package agenttool 画布智能体工具：唯一的一份工具清单（名称 / 说明 / 入参出参 JSON Schema / 注解），
// 渲染成 Function Call（OpenAI / Anthropic）与 MCP（tools/list）两种形式，并提供入参校验与 MCP Streamable HTTP 服务端。
//
// 执行在 canvas-rpc（rpc AgentCall）；本包不依赖 canvas-rpc，网关与 canvas-rpc 都引用它，保证两侧看到的是同一份定义。
// 设计见 docs/design/canvas-agent-tools.md。
package agenttool

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Schema JSON Schema（draft 2020-12 的子集：type / properties / required / additionalProperties / items /
// enum / pattern / minLength / maxLength / minimum / maximum / minItems / maxItems）。
type Schema = map[string]any

// Props 对象属性表。
type Props = map[string]any

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

func boolean(desc string) Schema { return Schema{"type": "boolean", "description": desc} }

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

// free 自由对象（键不限，不下钻）。
func free(desc string) Schema { return Schema{"type": "object", "description": desc} }

// out 出参对象（开放：不加 additionalProperties=false，便于只增字段）。
func out(props Props, required ...string) Schema {
	s := Schema{"type": "object", "properties": props}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, k := range required {
			r[i] = k
		}
		s["required"] = r
	}
	return s
}

// ---------------------------------------------------------------- 校验

// ValidationError 入参不符合 Schema。
type ValidationError struct {
	Path, Msg string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// Validate 按 Schema 校验 v（v 来自 encoding/json 解到 any：数字是 float64）。
// 校验前把对象里的 camelCase 键归一为 Schema 声明过的 snake_case 键（canvasId → canvas_id），自由对象（无 properties）不动。
func Validate(s Schema, v any) error { return validate("", s, v) }

func validate(path string, s Schema, v any) error {
	if s == nil {
		return nil
	}
	if ev, ok := s["enum"].([]any); ok {
		hit := false
		for _, e := range ev {
			if e == v {
				hit = true
				break
			}
		}
		if !hit {
			return &ValidationError{path, fmt.Sprintf("取值必须是 %s 之一", joinAny(ev))}
		}
	}
	switch t, _ := s["type"].(string); t {
	case "string":
		x, ok := v.(string)
		if !ok {
			return &ValidationError{path, "必须是字符串"}
		}
		n := len([]rune(x))
		if lo, ok := intOf(s["minLength"]); ok && n < lo {
			return &ValidationError{path, fmt.Sprintf("至少 %d 个字符", lo)}
		}
		if hi, ok := intOf(s["maxLength"]); ok && n > hi {
			return &ValidationError{path, fmt.Sprintf("最多 %d 个字符", hi)}
		}
		if p, ok := s["pattern"].(string); ok && !compiled(p).MatchString(x) {
			return &ValidationError{path, "格式不对（应匹配 " + p + "）"}
		}
	case "integer", "number":
		f, ok := v.(float64)
		if !ok {
			return &ValidationError{path, "必须是数字"}
		}
		if t == "integer" && f != math.Trunc(f) {
			return &ValidationError{path, "必须是整数"}
		}
		if lo, ok := floatOf(s["minimum"]); ok && f < lo {
			return &ValidationError{path, fmt.Sprintf("不能小于 %v", lo)}
		}
		if hi, ok := floatOf(s["maximum"]); ok && f > hi {
			return &ValidationError{path, fmt.Sprintf("不能大于 %v", hi)}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return &ValidationError{path, "必须是 true / false"}
		}
	case "array":
		xs, ok := v.([]any)
		if !ok {
			return &ValidationError{path, "必须是数组"}
		}
		if lo, ok := intOf(s["minItems"]); ok && len(xs) < lo {
			return &ValidationError{path, fmt.Sprintf("至少 %d 项", lo)}
		}
		if hi, ok := intOf(s["maxItems"]); ok && len(xs) > hi {
			return &ValidationError{path, fmt.Sprintf("最多 %d 项（单次批量上限）", hi)}
		}
		if it, ok := s["items"].(Schema); ok {
			for i, x := range xs {
				if err := validate(fmt.Sprintf("%s[%d]", path, i), it, x); err != nil {
					return err
				}
			}
		}
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return &ValidationError{path, "必须是对象"}
		}
		props, _ := s["properties"].(Props)
		if props != nil {
			normalizeKeys(m, props)
		}
		if req, ok := s["required"].([]any); ok {
			for _, k := range req {
				if _, has := m[k.(string)]; !has {
					return &ValidationError{join(path, k.(string)), "缺少必填字段"}
				}
			}
		}
		closed := s["additionalProperties"] == false
		for _, k := range sortedKeys(m) {
			ps, known := props[k]
			if !known {
				if closed {
					return &ValidationError{join(path, k), "不认识的字段（可用：" + strings.Join(sortedProps(props), ", ") + "）"}
				}
				continue
			}
			if m[k] == nil { // 显式 null 视同未传
				delete(m, k)
				continue
			}
			if err := validate(join(path, k), ps.(Schema), m[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// normalizeKeys camelCase → 已声明的 snake_case（snake 键已存在时丢弃 camel 键，以 snake 为准）。
func normalizeKeys(m map[string]any, props Props) {
	for k, v := range m {
		if _, ok := props[k]; ok {
			continue
		}
		sk := toSnake(k)
		if sk == k {
			continue
		}
		if _, ok := props[sk]; !ok {
			continue
		}
		if _, exists := m[sk]; !exists {
			m[sk] = v
		}
		delete(m, k)
	}
}

func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func intOf(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), true
	}
	return 0, false
}

func floatOf(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func joinAny(xs []any) string {
	ss := make([]string, len(xs))
	for i, x := range xs {
		ss[i] = fmt.Sprint(x)
	}
	return strings.Join(ss, " / ")
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedProps(p Props) []string {
	ks := make([]string, 0, len(p))
	for k := range p {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

var reCache sync.Map

func compiled(p string) *regexp.Regexp {
	if v, ok := reCache.Load(p); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile(p)
	reCache.Store(p, re)
	return re
}
