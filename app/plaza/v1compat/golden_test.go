package v1compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/v1wire"
)

// 黄金样本比对：testdata/java_golden.json 由 tools/plazav1diff/javagolden/GoldenGen.java 生成——
// 用 fusion-aigc 编译产物里的真实 Java 类（CreaftV1SupportController / CompatSupportService / PlazaHeadlineService /
// PlazaTemplateUsageStatsService / CommonResult）+ Jackson 2.17.3 跑 testdata/fixture.json 的同一份数据。
// 这里用同一份 fixture 喂 Go 实现，要求：HTTP 状态相同、规范化 JSON（键排序、数字按字面量）相同、每个 payload 子树逐字节相同。
// 唯一例外是登记过的有意偏差（Java subList 越界 500 → Go 400 PARAM_INVALID）。

type fixtureTemplate struct {
	ID          int64   `json:"id"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	TagsJSON    *string `json:"tagsJson"`
	DataJSON    *string `json:"dataJson"`
	SortOrder   int64   `json:"sortOrder"`
	UpdateTime  string  `json:"updateTime"`
	Enabled     int     `json:"enabled"`
}

type fixtureHeadline struct {
	ID              int64   `json:"id"`
	PlazaTemplateID int64   `json:"plazaTemplateId"`
	StartOffset     int     `json:"startOffset"`
	EndOffset       int     `json:"endOffset"`
	Why             *string `json:"why"`
	SortOrder       int64   `json:"sortOrder"`
	Enabled         int     `json:"enabled"`
}

type fixtureCase struct {
	Name     string   `json:"name"`
	Endpoint string   `json:"endpoint"`
	Page     int64    `json:"page"`
	Size     int64    `json:"size"`
	Tags     []string `json:"tags"`
	Tag      *string  `json:"tag"`
	Keyword  *string  `json:"keyword"`
}

type fixtureDataset struct {
	Name      string            `json:"name"`
	Templates []fixtureTemplate `json:"templates"`
	Headlines []fixtureHeadline `json:"headlines"`
	Usage     []UseCount        `json:"usage"`
	Cases     []fixtureCase     `json:"cases"`
}

func (u *UseCount) UnmarshalJSON(b []byte) error {
	var x struct{ TemplateID, Uses, PreviousUses int64 }
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	*u = UseCount{x.TemplateID, x.Uses, x.PreviousUses}
	return nil
}

type goldenCase struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Status    int    `json:"status"`
	Exception string `json:"exception"`
	Body      string `json:"body"`
}

type golden struct {
	NowEpochMilli int64 `json:"nowEpochMilli"`
	Datasets      []struct {
		Name  string       `json:"name"`
		Cases []goldenCase `json:"cases"`
	} `json:"datasets"`
	JacksonRoundTrip []struct {
		In      string            `json:"in"`
		Out     string            `json:"out"`
		ValueOf map[string]string `json:"valueOf"`
	} `json:"jacksonRoundTrip"`
	SortInput  []string `json:"sortInput"`
	SortOutput []string `json:"sortOutput"`
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// fakeSource 按 SQL 口径（enabled / deleted / 排序 / 日期窗口）在内存里过滤 fixture。
type fakeSource struct {
	ds        fixtureDataset
	usageArgs [][3]time.Time
	failUsage bool
}

func strBytes(s *string) []byte {
	if s == nil {
		return nil
	}
	return []byte(*s)
}

func nullStr(s *string) (n sql.NullString) {
	if s != nil {
		n.String, n.Valid = *s, true
	}
	return
}

func (f *fakeSource) row(t fixtureTemplate) TemplateRow {
	return TemplateRow{ID: t.ID, Title: nullStr(t.Title), Description: nullStr(t.Description),
		TagsJSON: strBytes(t.TagsJSON), DataJSON: strBytes(t.DataJSON)}
}

func (f *fakeSource) EnabledTemplates(context.Context) ([]TemplateRow, error) {
	var ts []fixtureTemplate
	for _, t := range f.ds.Templates {
		if t.Enabled == 1 {
			ts = append(ts, t)
		}
	}
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		if a.UpdateTime != b.UpdateTime {
			return a.UpdateTime > b.UpdateTime
		}
		return a.ID > b.ID
	})
	out := make([]TemplateRow, len(ts))
	for i, t := range ts {
		out[i] = f.row(t)
	}
	return out, nil
}

func (f *fakeSource) TemplatesByID(_ context.Context, ids []int64) ([]TemplateRow, error) {
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []TemplateRow
	for _, t := range f.ds.Templates {
		if t.Enabled == 1 && want[t.ID] {
			out = append(out, f.row(t))
		}
	}
	return out, nil
}

func (f *fakeSource) ActiveHeadlines(_ context.Context, today string) ([]HeadlineRow, error) {
	d, _ := time.ParseInLocation("2006-01-02", today, shanghai)
	var out []HeadlineRow
	for _, h := range f.ds.Headlines {
		s, e := d.AddDate(0, 0, h.StartOffset), d.AddDate(0, 0, h.EndOffset)
		if h.Enabled != 1 || s.After(d) || e.Before(d) {
			continue
		}
		r := HeadlineRow{ID: h.ID, Why: nullStr(h.Why)}
		r.PlazaTemplateID.Int64, r.PlazaTemplateID.Valid = h.PlazaTemplateID, true
		r.StartDate.String, r.StartDate.Valid = s.Format("2006-01-02"), true
		r.EndDate.String, r.EndDate.Valid = e.Format("2006-01-02"), true
		r.SortOrder.Int64, r.SortOrder.Valid = h.SortOrder, true
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeSource) CountUses(_ context.Context, from, mid, to time.Time) ([]UseCount, error) {
	f.usageArgs = append(f.usageArgs, [3]time.Time{from, mid, to})
	if f.failUsage {
		return nil, context.DeadlineExceeded
	}
	return f.ds.Usage, nil
}

// canonical 规范化 JSON：对象键排序、数字保留字面量（1.0E10 与 10000000000 不相等）。
func canonical(t *testing.T, body string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("不是 JSON: %v\n%s", err, body)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// payloads 文档序收集所有 payload 子树的 Jackson 重写字节（检查键序与数字 / 字符串的逐字节一致）。
func payloads(t *testing.T, body string) []string {
	t.Helper()
	v, err := v1wire.ParseJava([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(v v1wire.JValue)
	walk = func(v v1wire.JValue) {
		for _, m := range v.Obj {
			if m.Key == "payload" {
				out = append(out, string(v1wire.AppendJackson(nil, m.Val)))
			}
			walk(m.Val)
		}
		for _, e := range v.Arr {
			walk(e)
		}
	}
	walk(v)
	return out
}

// payloadRaw 从原始响应文本里按出现顺序截出每个 "payload": 之后的对象原文（不经任何解析）。
func payloadRaw(body string) []string {
	var out []string
	rest := body
	for {
		i := strings.Index(rest, `"payload":`)
		if i < 0 {
			return out
		}
		rest = rest[i+len(`"payload":`):]
		depth, inStr, esc := 0, false, false
		for j := 0; j < len(rest); j++ {
			c := rest[j]
			switch {
			case esc:
				esc = false
			case c == '\\' && inStr:
				esc = true
			case c == '"':
				inStr = !inStr
			case inStr:
			case c == '{' || c == '[':
				depth++
			case c == '}' || c == ']':
				depth--
				if depth == 0 {
					out = append(out, rest[:j+1])
					rest = rest[j+1:]
					j = len(rest)
				}
			}
		}
	}
}

func run(m *Module, c fixtureCase) ([]byte, *v1wire.JavaFailure) {
	ctx := context.Background()
	switch c.Endpoint {
	case "list":
		return m.List(ctx, c.Page, c.Size, c.Tags)
	case "tags":
		return m.Tags(ctx)
	default:
		var tag, kw string
		if c.Tag != nil {
			tag = *c.Tag
		}
		if c.Keyword != nil {
			kw = *c.Keyword
		}
		return m.Headlines(ctx, tag, kw, c.Tag != nil, c.Keyword != nil)
	}
}

// envelope 用真实写出路径包信封，去掉 requestId / timestamp 后返回（状态、正文）。
func envelope(t *testing.T, data []byte, f *v1wire.JavaFailure) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if f != nil {
		v1wire.WriteJavaFail(rec, req, f)
	} else {
		v1wire.WriteJavaOK(rec, req, data)
	}
	raw := rec.Body.String()
	// Go 信封固定以 ,"requestId":…,"timestamp":…} 结尾：按原文截掉（不经 json.Marshal，免得 HTML 转义改了字节）
	i := strings.LastIndex(raw, `,"requestId":`)
	if i < 0 || !strings.Contains(raw[i:], `,"timestamp":`) {
		t.Fatalf("信封缺 requestId / timestamp: %s", raw)
	}
	return rec.Code, raw[:i] + "}"
}

// 有意偏差：Java subList 越界（IndexOutOfBounds / IllegalArgument）是 HTTP 500 SYSTEM_ERROR，Go 回 400 PARAM_INVALID。
func isPageRangeDeviation(g goldenCase) bool {
	return g.Endpoint == "list" && (g.Exception == "java.lang.IndexOutOfBoundsException" || g.Exception == "java.lang.IllegalArgumentException")
}

func TestGoldenAgainstJava(t *testing.T) {
	var fx struct {
		Datasets []fixtureDataset `json:"datasets"`
	}
	var g golden
	loadJSON(t, "testdata/fixture.json", &fx)
	loadJSON(t, "testdata/java_golden.json", &g)
	now := time.UnixMilli(g.NowEpochMilli)
	if len(fx.Datasets) != len(g.Datasets) {
		t.Fatalf("fixture 与 golden 数据集个数不一致，请重新生成 golden")
	}
	total := 0
	for _, ds := range fx.Datasets {
		total += len(ds.Cases)
	}
	for i, ds := range fx.Datasets {
		gd := g.Datasets[i]
		if gd.Name != ds.Name || len(gd.Cases) != len(ds.Cases) {
			t.Fatalf("数据集 %s 与 golden 不对应，请重新生成 golden", ds.Name)
		}
		m := New(&fakeSource{ds: ds}, func() time.Time { return now })
		for j, c := range ds.Cases {
			gc := gd.Cases[j]
			name := ds.Name + "/" + c.Name
			t.Run(name, func(t *testing.T) {
				data, f := run(m, c)
				status, body := envelope(t, data, f)
				if isPageRangeDeviation(gc) {
					if gc.Status != 500 || !strings.Contains(gc.Body, `"SYSTEM_ERROR"`) {
						t.Fatalf("Java 越界不再是 500 SYSTEM_ERROR，偏差登记需复核: %d %s", gc.Status, gc.Body)
					}
					if status != 400 || !strings.Contains(body, `"PARAM_INVALID"`) || !strings.Contains(body, `"OUT_OF_RANGE"`) {
						t.Fatalf("Go 应回 400 PARAM_INVALID（有意偏差）: %d %s", status, body)
					}
					return
				}
				if status != gc.Status {
					t.Fatalf("HTTP 状态: go=%d java=%d\ngo:   %s\njava: %s", status, gc.Status, body, gc.Body)
				}
				if cg, cj := canonical(t, body), canonical(t, gc.Body); cg != cj {
					t.Fatalf("规范化 JSON 不一致\ngo:   %s\njava: %s", cg, cj)
				}
				pg, pj := payloadRaw(body), payloadRaw(gc.Body)
				if strings.Join(pg, "\n") != strings.Join(pj, "\n") {
					t.Fatalf("payload 原文不一致（键序 / 数字 / 转义）\ngo:   %v\njava: %v", pg, pj)
				}
				if a, b := payloads(t, body), payloads(t, gc.Body); strings.Join(a, "\n") != strings.Join(b, "\n") {
					t.Fatalf("payload 重写不一致\ngo:   %v\njava: %v", a, b)
				}
			})
		}
	}
	if total < 50 {
		t.Fatalf("黄金样本条数过少: %d", total)
	}
}

func TestJacksonRoundTripMatchesJava(t *testing.T) {
	var g golden
	loadJSON(t, "testdata/java_golden.json", &g)
	if len(g.JacksonRoundTrip) == 0 {
		t.Fatal("golden 缺 jacksonRoundTrip")
	}
	for _, rt := range g.JacksonRoundTrip {
		v, err := v1wire.ParseJava([]byte(rt.In))
		if err != nil {
			t.Fatalf("%s: %v", rt.In, err)
		}
		if got := string(v1wire.AppendJackson(nil, v)); got != rt.Out {
			t.Errorf("往返不一致\nin:   %s\ngo:   %s\njava: %s", rt.In, got, rt.Out)
		}
		for _, m := range v.Obj {
			if got := v1wire.JavaStringOf(m.Val); got != rt.ValueOf[m.Key] {
				t.Errorf("String.valueOf(%s): go=%q java=%q", m.Key, got, rt.ValueOf[m.Key])
			}
		}
	}
	in := append([]string(nil), g.SortInput...)
	sort.SliceStable(in, func(i, j int) bool { return v1wire.CompareUTF16(in[i], in[j]) < 0 })
	if strings.Join(in, "|") != strings.Join(g.SortOutput, "|") {
		t.Fatalf("UTF-16 排序不一致\ngo:   %q\njava: %q", in, g.SortOutput)
	}
}
