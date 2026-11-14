package v1compat

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"aigc-local-life/pkg/v1wire"

	"github.com/zeromicro/go-zero/core/logx"
)

// v1 头条是永久的 legacy 实现（DESIGN 14.8）：逐字复刻 Java PlazaHeadlineService#listCurrent 的现算逻辑——
// 读 afv_plaza_headline 全部规则（不分范围 / 行业，忽略 Go 侧的抑制、定位、快照），按模板去重后合并，按 Java 的顺序规则排序。

// 头条 payload 摘除的键：注意不含 authorName（PlazaHeadlineService.PAYLOAD_HIDDEN_KEYS 与列表的 PLAZA_META_KEYS 不同，Java 原样）。
var headlineHidden = map[string]bool{"agentSkillId": true, "agent_skill_id": true, "v1PlazaTemplateId": true, "prompt": true}

// readObjectLenient PlazaHeadlineService#readObject：空白 / 不合法 / 非对象 / JSON null 一律当空对象（不报错）。
func readObjectLenient(raw []byte) v1wire.JValue {
	empty := v1wire.JValue{Kind: v1wire.JObject}
	if !v1wire.HasText(string(raw)) {
		return empty
	}
	v, err := v1wire.ParseJava(raw)
	if err != nil || v.Kind != v1wire.JObject {
		return empty
	}
	return v
}

// headlineTags PlazaHeadlineService#readTags：去 null、String.valueOf、去掉没有文字的（hasText），不 trim。
func headlineTags(raw []byte) []string {
	v, ok := readObjectLenient(raw).Get("tags")
	out := []string{}
	if !ok || v.Kind != v1wire.JArray {
		return out
	}
	for _, e := range v.Arr {
		if e.Kind == v1wire.JNull {
			continue
		}
		if s := v1wire.JavaStringOf(e); v1wire.HasText(s) {
			out = append(out, s)
		}
	}
	return out
}

func sortOf(h HeadlineRow) int64 {
	if !h.SortOrder.Valid {
		return 0
	}
	return int64(int32(h.SortOrder.Int64)) // Integer
}

// dedupeByTemplate 同一模板多条头条同时生效：起始日最晚的优先，再按 sort 升序、id 降序（PlazaHeadlineService.java:127-137）。
func dedupeByTemplate(rows []HeadlineRow) []HeadlineRow {
	cand := make([]HeadlineRow, 0, len(rows))
	for _, r := range rows {
		if r.PlazaTemplateID.Valid {
			cand = append(cand, r)
		}
	}
	sort.SliceStable(cand, func(i, j int) bool {
		a, b := cand[i], cand[j]
		if a.StartDate.Valid != b.StartDate.Valid { // nullsLast
			return a.StartDate.Valid
		}
		if a.StartDate.String != b.StartDate.String {
			return a.StartDate.String > b.StartDate.String // yyyy-MM-dd 字典序即日期序；降序
		}
		if sortOf(a) != sortOf(b) {
			return sortOf(a) < sortOf(b)
		}
		return a.ID > b.ID
	})
	seen := map[int64]bool{}
	out := cand[:0:0]
	for _, r := range cand {
		if !seen[r.PlazaTemplateID.Int64] {
			seen[r.PlazaTemplateID.Int64] = true
			out = append(out, r)
		}
	}
	return out
}

type headlineCandidate struct {
	h    HeadlineRow
	t    TemplateRow
	tags []string
	data v1wire.JValue
	u    usage
}

