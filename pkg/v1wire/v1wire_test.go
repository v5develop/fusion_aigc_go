package v1wire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/pkg/xerr"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v %s", err, rec.Body.String())
	}
	return m
}

func TestOKEnvelope(t *testing.T) {
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Request-Id", "rid-1")
	rec := httptest.NewRecorder()
	OK(rec, r, nil)
	m := decode(t, rec)
	if rec.Code != 200 || m["code"] != float64(0) || m["msg"] != "success" || m["requestId"] != "rid-1" {
		t.Fatalf("成功信封不对: %v", m)
	}
	if _, has := m["reason"]; has {
		t.Fatal("成功不应带 reason")
	}
	if v, has := m["data"]; !has || v != nil {
		t.Fatalf("data 为空必须输出 null 而不是省略: %v", m)
	}
	if ts, _ := m["timestamp"].(float64); ts < 1e12 {
		t.Fatalf("timestamp 应是毫秒: %v", m["timestamp"])
	}
}

func TestRequestIDGenerated(t *testing.T) {
	r := httptest.NewRequest("GET", "/x", nil)
	if id := RequestID(r); len(id) != 32 {
		t.Fatalf("缺省应生成 32 位十六进制: %q", id)
	}
}

func TestFailStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   int
		reason string
	}{
		{"缺必填 400", xerr.New(xerr.ParamMissing, "请先选择行业"), 400, 1002, "PARAM_MISSING"},
		{"非法参数 400", xerr.Param("x"), 400, 1003, "PARAM_INVALID"},
		{"未登录 401", xerr.New(xerr.Unauthorized, "未登录"), 401, 2001, "UNAUTHORIZED"},
		{"行业失效 200", xerr.NewReason(xerr.NotFound, "INDUSTRY_NOT_FOUND", "这个行业已调整，请重新选择"), 200, 4001, "INDUSTRY_NOT_FOUND"},
		{"限流 429", xerr.NewReason(xerr.Conflict, ReasonTooFrequent, "操作太快了，稍后再试"), 429, 4002, ReasonTooFrequent},
		{"普通冲突 200", xerr.New(xerr.Conflict, "x"), 200, 4002, "RESOURCE_CONFLICT"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		Fail(rec, httptest.NewRequest("GET", "/x", nil), c.err)
		m := decode(t, rec)
		if rec.Code != c.status || m["code"] != float64(c.code) || m["reason"] != c.reason {
			t.Errorf("%s: status=%d body=%v", c.name, rec.Code, m)
		}
	}
}

func TestFailSystemErrorHidesDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	Fail(rec, httptest.NewRequest("GET", "/x", nil), http.ErrAbortHandler)
	m := decode(t, rec)
	if m["code"] != float64(xerr.ServerError) || strings.Contains(rec.Body.String(), "abort") {
		t.Fatalf("系统错误不应暴露细节: %v", m)
	}
}

func TestPublicCacheControl(t *testing.T) {
	rec := httptest.NewRecorder()
	Public(rec, 60)
	if rec.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatal(rec.Header())
	}
}
