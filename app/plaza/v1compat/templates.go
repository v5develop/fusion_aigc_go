package v1compat

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strconv"

	"aigc-local-life/pkg/v1wire"

	"github.com/zeromicro/go-zero/core/logx"
)

// Java ErrorCode（common/ErrorCode.java）里用到的几个码。
const (
	codeParamInvalid = 1003
	codeSystemError  = 5001
	codeDatabase     = 5002
)

// errSystem Java 的兜底处理器（GlobalExceptionHandler#handleException）：HTTP 500 + 5001「系统内部错误」。
// 库访问异常、data_json 是 JSON null（NPE）都落在这里。
func errSystem() *v1wire.JavaFailure {
	return &v1wire.JavaFailure{Status: http.StatusInternalServerError, Code: codeSystemError, Reason: "SYSTEM_ERROR", Msg: "系统内部错误"}
}

// errCorrupt CompatSupportService#data 读 JSON 失败：BusinessException(DATABASE_ERROR, "兼容数据损坏")，5xxx → HTTP 500。
func errCorrupt() *v1wire.JavaFailure {
	return &v1wire.JavaFailure{Status: http.StatusInternalServerError, Code: codeDatabase, Reason: "DATABASE_ERROR", Msg: "兼容数据损坏"}
}

// errTypeMismatch 查询参数转 long 失败（GlobalExceptionHandler#handleTypeMismatch）：HTTP 400 + 1003 + 逐字段明细。
func errTypeMismatch(name string) *v1wire.JavaFailure {
	msg := "参数 " + name + " 格式不正确"
	return &v1wire.JavaFailure{Status: http.StatusBadRequest, Code: codeParamInvalid, Reason: "PARAM_INVALID", Msg: msg,
		Data: v1wire.FieldViolationData(name, "INVALID_TYPE", msg)}
}

// errPageRange 有意偏差：Java 在 subList 越界时抛 IndexOutOfBounds / IllegalArgument（HTTP 500「系统内部错误」），
// 那是 Java 的缺陷不是契约；Go 回 HTTP 400 + PARAM_INVALID（DESIGN 9.1「分页」）。diff 语料不含这类请求。
func errPageRange(field string) *v1wire.JavaFailure {
	msg := "参数 " + field + " 超出范围"
	return &v1wire.JavaFailure{Status: http.StatusBadRequest, Code: codeParamInvalid, Reason: "PARAM_INVALID", Msg: msg,
		Data: v1wire.FieldViolationData(field, "OUT_OF_RANGE", msg)}
}

// supportData 复刻 CompatSupportService#data：列为 NULL → 空对象；JSON null → Java 返回 null，随后取键 NPE（5001）；
// 不是对象 → 「兼容数据损坏」（5002）。
func supportData(raw []byte) (v1wire.JValue, *v1wire.JavaFailure) {
	if raw == nil {
		return v1wire.JValue{Kind: v1wire.JObject}, nil
	}
	v, err := v1wire.ParseJava(raw)
	if err != nil {
		return v1wire.JValue{}, errCorrupt()
	}
	switch v.Kind {
	case v1wire.JObject:
		return v, nil
	case v1wire.JNull:
		return v1wire.JValue{}, errSystem()
	}
	return v1wire.JValue{}, errCorrupt()
}

// listTags CreaftV1SupportController#tags：tags_json.tags 是数组则逐个 String.valueOf（null → "null"，不过滤），否则空。
func listTags(raw []byte) ([]string, *v1wire.JavaFailure) {
	d, f := supportData(raw)
	if f != nil {
		return nil, f
	}
	v, ok := d.Get("tags")
	if !ok || v.Kind != v1wire.JArray {
		return []string{}, nil
	}
	out := make([]string, len(v.Arr))
	for i, e := range v.Arr {
		out[i] = v1wire.JavaStringOf(e)
	}
	return out, nil
}

// 广场 data_json 里不属于 v1 payload 的键（PLAZA_META_KEYS + prompt）。
var plazaHidden = map[string]bool{"agentSkillId": true, "agent_skill_id": true, "v1PlazaTemplateId": true, "authorName": true, "prompt": true}

// skillOf data.containsKey("agentSkillId") ? get("agentSkillId") : get("agent_skill_id")；null → JSON null，否则 String.valueOf。
func skillOf(data v1wire.JValue) []byte {
	v, ok := data.Get("agentSkillId")
	if !ok {
		v, ok = data.Get("agent_skill_id")
	}
	if !ok || v.Kind == v1wire.JNull {
		return []byte("null")
	}
	return v1wire.AppendJacksonString(nil, v1wire.JavaStringOf(v))
}

func appendStrings(b []byte, ss []string) []byte {
	b = append(b, '[')
	for i, s := range ss {
		if i > 0 {
			b = append(b, ',')
		}
		b = v1wire.AppendJacksonString(b, s)
	}
	return append(b, ']')
}

func appendNullableString(b []byte, s sql.NullString) []byte {
	if !s.Valid {
		return append(b, "null"...)
	}
	return v1wire.AppendJacksonString(b, s.String)
}