// Headlines GET /api/v1/plaza-headlines（tag / keyword 均可选；未知参数忽略，不接受 industry）。
func (m *Module) Headlines(ctx context.Context, tag, keyword string, tagSet, keywordSet bool) ([]byte, *v1wire.JavaFailure) {
	today := m.now().In(shanghai).Format("2006-01-02")
	active, err := m.src.ActiveHeadlines(ctx, today)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza v1compat: 查询头条失败: %v", err)
		return nil, errSystem()
	}
	snap := m.usage.snapshot(ctx)
	head := func(b []byte) []byte {
		b = append(b, `{"statsFrom":`...)
		b = v1wire.AppendJacksonString(b, snap.startDate())
		b = append(b, `,"statsTo":`...)
		b = v1wire.AppendJacksonString(b, snap.endDate())
		return append(b, `,"items":[`...)
	}
	if len(active) == 0 {
		return append(head(nil), "]}"...), nil
	}
	per := dedupeByTemplate(active)
	ids := make([]int64, len(per))
	for i, h := range per {
		ids[i] = h.PlazaTemplateID.Int64
	}
	byID := map[int64]TemplateRow{}
	if len(ids) > 0 {
		tpls, err := m.src.TemplatesByID(ctx, ids)
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza v1compat: 查询头条模板失败: %v", err)
			return nil, errSystem()
		}
		for _, t := range tpls {
			if _, dup := byID[t.ID]; !dup {
				byID[t.ID] = t
			}
		}
	}
	wantedTag, hasTag := "", tagSet && v1wire.HasText(tag)
	if hasTag {
		wantedTag = v1wire.JavaTrim(tag)
	}
	needle, hasNeedle := "", keywordSet && v1wire.HasText(keyword)
	if hasNeedle {
		needle = v1wire.JavaLowerRoot(v1wire.JavaTrim(keyword))
	}
	var cands []headlineCandidate
	for _, h := range per {
		t, ok := byID[h.PlazaTemplateID.Int64]
		if !ok {
			continue // 模板停用 / 删除：头条跟着消失
		}
		tags := headlineTags(t.TagsJSON)
		if hasTag && !containsStr(tags, wantedTag) {
			continue
		}
		data := readObjectLenient(t.DataJSON)
		if hasNeedle && !strings.Contains(searchText(t, tags, data, h.Why.String), needle) {
			continue
		}
		cands = append(cands, headlineCandidate{h: h, t: t, tags: tags, data: data, u: snap.of(t.ID)})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.u.uses != b.u.uses {
			return a.u.uses > b.u.uses
		}
		if sortOf(a.h) != sortOf(b.h) {
			return sortOf(a.h) < sortOf(b.h)
		}
		return a.h.ID < b.h.ID
	})
	b := head(nil)
	for i, c := range cands {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"headlineId":`...)
		b = strconv.AppendInt(b, c.h.ID, 10)
		b = append(b, `,"rank":`...)
		b = strconv.AppendInt(b, int64(i+1), 10)
		b = append(b, `,"plazaTemplateId":`...)
		b = strconv.AppendInt(b, c.t.ID, 10)
		b = append(b, `,"title":`...)
		b = appendNullableString(b, c.t.Title)
		b = append(b, `,"agentSkillId":`...)
		b = append(b, skillOf(c.data)...)
		b = append(b, `,"tags":`...)
		b = appendStrings(b, c.tags)
		b = append(b, `,"payload":`...)
		b = v1wire.AppendJackson(b, c.data.Without(headlineHidden))
		b = append(b, `,"why":`...)
		b = appendNullableString(b, c.h.Why)
		b = append(b, `,"uses":`...)
		b = strconv.AppendInt(b, c.u.uses, 10)
		b = append(b, `,"previousUses":`...)
		b = strconv.AppendInt(b, c.u.previous, 10)
		b = append(b, `,"trend":`...)
		if tr := c.u.trend(); tr == nil {
			b = append(b, "null"...)
		} else {
			b = strconv.AppendInt(b, *tr, 10)
		}
		b = append(b, `,"isNew":`...)
		b = strconv.AppendBool(b, c.u.previous == 0)
		b = append(b, `,"startDate":`...)
		b = appendNullableString(b, c.h.StartDate)
		b = append(b, `,"endDate":`...)
		b = appendNullableString(b, c.h.EndDate)
		b = append(b, '}')
	}
	return append(b, "]}"...), nil
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// searchText 标题 / 说明列 / payload.description（仅字符串）/ 标签 / 推荐语，空格拼接后小写（Locale.ROOT）。
func searchText(t TemplateRow, tags []string, data v1wire.JValue, why string) string {
	desc := ""
	if d, ok := data.Get("description"); ok && d.Kind == v1wire.JString {
		desc = d.Str
	}
	return v1wire.JavaLowerRoot(strings.Join([]string{t.Title.String, t.Description.String, desc, strings.Join(tags, " "), why}, " "))
}
