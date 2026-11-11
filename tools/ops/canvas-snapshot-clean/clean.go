package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"aigc-local-life/pkg/jsoncase"
)

// removal 删掉的一个下划线副本。
type removal struct {
	Path    string // JSONPath，如 $.nodes[0].data.run_id
	Twin    string // 同层保留的 camel 键，如 runId
	Differs bool   // 删掉的值与 camel 值不同（典型是副本落库后 camel 又更新过，副本已陈旧）
}

// pollutedKey 对象 obj 的键 k 是否是 jsoncase 修复前补出来的下划线副本：
//
//	(a) k 在 jsoncase 白名单里（中间件只给白名单内的名字补 snake）；
//	(b) 同层还有它的 camel 孪生键（toCamel 与中间件同一函数）；
//	(c) 该 camel 键经 toSnake 正好得到 k——中间件是从 camel 键算 snake 名的，反推不回 k 的组合（如 image_2k / _id / a__b）
//	    不可能是它补的。
//
// 没有 camel 孪生的下划线键一律不动。
func pollutedKey(obj map[string]any, k string) (camel string, ok bool) {
	if !jsoncase.IsKnownSnake(k) {
		return "", false
	}
	camel = jsoncase.ToCamel(k)
	if camel == k || jsoncase.ToSnake(camel) != k {
		return "", false
	}
	if _, has := obj[camel]; !has {
		return "", false
	}
	return camel, true
}

// cleanSnapshot 遍历整棵 JSON 树，删掉每个对象里的下划线副本。没有可删的返回原字节（changed=false，不重写）；
// 有删除时重新序列化：数字按原文（UseNumber，大整数不丢精度）、不做 HTML 转义；对象键序按 Go 的排序输出——
// 落库的是 MySQL JSON 列，键序本来就由 MySQL 规范化，语义不变。
func cleanSnapshot(raw []byte) (out []byte, removed []removal, err error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return raw, nil, err
	}
	v, removed = walk(v, "$")
	if len(removed) == 0 {
		return raw, nil, nil
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].Path < removed[j].Path })
	b, err := encodeJSON(v)
	if err != nil {
		return raw, nil, err
	}
	return b, removed, nil
}

// walk 先递归清理每个子值，再删本层的副本。将被删掉的副本内部的嵌套删除不计入报告（整个副本都会被删掉）。
func walk(v any, path string) (any, []removal) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		drop := map[string]string{} // 下划线副本 → camel 孪生
		for _, k := range keys {
			if camel, ok := pollutedKey(t, k); ok {
				drop[k] = camel
			}
		}
		var out []removal
		for _, k := range keys {
			nv, rs := walk(t[k], childPath(path, k))
			t[k] = nv
			if _, gone := drop[k]; !gone {
				out = append(out, rs...)
			}
		}
		for _, k := range keys {
			camel, gone := drop[k]
			if !gone {
				continue
			}
			out = append(out, removal{Path: childPath(path, k), Twin: camel, Differs: !sameJSON(t[k], t[camel])})
			delete(t, k)
		}
		return t, out
	case []any:
		var out []removal
		for i := range t {
			nv, rs := walk(t[i], path+"["+strconv.Itoa(i)+"]")
			t[i] = nv
			out = append(out, rs...)
		}
		return t, out
	default:
		return v, nil
	}
}

// childPath 对象子键的 JSONPath：标识符样式的用 .key，其余用 ["key"]。
func childPath(parent, k string) string {
	if isIdent(k) {
		return parent + "." + k
	}
	q, _ := json.Marshal(k)
	return parent + "[" + string(q) + "]"
}

func isIdent(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// sameJSON 两个值序列化后是否相同（对象键已排序；数字按原文比较，1 与 1.0 视为不同）。
func sameJSON(a, b any) bool {
	x, err1 := encodeJSON(a)
	y, err2 := encodeJSON(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// decodeJSON 解出单个 JSON 值（UseNumber）；尾部还有内容视为非法。
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("快照不是合法 JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("快照不是合法 JSON: 末尾有多余内容")
	}
	return v, nil
}

// encodeJSON 紧凑输出、不做 HTML 转义（< > & 原样），json.Number 按原文输出。
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
