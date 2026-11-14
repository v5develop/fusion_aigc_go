// Package jsoncase 让 Go 网关 / admin 的 JSON 字段同时支持 snake_case 与 Java 一致的 camelCase。
//
// 迁移期（增量双写，不破坏现有调用方）：
//   - 响应：每个 snake_case 键补一个 camelCase 别名，原键保留；
//   - 请求体：camelCase 键仅当它对应的 snake_case 名在白名单（known_gen.go）中、且请求里没有该 snake 键时，补上 snake 键；
//     白名单之外的键一律不动；
//   - 自由格式字段（freeFormKeys：.api 里类型为 interface{} / any / json.RawMessage / map 的字段名，如画布 snapshot、
//     Java 透传配置；画布工具 params 除外，其键是接口字段）的值请求、响应两侧都不下钻、原样保留字节——那是调用方私有 JSON，
//     里面恰好叫 taskId / run_id 的键不是接口字段，补别名会污染落库的快照。字符串承载的 JSON 文本不解析，天然不动。
//   - URL query：同一规则——camelCase 参数名对应的 snake 名在白名单中、且 query 里没有该 snake 参数时，
//     追加 snake 参数（值保持原始编码，重复参数逐个追加），原参数保留；snake 参数照旧可用。
//
// 非 JSON 响应（SSE、文件、静态资源）原样透传；JSON 响应超过 maxRespBytes 也透传（不改写）。
package jsoncase

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	maxReqBytes  = 4 << 20 // 请求体超过 4MB 不改写（须盖住画布保存：快照 ≤2MB，超限不改写会丢掉 ifMatchRevision → 保存被拒）
	maxRespBytes = 4 << 20 // 响应体超过 4MB 不改写
)

// SkipHeader 处理器在写响应前设置 "X-Jsoncase: off"，本响应不补驼峰别名（头本身不会发给客户端）。
const SkipHeader = "X-Jsoncase"

// Middleware 可直接传给 rest.Server.Use。
func Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if verbatim(r) { // 原样放行：请求与响应都不做 snake / camel 别名改写
			next(w, r)
			return
		}
		rewriteRequest(r)
		rw := &respWriter{ResponseWriter: w}
		next(rw, r)
		rw.finish()
	}
}

// ---------------------------------------------------------------- 请求

func rewriteRequest(r *http.Request) {
	rewriteQuery(r)
	if r.Body == nil || r.Body == http.NoBody || !isJSON(r.Header.Get("Content-Type")) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReqBytes+1))
	_ = r.Body.Close()
	if err != nil || len(raw) > maxReqBytes {
		r.Body = io.NopCloser(bytes.NewReader(raw)) // 超限或读失败：原样放回
		return
	}
	out := raw
	if b, changed := rewriteTree(raw, addSnakeAlias); changed {
		out = b
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
}

// rewriteQuery 在 RawQuery 上原样追加 snake 别名参数：不重排、不重编码已有参数，
// 新参数的值直接复用原参数的原始编码（"=" 之后的部分），所以编码不会被改写。
func rewriteQuery(r *http.Request) {
	if r.URL == nil || r.URL.RawQuery == "" {
		return
	}
	raw := r.URL.RawQuery
	pairs := strings.Split(raw, "&")
	keys := make([]string, len(pairs))
	present := make(map[string]bool, len(pairs))
	for i, p := range pairs {
		k := p
		if j := strings.IndexByte(p, '='); j >= 0 {
			k = p[:j]
		}
		dk, err := url.QueryUnescape(k)
		if err != nil {
			continue // 键编码非法：不参与改写，原样留给下游处理
		}
		keys[i] = dk
		present[dk] = true
	}
	var added []string
	for i, p := range pairs {
		k := keys[i]
		if k == "" {
			continue
		}
		snake := toSnake(k)
		if snake == k || !knownSnake[snake] || present[snake] {
			continue
		}
		if j := strings.IndexByte(p, '='); j >= 0 {
			added = append(added, snake+p[j:])
		} else {
			added = append(added, snake)
		}
	}
	if len(added) == 0 {
		return
	}
	r.URL.RawQuery = raw + "&" + strings.Join(added, "&")
	r.Form = nil // 防御：确保下游 ParseForm 读到改写后的 query
}

// addSnakeAlias 请求侧：camel 键对应的 snake 名在白名单内、且本层没有该 snake 键时，补上 snake 键。
func addSnakeAlias(obj map[string]json.RawMessage, k string) (string, bool) {
	snake := toSnake(k)
	if snake == k || !knownSnake[snake] {
		return "", false
	}
	if _, exists := obj[snake]; exists {
		return "", false
	}
	return snake, true
}

// ---------------------------------------------------------------- 响应

type respWriter struct {
	http.ResponseWriter
	status      int
	buf         bytes.Buffer
	buffering   bool // 已判定为 JSON，缓冲到结束再改写
	passthrough bool // 非 JSON：直接写底层
	decided     bool
	wroteHeader bool
}

func (w *respWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	// 响应头 X-Jsoncase: off：协议性 JSON（MCP JSON-RPC、工具的 JSON Schema 定义）键名不能被补别名，原样透传
	if strings.EqualFold(w.ResponseWriter.Header().Get(SkipHeader), "off") {
		w.ResponseWriter.Header().Del(SkipHeader)
		w.passthrough = true
		return
	}
	ct := w.ResponseWriter.Header().Get("Content-Type")
	if isJSON(ct) {
		w.buffering = true
	} else {
		w.passthrough = true
	}
}

