package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// 广场模板 data_json 里的元数据键：不属于 v1 的 payload（下发时摘掉）。
// 与 Java CreaftV1SupportController.PLAZA_META_KEYS 一致；authorName 在 payload 里也要摘（下发在外层）。
var plazaMetaKeys = map[string]bool{"agentSkillId": true, "agent_skill_id": true, "v1PlazaTemplateId": true, "authorName": true}

// 后台表单的元数据键（AdminCreaftTemplateSupport.PLAZA_META_KEYS）：不含 authorName，它留在载荷里由运营直接编辑。
var adminMetaKeys = map[string]bool{"agentSkillId": true, "agent_skill_id": true, "v1PlazaTemplateId": true}

const (
	keySkill = "agentSkillId"
	keyV1ID  = "v1PlazaTemplateId"
)

// kv 有序对象的一项：值保持原始 JSON 字节（不经 map[string]any，所以键序与大整数精度都不丢）。
type kv struct {
	key string
	val json.RawMessage
}

// parseObject 按出现顺序读出顶层对象。不是对象（或为空）返回 ok=false。
func parseObject(raw []byte) (items []kv, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, isStr := kt.(string)
		if !isStr {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		items = append(items, kv{key, v})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	// 尾随内容也算不合法（MySQL 的 JSON 列同样不收）
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return items, true
}

func writeObject(items []kv) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(it.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(it.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// stripTopKeys 摘除顶层若干键，其余键与值原样、顺序不变。非对象返回 {}。
func stripTopKeys(raw []byte, drop map[string]bool) json.RawMessage {
	items, ok := parseObject(raw)
	if !ok {
		return json.RawMessage(`{}`)
	}
	out := items[:0:0]
	for _, it := range items {
		if drop[it.key] {
			continue
		}
		out = append(out, it)
	}
	return writeObject(out)
}

// topValue 取顶层键的原始值（重复键取最后一个，同 Jackson）。
func topValue(raw []byte, key string) (json.RawMessage, bool) {
	items, ok := parseObject(raw)
	if !ok {
		return nil, false
	}
	var found json.RawMessage
	var has bool
	for _, it := range items {
		if it.key == key {
			found, has = it.val, true
		}
	}
	return found, has
}

// topString 顶层键的字符串形态：字符串取其值，数字取字面量（雪花 id 不经 float64），null / 缺失为 ""。
func topString(raw []byte, key string) string {
	v, ok := topValue(raw, key)
	if !ok {
		return ""
	}
	return scalarString(v)
}

func scalarString(v json.RawMessage) string {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return ""
	}
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	if v[0] == '{' || v[0] == '[' {
		return ""
	}
	return string(v)
}

// skillID 模板绑定的技能 id（字符串；agentSkillId 优先，其次 agent_skill_id）。
func skillID(data []byte) string {
	if s := topString(data, keySkill); s != "" {
		return s
	}
	return topString(data, "agent_skill_id")
}

// sanitizePayload 用户端下发的 payload：摘除顶层 prompt 与元数据键，其余原样（照 Java plazaTemplate）。
func sanitizePayload(data []byte) json.RawMessage {
	drop := map[string]bool{"prompt": true}
	for k := range plazaMetaKeys {
		drop[k] = true
	}
	return stripTopKeys(data, drop)
}

// payloadForDisplay 后台表单回显的载荷：去掉元数据键（保留 prompt 与 authorName，运营要编辑）。
func payloadForDisplay(data []byte) json.RawMessage { return stripTopKeys(data, adminMetaKeys) }

// requireJSONObject 载荷必须是 JSON 对象：空 -> {}；数组 / 标量 / 带尾随内容 -> 错误。
func requireJSONObject(payload []byte) (json.RawMessage, error) {
	p := bytes.TrimSpace(payload)
	if len(p) == 0 || string(p) == "null" {
		return json.RawMessage(`{}`), nil
	}
	if _, ok := parseObject(p); !ok {
		return nil, errParam("载荷必须是 JSON 对象")
	}
	return json.RawMessage(p), nil
}

// composeData 合成 data_json：载荷（已校验为对象，元数据键被忽略）+ 原有的 v1PlazaTemplateId + 技能绑定。
// 复刻 Java plazaDataJson：编辑是整体替换，必须保留 v1PlazaTemplateId，否则导入脚本按它判重时会再插一遍。
// Agent 运行时读取的 prompt / guidance 随载荷保留（Java 契约第 2 条）。
func composeData(payload []byte, agentSkillID string, existing []byte) (json.RawMessage, error) {
	obj, err := requireJSONObject(payload)
	if err != nil {
		return nil, err
	}
	items, _ := parseObject(obj)
	out := make([]kv, 0, len(items)+2)
	for _, it := range items {
		if adminMetaKeys[it.key] {
			continue
		}
		out = append(out, it)
	}
	if v1, ok := topValue(existing, keyV1ID); ok {
		out = append(out, kv{keyV1ID, v1})
	}
	if agentSkillID != "" {
		b, _ := json.Marshal(agentSkillID)
		out = append(out, kv{keySkill, b})
	}
	return writeObject(out), nil
}

// readTags 读 tags_json（{"tags":[…]}）：非对象、缺键、值不是数组都按「没有标签」。
func readTags(raw []byte) []string {
	v, ok := topValue(raw, "tags")
	if !ok {
		return []string{}
	}
	var arr []any
	if json.Unmarshal(v, &arr) != nil {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		s := strings.TrimSpace(fmt.Sprint(a))
		if s != "" && a != nil {
			out = append(out, s)
		}
	}
	return out
}

// writeTags 写 tags_json：去空、去首尾空白、去重（保序）。
func writeTags(tags []string) json.RawMessage {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	b, _ := json.Marshal(map[string][]string{"tags": out})
	return b
}

// firstResult 封面：data_json.result[0]，缺则 image_urls[0]。
func firstResult(data []byte) string {
	for _, key := range []string{"result", "image_urls"} {
		v, ok := topValue(data, key)
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(v, &arr) == nil && len(arr) > 0 {
			if s := scalarString(arr[0]); s != "" {
				return s
			}
		}
	}
	return ""
}

// jsonStrings 读 JSON 字符串数组（非数组 / 空 → 空切片，保证响应里是 [] 而不是 null）。
func jsonStrings(raw []byte) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	var arr []string
	if json.Unmarshal(raw, &arr) != nil {
		return out
	}
	for _, s := range arr {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