// listItem plazaListItem：{payload, plazaTemplateId, title, agentSkillId, tags, authorName, usageCount}（键序同 Java LinkedHashMap）。
func listItem(b []byte, t TemplateRow, u *usageSnapshot) ([]byte, *v1wire.JavaFailure) {
	data, f := supportData(t.DataJSON)
	if f != nil {
		return nil, f
	}
	tags, f := listTags(t.TagsJSON)
	if f != nil {
		return nil, f
	}
	author := ""
	if a, ok := data.Get("authorName"); ok && a.Kind != v1wire.JNull {
		author = v1wire.JavaTrim(v1wire.JavaStringOf(a))
	}
	if author == "" {
		author = "官方"
	}
	b = append(b, `{"payload":`...)
	b = v1wire.AppendJackson(b, data.Without(plazaHidden))
	b = append(b, `,"plazaTemplateId":`...)
	b = strconv.AppendInt(b, t.ID, 10)
	b = append(b, `,"title":`...)
	b = appendNullableString(b, t.Title)
	b = append(b, `,"agentSkillId":`...)
	b = append(b, skillOf(data)...)
	b = append(b, `,"tags":`...)
	b = appendStrings(b, tags)
	b = append(b, `,"authorName":`...)
	b = v1wire.AppendJacksonString(b, author)
	b = append(b, `,"usageCount":`...)
	b = strconv.AppendInt(b, u.of(t.ID).uses, 10)
	return append(b, '}'), nil
}

// pageSlice 复刻 CompatSupportService.java:222-223 的内存分页（Java long 运算，Go int64 溢出回绕方式相同）。
// ok=false 表示 Java 的 subList 会抛异常（有意偏差：Go 回 PARAM_INVALID）。
func pageSlice(page, size int64, n int) (from, to int64, current, pages int64, field string, ok bool) {
	total := int64(n)
	from = min((page-1)*size, total)
	to = min(from+size, total)
	if from < 0 || to < 0 || from > to {
		if page < 1 {
			field = "page"
		} else {
			field = "size"
		}
		return 0, 0, 0, 0, field, false
	}
	current = 1 // MyBatis-Plus Page(current, size, total)：current > 1 才采纳
	if page > 1 {
		current = page
	}
	if size != 0 { // IPage#getPages：size=0 → 0，否则向上取整
		pages = total / size
		if total%size != 0 {
			pages++
		}
	}
	return from, to, current, pages, "", true
}

// List GET /api/v1/plaza-templates/list。tags 为 nil / 空表示不过滤；多个取并集（命中任一）。
func (m *Module) List(ctx context.Context, page, size int64, tags []string) ([]byte, *v1wire.JavaFailure) {
	u := m.usage.snapshot(ctx) // Java 先取统计快照再查模板
	rows, err := m.src.EnabledTemplates(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza v1compat: 查询广场模板失败: %v", err)
		return nil, errSystem()
	}
	if len(tags) > 0 {
		want := make(map[string]bool, len(tags))
		for _, t := range tags {
			want[t] = true
		}
		kept := rows[:0:0]
		for _, r := range rows {
			d, f := supportData(r.TagsJSON)
			if f != nil {
				return nil, f
			}
			v, ok := d.Get("tags")
			if !ok || v.Kind != v1wire.JArray {
				continue
			}
			for _, e := range v.Arr {
				if want[v1wire.JavaStringOf(e)] {
					kept = append(kept, r)
					break
				}
			}
		}
		rows = kept
	}
	from, to, current, pages, field, ok := pageSlice(page, size, len(rows))
	if !ok {
		return nil, errPageRange(field)
	}
	b := []byte(`{"current":`)
	b = strconv.AppendInt(b, current, 10)
	b = append(b, `,"pages":`...)
	b = strconv.AppendInt(b, pages, 10)
	b = append(b, `,"total":`...)
	b = strconv.AppendInt(b, int64(len(rows)), 10)
	b = append(b, `,"data":[`...)
	for i, r := range rows[from:to] {
		if i > 0 {
			b = append(b, ',')
		}
		var f *v1wire.JavaFailure
		if b, f = listItem(b, r, u); f != nil {
			return nil, f
		}
	}
	return append(b, "]}"...), nil
}

// tagsScanLimit 标签清单只扫前 1000 条启用模板（plazaTemplates(null, 1, 1000)），超出部分的标签会缺——Java 原样。
const tagsScanLimit = 1000

// Tags GET /api/v1/plaza-templates/tags：前 1000 条模板的标签，去重（保留首次出现）后按 String.compareTo（UTF-16 码元序）排序。
func (m *Module) Tags(ctx context.Context) ([]byte, *v1wire.JavaFailure) {
	rows, err := m.src.EnabledTemplates(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza v1compat: 查询广场模板失败: %v", err)
		return nil, errSystem()
	}
	if len(rows) > tagsScanLimit {
		rows = rows[:tagsScanLimit]
	}
	seen := map[string]bool{}
	var all []string
	for _, r := range rows {
		tags, f := listTags(r.TagsJSON)
		if f != nil {
			return nil, f
		}
		for _, t := range tags {
			if !seen[t] {
				seen[t] = true
				all = append(all, t)
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return v1wire.CompareUTF16(all[i], all[j]) < 0 })
	return appendStrings(nil, all), nil
}
