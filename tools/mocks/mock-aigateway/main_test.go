package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/aigateway"
)

func signed(t *testing.T, secret, purpose, method, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	sg := aigateway.Signer{CallerID: "fusion-go", KeyID: "fusion-go-v1", Secret: secret}
	if err := sg.Sign(req, purpose, b, time.Now(), "nonce-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "k1")
	return req
}

func do(s *server, req *http.Request) (int, map[string]any) {
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	m := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func TestSubmitPollAndAuth(t *testing.T) {
	s := newServer()
	body := `{"operation":"image.edit","model_group":"img.gpt-image-2","input":{}}`
	code, c := do(s, signed(t, "test-secret", "calls", "POST", "/v1/calls", body))
	if code != 200 || c["status"] != "PENDING" {
		t.Fatalf("submit %d %v", code, c)
	}
	id := c["id"].(string)
	if _, c = do(s, signed(t, "test-secret", "calls", "GET", "/v1/calls/"+id, "")); c["status"] != "RUNNING" {
		t.Fatalf("want RUNNING got %v", c["status"])
	}
	_, c = do(s, signed(t, "test-secret", "calls", "GET", "/v1/calls/"+id, ""))
	outs := c["result"].(map[string]any)["outputs"].([]any)
	if c["status"] != "SUCCEEDED" || len(outs) != 1 {
		t.Fatalf("want SUCCEEDED+1 output got %v", c)
	}
	if code, _ = do(s, signed(t, "wrong", "calls", "GET", "/v1/calls/"+id, "")); code != 401 {
		t.Fatalf("bad secret want 401 got %d", code)
	}
	if code, _ = do(s, signed(t, "test-secret", "catalog", "GET", "/v1/calls/"+id, "")); code != 401 {
		t.Fatalf("purpose mismatch want 401 got %d", code)
	}
	if code, c = do(s, signed(t, "test-secret", "catalog", "GET", "/v1/catalog/model-groups", "")); code != 200 || len(c["items"].([]any)) != 8 { // 3 个图片组 + 4 个视频组 + 1 个视觉对话组
		t.Fatalf("catalog %d %v", code, c)
	}
}

func TestRequestHashIgnoresDeadlineAndLabels(t *testing.T) {
	a := requestHash([]byte(`{"operation":"image.edit","model_group":"g","input":{"prompt":"x"},"labels":{"task":"1"},"constraints":{"deadline_at":"2026-10-31T00:00:00Z"},"limits":{"max_cost":"5.00"}}`))
	b := requestHash([]byte(`{"operation":"image.edit","model_group":"g","input":{"prompt":"x"},"labels":{"task":"2"},"constraints":{"deadline_at":"2026-10-31T00:05:00Z"},"limits":{"max_cost":"5.00"}}`))
	c := requestHash([]byte(`{"operation":"image.edit","model_group":"g","input":{"prompt":"x"},"limits":{"max_cost":"6.00"}}`))
	if a != b || a == c {
		t.Fatalf("hash 口径不对：%s %s %s", a, b, c)
	}
}

func TestInjectOutcomes(t *testing.T) {
	s := newServer()
	inject := func(body string) {
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, httptest.NewRequest("POST", "/debug/inject", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("inject %d %s", w.Code, w.Body.String())
		}
	}
	submit := func(key, prompt string) (int, map[string]any) {
		req := signed(t, "test-secret", "calls", "POST", "/v1/calls?wait=1",
			`{"operation":"image.generate","model_group":"g","input":{"prompt":"`+prompt+`"}}`)
		req.Header.Set("Idempotency-Key", key)
		return do(s, req)
	}
	inject(`{"match":"MK_FAIL","outcome":"FAILED","class":"INPUT_REJECTED","times":1}`)
	inject(`{"match":"MK_UNK","outcome":"UNKNOWN"}`)
	inject(`{"match":"MK_HOLD","outcome":"HOLD"}`)
	inject(`{"match":"MK_503","outcome":"SUBMIT_503","times":1}`)

	if _, c := submit("a1", "x MK_FAIL"); c["status"] != "FAILED" || c["retry_safe"] != false ||
		c["failure"].(map[string]any)["class"] != "INPUT_REJECTED" {
		t.Fatalf("FAILED 注入不对：%v", c)
	}
	if _, c := submit("a1", "x MK_FAIL"); c["status"] != "FAILED" { // 同键重提拿回原调用
		t.Fatalf("同键重提：%v", c)
	}
	if _, c := submit("a2", "y MK_FAIL"); c["status"] != "SUCCEEDED" { // times=1 已用完
		t.Fatalf("次数用完后应成功：%v", c)
	}
	if _, c := submit("b1", "MK_UNK"); c["status"] != "UNKNOWN" || c["money_at_risk"] != true {
		t.Fatalf("UNKNOWN 注入不对：%v", c)
	}
	_, c := submit("c1", "MK_HOLD")
	if c["status"] != "RUNNING" {
		t.Fatalf("HOLD 应停在 RUNNING：%v", c)
	}
	id := c["id"].(string)
	if _, c = do(s, signed(t, "test-secret", "calls", "POST", "/v1/calls/"+id+"/cancel", "")); c["status"] != "CANCELLED" {
		t.Fatalf("HOLD 可取消：%v", c)
	}
	if code, _ := submit("d1", "MK_503"); code != 503 {
		t.Fatalf("SUBMIT_503 应回 503，got %d", code)
	}
	if _, c := submit("d1", "MK_503"); c["status"] != "SUCCEEDED" {
		t.Fatalf("503 之后重提应成功：%v", c)
	}
}
