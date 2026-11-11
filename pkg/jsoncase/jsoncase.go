// Package jsoncase 让 Go 网关 / admin 的 JSON 字段同时支持 snake_case 与 Java 一致的 camelCase。
//
// 迁移期（增量双写，不破坏现有调用方）：
//   - 响应：每个 snake_case 键补一个 camelCase 别名，原键保留；
//   - 请求体：camelCase 键仅当它对应的 snake_case 名在白名单（known_gen.go）中、且请求里没有该 snake 键时，补上 snake 键；
//     白名单之外的键（如用户自由格式 map 里的键）一律不动。
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
	"strconv"
	"strings"
)

const (
	maxReqBytes  = 1 << 20 // 请求体超过 1MB 不改写
	maxRespBytes = 4 << 20 // 响应体超过 4MB 不改写
)

// Middleware 可直接传给 rest.Server.Use。
func Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	if v, ok := decode(raw); ok {
		if addSnakeAliases(v) {
			if b, err := json.Marshal(v); err == nil {
				out = b
			}
		}
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

// addSnakeAliases 递归：白名单内的 snake 键缺失且存在对应 camel 键时补上 snake 键。返回是否改动。
func addSnakeAliases(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if changed2 := addSnakeAliases(child); changed2 {
				changed = true
			}
			snake := toSnake(k)
			if snake == k || !knownSnake[snake] {
				continue
			}
			if _, exists := t[snake]; exists {
				continue
			}
			t[snake] = child
			changed = true
		}
	case []any:
		for _, child := range t {
			if addSnakeAliases(child) {
				changed = true
			}
		}
	}
	return changed
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
		if v, ok := decode(body); ok && addCamelAliases(v) {
			if b, err := json.Marshal(v); err == nil {
				body = b
			}
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

// addCamelAliases 递归：snake 键补 camel 别名（原键保留）。返回是否改动。
func addCamelAliases(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if addCamelAliases(child) {
				changed = true
			}
			if !strings.Contains(k, "_") {
				continue
			}
			camel := toCamel(k)
			if camel == k {
				continue
			}
			if _, exists := t[camel]; exists {
				continue
			}
			t[camel] = child
			changed = true
		}
	case []any:
		for _, child := range t {
			if addCamelAliases(child) {
				changed = true
			}
		}
	}
	return changed
}

// ---------------------------------------------------------------- 工具

func isJSON(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "+json")
}

func decode(raw []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
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
