package jsoncase

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/rest/httpx"
)

func run(t *testing.T, h http.HandlerFunc, method, body, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	Middleware(h)(rec, req)
	return rec
}

func jsonHandler(resp string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(resp))
	}
}

func TestResponseGetsCamelAliasesKeepingSnake(t *testing.T) {
	rec := run(t, jsonHandler(`{"code":0,"data":{"put_url":"u","items":[{"task_id":"t1"}]}}`), "GET", "", "")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	data := got["data"].(map[string]any)
	if data["put_url"] != "u" || data["putUrl"] != "u" {
		t.Errorf("put_url/putUrl not both present: %v", data)
	}
	item := data["items"].([]any)[0].(map[string]any)
	if item["task_id"] != "t1" || item["taskId"] != "t1" {
		t.Errorf("nested alias missing: %v", item)
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Errorf("Content-Length not set")
	}
}

func TestNonJSONPassesThrough(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"put_url\":\"x\"}\n\n")
	}
	rec := run(t, h, "GET", "", "")
	if rec.Body.String() != "data: {\"put_url\":\"x\"}\n\n" {
		t.Errorf("SSE body changed: %q", rec.Body.String())
	}
}

func TestRequestCamelMappedOnlyForKnownNames(t *testing.T) {
	var seen map[string]any
	h := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}
	// content_type 是白名单内的字段名：camel 请求补上 snake
	run(t, h, "POST", `{"kind":"image","contentType":"image/png","myKey":"keep"}`, "application/json")
	if seen["content_type"] != "image/png" || seen["contentType"] != "image/png" {
		t.Errorf("content_type not added from contentType: %v", seen)
	}
	// 非白名单的自由键不改名
	if _, ok := seen["my_key"]; ok {
		t.Errorf("non-whitelisted key was rewritten: %v", seen)
	}
	if seen["myKey"] != "keep" {
		t.Errorf("free-form key lost: %v", seen)
	}
}

func TestRequestSnakeWinsWhenBothGiven(t *testing.T) {
	var seen map[string]any
	h := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
	}
	run(t, h, "POST", `{"content_type":"image/jpeg","contentType":"image/png"}`, "application/json")
	if seen["content_type"] != "image/jpeg" {
		t.Errorf("snake value overwritten: %v", seen)
	}
}

func TestToSnakeToCamel(t *testing.T) {
	if toSnake("contentType") != "content_type" || toCamel("content_type") != "contentType" {
		t.Errorf("case conversion wrong: %s / %s", toSnake("contentType"), toCamel("content_type"))
	}
}

// ---------------------------------------------------------------- query

// runQuery 发一个只带 query 的 GET，返回 handler 看到的 RawQuery。
func runQuery(t *testing.T, rawQuery string) string {
	t.Helper()
	var seen string
	h := func(w http.ResponseWriter, r *http.Request) { seen = r.URL.RawQuery }
	req := httptest.NewRequest("GET", "/x?"+rawQuery, nil)
	Middleware(h)(httptest.NewRecorder(), req)
	return seen
}

func TestQueryCamelMappedToSnake(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "pageSize=20&billingStatus=HELD&page=2"))
	if q.Get("page_size") != "20" || q.Get("billing_status") != "HELD" {
		t.Errorf("camel query not mapped: %v", q)
	}
	if q.Get("pageSize") != "20" || q.Get("page") != "2" {
		t.Errorf("original params lost: %v", q)
	}
}

func TestQuerySnakeUntouched(t *testing.T) {
	raw := "page_size=20&billing_status=HELD"
	if got := runQuery(t, raw); got != raw {
		t.Errorf("snake query changed: %q", got)
	}
}

func TestQuerySnakeWinsWhenBothGiven(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "pageSize=50&page_size=20"))
	if got := q["page_size"]; len(got) != 1 || got[0] != "20" {
		t.Errorf("snake value overwritten / duplicated: %v", got)
	}
}

func TestQueryUnknownKeysUntouched(t *testing.T) {
	raw := "myKey=1&keyword=abc&someCamelThing=x"
	if got := runQuery(t, raw); got != raw {
		t.Errorf("unknown keys rewritten: %q", got)
	}
}

func TestQueryRepeatedKeys(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "taskIds=a&taskIds=b&tagIds=1"))
	if got := q["task_ids"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("repeated camel values not all mapped in order: %v", got)
	}
	if got := q["tag_ids"]; len(got) != 1 || got[0] != "1" {
		t.Errorf("tag_ids not mapped: %v", got)
	}
}

func TestQueryEncodingPreserved(t *testing.T) {
	raw := "keyword=%E4%B8%AD%E6%96%87+a&entryCode=a%26b%3Dc%2Fd&userId"
	got := runQuery(t, raw)
	want := raw + "&entry_code=a%26b%3Dc%2Fd&user_id"
	if got != want {
		t.Errorf("encoding not preserved:\n got  %q\n want %q", got, want)
	}
	q, _ := url.ParseQuery(got)
	if q.Get("entry_code") != "a&b=c/d" || q.Get("keyword") != "中文 a" {
		t.Errorf("decoded values wrong: %v", q)
	}
	if _, ok := q["user_id"]; !ok {
		t.Errorf("valueless key not mapped: %v", q)
	}
}

func TestQueryEncodedCamelKeyMapped(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "page%53ize=7"))
	if q.Get("page_size") != "7" {
		t.Errorf("percent-encoded camel key not mapped: %v", q)
	}
}

func TestQueryBindsThroughGoZero(t *testing.T) {
	type listReq struct {
		Page          int64    `form:"page,optional"`
		PageSize      int64    `form:"page_size,optional"`
		BillingStatus string   `form:"billing_status,optional"`
		TaskIds       []string `form:"task_ids,optional"`
	}
	var got listReq
	var perr error
	h := func(w http.ResponseWriter, r *http.Request) { perr = httpx.Parse(r, &got) }
	req := httptest.NewRequest("GET", "/x?page=2&pageSize=20&billingStatus=HELD&taskIds=a&taskIds=b", nil)
	Middleware(h)(httptest.NewRecorder(), req)
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	if got.Page != 2 || got.PageSize != 20 || got.BillingStatus != "HELD" || len(got.TaskIds) != 2 {
		t.Errorf("go-zero binding did not see mapped query: %+v", got)
	}
}

func TestQueryMalformedPairsLeftAlone(t *testing.T) {
	// 键编码非法 / 空段 / 分号：不参与改写也不报错，合法的 camel 参数照常补 snake
	got := runQuery(t, "bad%zzKey=1&&a;b=2&pageSize=3")
	want := "bad%zzKey=1&&a;b=2&pageSize=3&page_size=3"
	if got != want {
		t.Errorf("malformed query handling:\n got  %q\n want %q", got, want)
	}
}
