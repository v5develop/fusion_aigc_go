package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/v1compat"
)

// memSource 内存数据（两个「上游」共用同一份，任何差异都只能来自下面故意注入的改写）。
type memSource struct{ uses int64 }

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

func (m *memSource) EnabledTemplates(context.Context) ([]v1compat.TemplateRow, error) {
	var out []v1compat.TemplateRow
	for i, tag := range []string{"电商", "海报", "😀表情", "电商", "Mixed"} {
		out = append(out, v1compat.TemplateRow{ID: int64(i + 1), Title: ns("模板" + tag),
			TagsJSON: []byte(`{"tags": ["` + tag + `", "通用"]}`),
			DataJSON: []byte(`{"prompt": "p", "description": "说明 ` + tag + `", "n": 1e10, "agentSkillId": 2085576666117050369}`)})
	}
	return out, nil
}

func (m *memSource) TemplatesByID(ctx context.Context, ids []int64) ([]v1compat.TemplateRow, error) {
	all, _ := m.EnabledTemplates(ctx)
	return all, nil
}

func (m *memSource) ActiveHeadlines(_ context.Context, today string) ([]v1compat.HeadlineRow, error) {
	var out []v1compat.HeadlineRow
	for i := 1; i <= 3; i++ {
		h := v1compat.HeadlineRow{ID: int64(i), StartDate: ns(today), EndDate: ns(today), Why: ns("推荐语")}
		h.PlazaTemplateID = sql.NullInt64{Int64: int64(i), Valid: true}
		out = append(out, h)
	}
	return out, nil
}

func (m *memSource) CountUses(context.Context, time.Time, time.Time, time.Time) ([]v1compat.UseCount, error) {
	return []v1compat.UseCount{{TemplateID: 1, Uses: m.uses, PreviousUses: 10}, {TemplateID: 2, Uses: 50, PreviousUses: 0}}, nil
}

