package v1compat

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/jsoncase"

	"github.com/DATA-DOG/go-sqlmock"
)

func ptr(s string) *string { return &s }

// smallDataset 四个模板：1/2 带「电商」，3 带「海报」，4 停用。
func smallDataset() fixtureDataset {
	return fixtureDataset{
		Templates: []fixtureTemplate{
			{ID: 1, Title: ptr("A"), TagsJSON: ptr(`{"tags":["电商","a"]}`), DataJSON: ptr(`{"prompt":"x","agentSkillId":"1876543210987654321"}`), SortOrder: 0, UpdateTime: "2026-11-02 00:00:00", Enabled: 1},
			{ID: 2, Title: ptr("B"), TagsJSON: ptr(`{"tags":["电商","b"]}`), DataJSON: ptr(`{}`), SortOrder: 0, UpdateTime: "2026-11-01 00:00:00", Enabled: 1},
			{ID: 3, Title: ptr("C"), TagsJSON: ptr(`{"tags":["海报","c,d"]}`), DataJSON: ptr(`{}`), SortOrder: 1, UpdateTime: "2026-11-01 00:00:00", Enabled: 1},
			{ID: 4, Title: ptr("D"), TagsJSON: ptr(`{"tags":["电商"]}`), DataJSON: ptr(`{}`), SortOrder: 0, UpdateTime: "2026-11-01 00:00:00", Enabled: 0},
		},
		Headlines: []fixtureHeadline{{ID: 9, PlazaTemplateID: 3, StartOffset: -1, EndOffset: 1, Why: ptr("w"), Enabled: 1}},
	}
}

var fixedNow = time.Date(2026, 11, 14, 10, 0, 0, 0, shanghai)

func get(t *testing.T, h http.HandlerFunc, target string, hdr ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: 非 JSON %v: %s", target, err, rec.Body.String())
	}
	return rec, env
}

func ids(env map[string]any) []float64 {
	var out []float64
	for _, it := range env["data"].(map[string]any)["data"].([]any) {
		out = append(out, it.(map[string]any)["plazaTemplateId"].(float64))
	}
	return out
}