func (w *respWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.decide()
	if w.passthrough {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *respWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.passthrough {
		return w.ResponseWriter.Write(p)
	}
	return w.buf.Write(p)
}

func (w *respWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok && w.passthrough {
		f.Flush()
	}
}

// finish 在 handler 结束后调用：把缓冲的 JSON 改写后写出。
func (w *respWriter) finish() {
	if !w.buffering {
		return
	}
	body := w.buf.Bytes()
	if len(body) <= maxRespBytes {
		if b, changed := rewriteTree(body, addCamelAlias); changed {
			body = b
		}
	}
	w.ResponseWriter.Header().Del("Content-Length")
	w.ResponseWriter.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}

// addCamelAlias 响应侧：snake 键补 camel 别名（原键保留，本层已有该 camel 键时不补）。
func addCamelAlias(obj map[string]json.RawMessage, k string) (string, bool) {
	if !strings.Contains(k, "_") {
		return "", false
	}
	camel := toCamel(k)
	if camel == k {
		return "", false
	}
	if _, exists := obj[camel]; exists {
		return "", false
	}
	return camel, true
}

// ---------------------------------------------------------------- 改写树

// aliasFunc 判断对象 obj 的键 k 是否要补别名，返回别名。
type aliasFunc func(obj map[string]json.RawMessage, k string) (string, bool)

// rewriteTree 逐层改写 JSON：对象的每个键先递归改写其值，再按 alias 补别名（别名与原键共享改写后的值）；
// 自由格式键（freeFormKeys，按键名或其 snake 形式匹配）的值原样保留字节、不下钻——画布快照、工具参数等
// 调用方私有 JSON 里的键不是接口字段。返回是否改动；未改动时调用方应沿用原始字节。
func rewriteTree(raw []byte, alias aliasFunc) ([]byte, bool) {
	v, changed, ok := rewriteValue(raw, alias)
	if !ok || !changed {
		return raw, false
	}
	return v, true
}

func rewriteValue(raw json.RawMessage, alias aliasFunc) (json.RawMessage, bool, bool) {
	switch firstByte(raw) {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return raw, false, false
		}
		changed := false
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys) // 别名冲突时结果确定
		for _, k := range keys {
			if !isFreeForm(k) {
				if nv, c, ok := rewriteValue(obj[k], alias); ok && c {
					obj[k] = nv
					changed = true
				}
			}
			if name, add := alias(obj, k); add {
				obj[name] = obj[k]
				changed = true
			}
		}
		if !changed {
			return raw, false, true
		}
		b, err := marshalNoEscape(obj)
		if err != nil {
			return raw, false, false
		}
		return b, true, true
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return raw, false, false
		}
		changed := false
		for i := range arr {
			if nv, c, ok := rewriteValue(arr[i], alias); ok && c {
				arr[i] = nv
				changed = true
			}
		}
		if !changed {
			return raw, false, true
		}
		b, err := marshalNoEscape(arr)
		if err != nil {
			return raw, false, false
		}
		return b, true, true
	default:
		// 标量：原样；非法 JSON 由上层 Unmarshal 报错兜住
		return raw, false, json.Valid(raw)
	}
}

func isFreeForm(k string) bool {
	return freeFormKeys[k] || freeFormKeys[toSnake(k)]
}

func firstByte(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return c
	}
	return 0
}

// marshalNoEscape 不做 HTML 转义：未改动的子树（含自由格式值）按原字节输出，只去空白。
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ---------------------------------------------------------------- 工具

func isJSON(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "+json")
}

// toSnake camelCase → snake_case（只处理 ASCII 字母与数字，其余原样）。
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 'a' - 'A')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// toCamel snake_case → camelCase。
func toCamel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// verbatimPrefixes 原样放行的路径前缀（请求体不补 snake 别名、响应不补 camel 别名）：
//   - /api/v1/plaza/：响应由 pkg/v1wire 手写（camelCase、与 Java CommonResult 同形），
//     不能再被补别名键（DESIGN 1.4：给 jsoncase 加一个 verbatim 前缀白名单）；
//   - /api/v1/canvas/agent/：Function Call 定义与执行——工具名、入参键、JSON Schema 都是协议本身，
//     入参校验是 additionalProperties:false，补出来的 snake 别名会让合法调用被判「多余字段」；
//
// 后台前缀 /api/v1/plaza/admin 除外：后台沿用 Go 默认信封与双写别名（前端有现成适配）。
var verbatimPrefixes = []string{"/api/v1/plaza/", "/api/v1/canvas/agent/"}

// verbatimExact 原样放行的精确路径（含其子路径）：MCP JSON-RPC 端点，请求 params / 响应 result 键名都由 MCP 规范定死。
var verbatimExact = []string{"/api/v1/canvas/mcp"}

var verbatimExcluded = []string{"/api/v1/plaza/admin"}

func verbatim(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	return isVerbatimPath(r.URL.Path)
}

func isVerbatimPath(p string) bool {
	for _, ex := range verbatimExcluded {
		if p == ex || strings.HasPrefix(p, ex+"/") {
			return false
		}
	}
	for _, e := range verbatimExact {
		if p == e || strings.HasPrefix(p, e+"/") {
			return true
		}
	}
	for _, pre := range verbatimPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}