func server(t *testing.T, src v1compat.Source, rewrite func(path string, body []byte) []byte) string {
	t.Helper()
	m := v1compat.New(src, nil)
	mux := http.NewServeMux()
	mux.HandleFunc(v1compat.PathList, m.HandleList)
	mux.HandleFunc(v1compat.PathTags, m.HandleTags)
	mux.HandleFunc(v1compat.PathHeadlines, m.HandleHeadlines)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rewrite == nil {
			mux.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rewrite(r.URL.Path, rec.Body.Bytes()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runTool(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := run(append([]string{"-corpus", "corpus.json", "-retries", "0"}, args...), &out)
	return code, out.String()
}

func TestIdenticalUpstreamsPass(t *testing.T) {
	java := server(t, &memSource{uses: 100}, nil)
	gov := server(t, &memSource{uses: 100}, nil)
	report := filepath.Join(t.TempDir(), "r.md")
	code, out := runTool(t, "-java", java, "-go", gov, "-report", report)
	if code != 0 {
		t.Fatalf("同源应 0 差异，退出码 %d\n%s", code, out)
	}
	if !strings.Contains(out, "0 硬差异") || !strings.Contains(out, "[跳过] list_logged_in") {
		t.Fatalf("报告: %s", out)
	}
	b, _ := os.ReadFile(report)
	if !strings.Contains(string(b), "| plazav1diff") && !strings.Contains(string(b), "# plazav1diff 报告") {
		t.Fatalf("Markdown 报告: %s", b)
	}
}

func TestStatsPhaseDifferenceIsTolerated(t *testing.T) {
	java := server(t, &memSource{uses: 100}, nil)
	gov := server(t, &memSource{uses: 103}, nil) // 统计缓存相位差：uses / trend / usageCount 小幅不同
	code, out := runTool(t, "-java", java, "-go", gov)
	if code != 0 || !strings.Contains(out, "[容差]") {
		t.Fatalf("容差内的统计差异应放过，退出码 %d\n%s", code, out)
	}
	gov2 := server(t, &memSource{uses: 10}, nil) // 统计差太多：uses 掉到 50 以下，名次也变了
	if code, out := runTool(t, "-java", java, "-go", gov2); code != 1 {
		t.Fatalf("超出容差应判硬差异，退出码 %d\n%s", code, out)
	}
}

func TestHardDifferencesFail(t *testing.T) {
	java := server(t, &memSource{uses: 100}, nil)
	cases := map[string]func(string, []byte) []byte{
		// payload 键序不同：规范化后相等，但原文比较能抓到
		"payload 键序": func(_ string, b []byte) []byte {
			return bytes.Replace(b, []byte(`{"description":"说明 电商","n":1.0E10}`), []byte(`{"n":1.0E10,"description":"说明 电商"}`), 1)
		},
		"数字写法": func(_ string, b []byte) []byte { return bytes.ReplaceAll(b, []byte(`1.0E10`), []byte(`10000000000`)) },
		"多出字段": func(p string, b []byte) []byte {
			if p != v1compat.PathList {
				return b
			}
			return bytes.Replace(b, []byte(`"usageCount":`), []byte(`"industries":[],"usageCount":`), 1)
		},
		"agentSkillId 变数字": func(_ string, b []byte) []byte {
			return bytes.ReplaceAll(b, []byte(`"agentSkillId":"2085576666117050369"`), []byte(`"agentSkillId":2085576666117050369`))
		},
		"信封多了 reason": func(_ string, b []byte) []byte {
			return bytes.Replace(b, []byte(`{"code":0,`), []byte(`{"code":0,"reason":"OK",`), 1)
		},
	}
	for name, rw := range cases {
		gov := server(t, &memSource{uses: 100}, rw)
		code, out := runTool(t, "-java", java, "-go", gov)
		if code != 1 || !strings.Contains(out, "[差异]") {
			t.Fatalf("%s: 应判硬差异，退出码 %d\n%s", name, code, out)
		}
	}
}

func TestStatusAndNetworkErrors(t *testing.T) {
	java := server(t, &memSource{uses: 100}, nil)
	gov := httptest.NewServer(http.NotFoundHandler()) // Go 开关没开：路由不存在
	defer gov.Close()
	if code, out := runTool(t, "-java", java, "-go", gov.URL, "-only", "^tags$"); code != 1 || !strings.Contains(out, "HTTP 状态不同") {
		t.Fatalf("404 应是硬差异，退出码 %d\n%s", code, out)
	}
	if code, _ := runTool(t, "-java", java, "-go", "http://127.0.0.1:1", "-only", "^tags$", "-timeout", "2s"); code != 2 {
		t.Fatalf("连不上应退出码 2，得到 %d", code)
	}
	if code, _ := runTool(t); code != 2 {
		t.Fatal("缺 -java / -go 应退出码 2")
	}
}

func TestCompareClassification(t *testing.T) {
	r := Rules{Ignore: []string{"timestamp"}, Raw: []string{"payload"}, Volatile: []string{"uses", "trend", "isNew"},
		OrderDerived: []string{"rank"}, Align: map[string]string{"data.items": "id"}, VolatileAbs: 2, VolatileRel: 0}
	tree := func(s string) *node {
		n, err := parseTree([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	j := tree(`{"timestamp":1,"data":{"items":[{"id":1,"rank":1,"uses":10,"trend":null,"isNew":true,"payload":{"a":1,"b":2}},{"id":2,"rank":2,"uses":9,"trend":5,"isNew":false,"payload":{}}]}}`)
	g := tree(`{"timestamp":2,"data":{"items":[{"id":2,"rank":1,"uses":11,"trend":6,"isNew":false,"payload":{}},{"id":1,"rank":2,"uses":10,"trend":null,"isNew":true,"payload":{"a":1,"b":2}}]}}`)
	ds := Compare(j, g, r)
	for _, d := range ds {
		if !d.Tolerated {
			t.Fatalf("统计相位差引起的名次 / 顺序 / trend 变化应放过: %+v", d)
		}
	}
	if len(ds) == 0 {
		t.Fatal("应报出容差内差异")
	}
	g2 := tree(`{"timestamp":2,"data":{"items":[{"id":1,"rank":1,"uses":10,"trend":null,"isNew":true,"payload":{"b":2,"a":1}},{"id":2,"rank":2,"uses":9,"trend":5,"isNew":false,"payload":{}}]}}`)
	if ds := Compare(j, g2, r); len(ds) != 1 || ds[0].Tolerated || ds[0].Path != "data.items[id=1].payload" {
		t.Fatalf("payload 键序必须逐字节一致: %+v", ds)
	}
	g3 := tree(`{"timestamp":2,"data":{"items":[{"id":2,"rank":1,"uses":9,"trend":5,"isNew":false,"payload":{}},{"id":1,"rank":2,"uses":10,"trend":null,"isNew":true,"payload":{"a":1,"b":2}}]}}`)
	hard := 0
	for _, d := range Compare(j, g3, r) {
		if !d.Tolerated {
			hard++
		}
	}
	if hard == 0 {
		t.Fatal("统计完全相同而顺序不同，是硬差异")
	}
}
