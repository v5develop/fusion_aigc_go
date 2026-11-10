package jsoncase

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