func TestListBindingLikeSpring(t *testing.T) {
	m := New(&fakeSource{ds: smallDataset()}, func() time.Time { return fixedNow })
	cases := []struct {
		q    string
		want []float64
	}{
		{"", []float64{1, 2, 3}},
		{"?page=&size=", []float64{1, 2, 3}},                         // 空串 → defaultValue
		{"?page=0x1&size=%232", []float64{1, 2}},                     // NumberUtils：0x / # 十六进制
		{"?page=%201%20&size=+2", []float64{1, 2}},                   // 去掉所有空白；+2 合法
		{"?page=2&size=2&page=9", []float64{3}},                      // 重复传取第一个
		{"?tags=%E6%B5%B7%E6%8A%A5", []float64{3}},                   // 单标签
		{"?tags=a,%20b%20", []float64{1, 2}},                         // 单值逗号拆分 + trim
		{"?tags=c,d", nil},                                           // 单值被拆成 c / d，标签 "c,d" 不命中
		{"?tags=c,d&tags=x", []float64{3}},                           // 多值不拆：命中 "c,d"
		{"?tags=", []float64{1, 2, 3}},                               // 空串 → 空列表 → 不过滤
		{"?tags=a&industry=foot_bath&mediaType=video", []float64{1}}, // 未知参数忽略
	}
	for _, c := range cases {
		rec, env := get(t, m.HandleList, PathList+c.q)
		if rec.Code != 200 || env["code"].(float64) != 0 {
			t.Fatalf("%s: %d %s", c.q, rec.Code, rec.Body.String())
		}
		if got := ids(env); jsonOf(got) != jsonOf(c.want) {
			t.Fatalf("%s: got %v want %v", c.q, got, c.want)
		}
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestListTypeMismatchIs400LikeJava(t *testing.T) {
	m := New(&fakeSource{ds: smallDataset()}, func() time.Time { return fixedNow })
	for q, field := range map[string]string{"?page=abc": "page", "?page=%20": "page", "?size=1.5": "size",
		"?page=abc&size=x": "page", "?size=99999999999999999999": "size", "?page=0x-1": "page"} {
		rec, env := get(t, m.HandleList, PathList+q)
		if rec.Code != 400 || env["code"].(float64) != 1003 || env["reason"] != "PARAM_INVALID" ||
			env["msg"] != "参数 "+field+" 格式不正确" {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		e := env["data"].(map[string]any)["errors"].([]any)[0].(map[string]any)
		if e["field"] != field || e["reason"] != "INVALID_TYPE" {
			t.Fatalf("%s: 明细 %v", q, e)
		}
	}
}

func TestEnvelopeAndRequestID(t *testing.T) {
	m := New(&fakeSource{ds: smallDataset()}, func() time.Time { return fixedNow })
	rec, env := get(t, m.HandleTags, PathTags, "X-Request-Id", "  abcDEF12-3  ")
	if rec.Header().Get("X-Request-Id") != "abcDEF12-3" || env["requestId"] != "abcDEF12-3" {
		t.Fatalf("合法入站 id 应沿用（trim 后）: %v / %s", env["requestId"], rec.Header().Get("X-Request-Id"))
	}
	if _, has := env["reason"]; has {
		t.Fatal("成功不带 reason")
	}
	if env["msg"] != "success" || env["timestamp"] == nil {
		t.Fatalf("信封: %s", rec.Body.String())
	}
	if !strings.HasPrefix(rec.Body.String(), `{"code":0,"msg":"success","data":["a","b","c,d","海报","电商"],"requestId":`) {
		t.Fatalf("键序 / 标签排序: %s", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type: %s", rec.Header().Get("Content-Type"))
	}
	for _, bad := range []string{"short", "has space inside", "bad;chars!", strings.Repeat("a", 65)} {
		_, env := get(t, m.HandleTags, PathTags, "X-Request-Id", bad)
		if id := env["requestId"].(string); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
			t.Fatalf("非法入站 id %q 应换成 32 位小写十六进制，得到 %q", bad, id)
		}
	}
}

// 经过网关的 jsoncase 中间件：响应必须原样（不补 snake 别名），X-Jsoncase 头不外泄。
func TestVerbatimThroughJsoncase(t *testing.T) {
	m := New(&fakeSource{ds: smallDataset()}, func() time.Time { return fixedNow })
	h := jsoncase.Middleware(m.HandleList)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, PathList+"?tags=a", nil))
	body := rec.Body.String()
	for _, alias := range []string{"plaza_template_id", "agent_skill_id", "usage_count", "author_name", "request_id"} {
		if strings.Contains(body, alias) {
			t.Fatalf("v1 响应被 jsoncase 补了别名 %s: %s", alias, body)
		}
	}
	if rec.Header().Get(jsoncase.SkipHeader) != "" {
		t.Fatal("X-Jsoncase 头不应发给客户端")
	}
	if !strings.Contains(body, `"agentSkillId":"1876543210987654321"`) {
		t.Fatalf("agentSkillId 应为字符串: %s", body)
	}
}

func TestHeadlineIgnoresIndustryAndUnknownParams(t *testing.T) {
	m := New(&fakeSource{ds: smallDataset()}, func() time.Time { return fixedNow })
	_, a := get(t, m.HandleHeadlines, PathHeadlines)
	_, b := get(t, m.HandleHeadlines, PathHeadlines+"?industry=foot_bath&subIndustry=x&scope=ALL")
	delete(a, "timestamp")
	delete(a, "requestId")
	delete(b, "timestamp")
	delete(b, "requestId")
	if jsonOf(a) != jsonOf(b) {
		t.Fatalf("industry 等未知参数不得改变输出\n%v\n%v", a, b)
	}
	// tag 重复传：Spring 用逗号拼接成一个值
	_, c := get(t, m.HandleHeadlines, PathHeadlines+"?tag=c&tag=d")
	if items := c["data"].(map[string]any)["items"].([]any); len(items) != 1 {
		t.Fatalf("tag=c&tag=d 应等同 tag=\"c,d\"，命中模板 3: %v", c)
	}
}

func TestUsageCacheAndFallback(t *testing.T) {
	src := &fakeSource{ds: fixtureDataset{Usage: []UseCount{{TemplateID: 1, Uses: 5, PreviousUses: 2}}}}
	now := fixedNow
	u := newUsageStats(src, func() time.Time { return now })
	s1 := u.snapshot(context.Background())
	if s1.of(1).uses != 5 || len(src.usageArgs) != 1 {
		t.Fatal("首次应查库")
	}
	a := src.usageArgs[0]
	if !a[2].Equal(fixedNow) || !a[1].Equal(fixedNow.Add(-7*24*time.Hour)) || !a[0].Equal(fixedNow.Add(-14*24*time.Hour)) {
		t.Fatalf("窗口: %v", a)
	}
	if s1.startDate() != "2026-11-07" || s1.endDate() != "2026-11-14" {
		t.Fatalf("statsFrom/To: %s %s", s1.startDate(), s1.endDate())
	}
	now = now.Add(9 * time.Minute)
	u.snapshot(context.Background())
	if len(src.usageArgs) != 1 {
		t.Fatal("10 分钟内应走缓存")
	}
	now = now.Add(2 * time.Minute)
	src.failUsage = true
	s3 := u.snapshot(context.Background())
	if s3.of(1).uses != 5 || !s3.computedAt.Equal(now) {
		t.Fatal("刷新失败应沿用上一份并把计算时刻推后")
	}
	u2 := newUsageStats(src, func() time.Time { return now })
	if s := u2.snapshot(context.Background()); s.of(1).uses != 0 || u2.cached != nil {
		t.Fatal("没有上一份时当全 0 且不缓存")
	}
	tr := func(uses, prev int64) any {
		if p := (usage{uses, prev}).trend(); p != nil {
			return *p
		}
		return nil
	}
	if tr(3, 0) != nil || tr(42, 5) != int64(740) || tr(3, 7) != int64(-57) || tr(1, 8) != int64(-87) || tr(3, 2) != int64(50) {
		t.Fatal("trend 取整（Math.round）")
	}
}

// 只读：三个接口对库只发 SELECT（sqlmock 对任何未声明的语句——含 INSERT/UPDATE/DELETE——直接报错）。
func TestSQLSourceIsReadOnly(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tplCols := []string{"id", "title", "description", "tags_json", "data_json"}
	mock.ExpectQuery(`^SELECT plaza_template_id, SUM\(CASE WHEN create_time >= \? .* FROM afv_agent_turn WHERE plaza_template_id IS NOT NULL AND create_time >= \? AND create_time < \? GROUP BY plaza_template_id$`).
		WithArgs("2026-11-07 10:00:00", "2026-11-07 10:00:00", "2026-10-31 10:00:00", "2026-11-14 10:00:00").
		WillReturnRows(sqlmock.NewRows([]string{"plaza_template_id", "uses", "previous_uses"}).AddRow(int64(1), []byte("3"), []byte("1")))
	mock.ExpectQuery(`^SELECT id, title, description, tags_json, data_json FROM afv_compat_plaza_template WHERE enabled = 1 AND deleted = 0 ORDER BY sort_order ASC, update_time DESC, id DESC$`).
		WillReturnRows(sqlmock.NewRows(tplCols).AddRow(int64(1), "A", nil, []byte(`{"tags": ["x"]}`), []byte(`{"a": 1}`)))
	mock.ExpectQuery(`^SELECT id, title, description, tags_json, data_json FROM afv_compat_plaza_template WHERE enabled = 1 AND deleted = 0 ORDER BY`).
		WillReturnRows(sqlmock.NewRows(tplCols).AddRow(int64(1), "A", nil, nil, []byte(`{}`)))
	mock.ExpectQuery(`^SELECT id, plaza_template_id, DATE_FORMAT\(start_date, '%Y-%m-%d'\), DATE_FORMAT\(end_date, '%Y-%m-%d'\), why, sort_order FROM afv_plaza_headline WHERE enabled = 1 AND start_date <= \? AND end_date >= \? AND deleted = 0$`).
		WithArgs("2026-11-14", "2026-11-14").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plaza_template_id", "s", "e", "why", "sort_order"}).AddRow(int64(5), int64(1), "2026-11-13", "2026-11-15", "w", int64(0)))
	mock.ExpectQuery(`^SELECT id, title, description, tags_json, data_json FROM afv_compat_plaza_template WHERE id IN \(\?\) AND enabled = 1 AND deleted = 0$`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows(tplCols).AddRow(int64(1), "A", "d", []byte(`{"tags": ["x"]}`), []byte(`{"a": 1}`)))

	m := New(SQLSource{DB: db}, func() time.Time { return fixedNow })
	if rec, _ := get(t, m.HandleList, PathList); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"usageCount":3`) {
		t.Fatalf("list: %s", rec.Body.String())
	}
	if rec, _ := get(t, m.HandleTags, PathTags); rec.Code != 200 {
		t.Fatalf("tags: %s", rec.Body.String())
	}
	if rec, _ := get(t, m.HandleHeadlines, PathHeadlines); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"uses":3,"previousUses":1,"trend":200,"isNew":false`) {
		t.Fatalf("headlines: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// 源码层面再兜一道：本包非测试文件里不得出现写语句
	for _, f := range []string{"store.go", "templates.go", "headlines.go", "usage.go", "http.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if w := regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM|REPLACE\s+INTO|ALTER\s+TABLE|TRUNCATE\s+TABLE|FOR\s+UPDATE)\b`).Find(b); w != nil {
			t.Fatalf("%s 出现写语句 %q", f, w)
		}
	}
}

func TestDBErrorIs500SystemError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT").WillReturnError(driver.ErrBadConn)
	m := New(SQLSource{DB: db}, func() time.Time { return fixedNow })
	rec, env := get(t, m.HandleTags, PathTags)
	if rec.Code != 500 || env["code"].(float64) != 5001 || env["reason"] != "SYSTEM_ERROR" || env["msg"] != "系统内部错误" || env["data"] != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
