package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	handler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
	m := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func TestAnalyzeEchoesFabric(t *testing.T) {
	code, m := post(t, "/api/reverse/analyze", `{"fabric":"上衣：真丝"}`)
	p, _ := m["prompt"].(string)
	if code != 200 || !strings.Contains(p, "上衣：真丝") || !strings.HasPrefix(p, "【任务】") || len([]rune(p)) <= 800 {
		t.Fatalf("code=%d prompt=%q", code, p)
	}
}

func TestFailSwitchAndPoseSource(t *testing.T) {
	t.Setenv("MOCK_REVERSE_FAIL", "analyze")
	if code, _ := post(t, "/api/reverse/analyze", `{}`); code != 502 {
		t.Fatalf("want 502 got %d", code)
	}
	if code, _ := post(t, "/api/pose/source", `{}`); code != 422 {
		t.Fatalf("want 422 got %d", code)
	}
}
