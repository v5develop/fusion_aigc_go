package aigateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSecret = "fusion-go-test-secret"

// 路由 → x-aig-scope（openapi_paths.yaml）。
func routeScope(method, path string) string {
	switch {
	case method == http.MethodPost && path == "/v1/calls",
		method == http.MethodGet && strings.HasPrefix(path, "/v1/calls/") && !strings.HasSuffix(path, "/cancel"),
		method == http.MethodPost && strings.HasSuffix(path, "/cancel"),
		method == http.MethodPost && path == "/v1/calls:batchGet":
		return PurposeCalls
	case strings.HasPrefix(path, "/v1/catalog/"):
		return PurposeCatalog
	case path == "/v1/ledger/costs":
		return PurposeLedger
	}
	return ""
}

type seen struct {
	method, path, rawQuery, idemKey string
	body                            []byte
	header                          http.Header
}

// newGateway 假网关：校验七个头、Purpose 与路由 scope、签名本身，再交给 handle。
func newGateway(t *testing.T, handle func(w http.ResponseWriter, s seen)) (*Client, *[]seen) {
	t.Helper()
	var log []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := seen{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, idemKey: r.Header.Get("Idempotency-Key"), body: body, header: r.Header.Clone()}
		log = append(log, s)
		for _, h := range []string{HeaderVersion, HeaderCaller, HeaderKeyID, HeaderPurpose, HeaderTimestamp, HeaderNonce, HeaderSignature} {
			if r.Header.Get(h) == "" {
				t.Errorf("%s %s: 缺少请求头 %s", r.Method, r.URL.Path, h)
			}
		}
		if got, want := r.Header.Get(HeaderPurpose), routeScope(r.Method, r.URL.Path); got != want {
			t.Errorf("%s %s: purpose=%q, 路由 scope=%q", r.Method, r.URL.Path, got, want)
		}
		if r.Header.Get(HeaderCaller) != DefaultCallerID || r.Header.Get(HeaderKeyID) != DefaultKeyID {
			t.Errorf("caller/key id = %q/%q", r.Header.Get(HeaderCaller), r.Header.Get(HeaderKeyID))
		}
		var ts int64
		_ = json.Unmarshal([]byte(r.Header.Get(HeaderTimestamp)), &ts)
		canonical := CanonicalString(r.Header.Get(HeaderCaller), r.Header.Get(HeaderPurpose), r.Method, r.URL.Path,
			CanonicalQuery(r.URL.RawQuery), ts, r.Header.Get(HeaderNonce), body)
		if Signature(testSecret, canonical) != r.Header.Get(HeaderSignature) {
			t.Errorf("%s %s: 签名不匹配", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		handle(w, s)
	}))
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL + "/", Secret: testSecret, Timeout: 5 * time.Second})
	return c, &log
}

const callJSON = `{
  "id": "call_01J8Z3K6W9ABCDEF",
  "idempotency_key": "job-1:gen/0",
  "caller_id": "fusion-go",
  "request_hash": "` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `",
  "operation": "image.generate",
  "model_group": "img.gpt-image-2",
  "status": "SUCCEEDED",
  "run_no": 1,
  "updated_seq": 7,
  "attempts": [{"seq":1,"run_no":1,"deployment":"d1","provider":"openai","state":"SUCCEEDED","charged":"CHARGED",
    "started_at":"2026-09-27T08:00:00Z","request":{"method":"POST"},"future_field":1}],
  "result": {"operation":"image.generate","outputs":[
    {"index":0,"status":"SUCCEEDED","media":{"rid":"abc123-work-abcdefghijklmnopqrstuvwxyz","aig_key":"abc123-work-abcdefghijklmnopqrstuvwxyz",
      "url":"https://b/x.png?Signature=y","url_expires_at":"2026-09-28T08:00:41Z","sha256":"` + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" + `",
      "bytes":1482113,"mime":"image/png","width":1024,"height":1536}},
    {"index":1,"status":"FAILED","failure":{"class":"CONTENT_POLICY","code":"vendor.content_policy.post_accept"}}]},
  "cost": {"amount":"0.12","currency":"CNY","amount_cny":"0.12","source":"configured","final":true},
  "retry_safe": false,
  "money_at_risk": false,
  "labels": {"job":"job-1"},
  "created_at": "2026-09-27T08:00:00Z",
  "updated_at": "2026-09-27T08:00:41Z",
  "completed_at": "2026-09-27T08:00:41Z",
  "unknown_new_field": {"x": 1}
}`

func TestSubmit(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, callJSON)
	})
	call, err := c.Submit(context.Background(), CallRequest{
		Operation:  "image.generate",
		ModelGroup: "img.gpt-image-2",
		Input:      map[string]any{"prompt": "白底 商品", "n": 2},
		Limits:     &Limits{MaxCost: "2.50"},
		Labels:     map[string]string{"job": "job-1"},
	}, SubmitOptions{IdempotencyKey: "job-1:gen/0", Wait: 25 * time.Second, RetryFailed: true})
	if err != nil {
		t.Fatal(err)
	}
	s := (*log)[0]
	if s.method != http.MethodPost || s.path != "/v1/calls" {
		t.Fatalf("route = %s %s", s.method, s.path)
	}
	if s.idemKey != "job-1:gen/0" {
		t.Errorf("Idempotency-Key = %q", s.idemKey)
	}
	if s.rawQuery != "retry_failed=true&wait=25" {
		t.Errorf("query = %q", s.rawQuery)
	}
	var body map[string]any
	_ = json.Unmarshal(s.body, &body)
	if body["operation"] != "image.generate" || body["limits"].(map[string]any)["max_cost"] != "2.50" {
		t.Errorf("body = %s", s.body)
	}
	if _, ok := body["constraints"]; ok {
		t.Errorf("空 constraints 不应发送: %s", s.body)
	}

	if !call.IsTerminal() || !call.Succeeded() || call.UpdatedSeq != 7 || len(call.Attempts) != 1 {
		t.Fatalf("call = %+v", call)
	}
	media := call.Result.Media()
	if len(media) != 1 || media[0].Ref() != "mr://abc123-work-abcdefghijklmnopqrstuvwxyz" || media[0].Bytes != 1482113 {
		t.Fatalf("media = %+v", media)
	}
	if call.Result.Outputs[1].Failure.Class != "CONTENT_POLICY" {
		t.Errorf("output failure = %+v", call.Result.Outputs[1])
	}
	var raw struct {
		Outputs []json.RawMessage `json:"outputs"`
	}
	if err := call.Result.Decode(&raw); err != nil || len(raw.Outputs) != 2 {
		t.Errorf("Decode raw: %v %d", err, len(raw.Outputs))
	}
}

func TestSubmitDefaultWaitAndValidation(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) { _, _ = io.WriteString(w, callJSON) })
	if _, err := c.Submit(context.Background(), CallRequest{Operation: "image.generate", ModelGroup: "g"},
		SubmitOptions{IdempotencyKey: "k1", Wait: UseDefaultWait}); err != nil {
		t.Fatal(err)
	}
	if (*log)[0].rawQuery != "wait=25" {
		t.Errorf("query = %q", (*log)[0].rawQuery)
	}
	if _, err := c.Submit(context.Background(), CallRequest{}, SubmitOptions{IdempotencyKey: "has space"}); err == nil {
		t.Error("非法幂等键应在本地拒绝")
	}
	if _, err := c.Submit(context.Background(), CallRequest{}, SubmitOptions{IdempotencyKey: "k", ReconciledNotCharged: "r1"}); err == nil {
		t.Error("reconciled_not_charged 无 retry_failed 应在本地拒绝")
	}
	if len(*log) != 1 {
		t.Errorf("本地校验失败不应发请求")
	}
}

func TestGetLongPoll(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) { _, _ = io.WriteString(w, callJSON) })
	call, err := c.Get(context.Background(), "call_01J8Z3K6W9ABCDEF", 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := (*log)[0]
	if s.method != http.MethodGet || s.path != "/v1/calls/call_01J8Z3K6W9ABCDEF" || s.rawQuery != "wait=60" || len(s.body) != 0 {
		t.Errorf("seen = %s %s ?%s body=%q", s.method, s.path, s.rawQuery, s.body)
	}
	if call.ID != "call_01J8Z3K6W9ABCDEF" {
		t.Errorf("id = %q", call.ID)
	}
	if _, err := c.Get(context.Background(), "../admin", 0); err == nil {
		t.Error("非法 call id 应在本地拒绝")
	}
}

func TestCancel(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) {
		_, _ = io.WriteString(w, strings.Replace(callJSON, `"status": "SUCCEEDED"`, `"status": "CANCELLED"`, 1))
	})
	call, err := c.Cancel(context.Background(), "call_01J8Z3K6W9ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	s := (*log)[0]
	if s.method != http.MethodPost || s.path != "/v1/calls/call_01J8Z3K6W9ABCDEF/cancel" {
		t.Errorf("route = %s %s", s.method, s.path)
	}
	if call.Status != StatusCancelled || !call.IsTerminal() || call.Succeeded() {
		t.Errorf("status = %s", call.Status)
	}
}

func TestBatchGet(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) {
		_, _ = io.WriteString(w, `{"items":[`+callJSON+`],"missing":["call_00000000000000"]}`)
	})
	calls, err := c.BatchGet(context.Background(), []string{"call_01J8Z3K6W9ABCDEF", "call_00000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	s := (*log)[0]
	if s.method != http.MethodPost || s.path != "/v1/calls:batchGet" {
		t.Errorf("route = %s %s", s.method, s.path)
	}
	if string(s.body) != `{"ids":["call_01J8Z3K6W9ABCDEF","call_00000000000000"]}` {
		t.Errorf("body = %s", s.body)
	}
	if len(calls) != 1 || calls[0].ID != "call_01J8Z3K6W9ABCDEF" {
		t.Errorf("calls = %+v", calls)
	}
	if out, err := c.BatchGet(context.Background(), nil); err != nil || out != nil {
		t.Error("空 ids 应直接返回")
	}
	if _, err := c.BatchGet(context.Background(), make([]string, MaxBatchGet+1)); err == nil {
		t.Error("超过 200 应在本地拒绝")
	}
}

func TestCatalogAndLedgerPurpose(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) {
		switch s.path {
		case "/v1/catalog/model-groups":
			_, _ = io.WriteString(w, `{"items":[{"code":"img.gpt-image-2","operations":["image.generate","image.edit"],"display_name":"GPT Image 2","enabled":true,
				"options_schema":{"type":"object"},"feature_limits":{"max_n":4,"sizes":["1024x1024"]},"default_timeout_s":600}],"config_version":3}`)
		case "/v1/catalog/readiness":
			_, _ = io.WriteString(w, `{"items":[{"model_group":"img.gpt-image-2","ready":false,"reason":"deployments.all_circuit_open","enabled_deployments":2,"available_deployments":0}],"config_version":3,"checked_at":"2026-09-27T08:00:00Z"}`)
		case "/v1/ledger/costs":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"FORBIDDEN_SCOPE","check":"hmac.scope_not_granted","message":"key lacks scope ledger"}}`)
		}
	})
	groups, err := c.ModelGroups(context.Background())
	if err != nil || len(groups) != 1 || *groups[0].FeatureLimits.MaxN != 4 || groups[0].Operations[1] != "image.edit" {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	ready, err := c.Readiness(context.Background(), []string{"img.gpt-image-2"})
	if err != nil || len(ready) != 1 || ready[0].Ready || ready[0].Reason != "deployments.all_circuit_open" {
		t.Fatalf("ready = %+v, %v", ready, err)
	}
	if string((*log)[1].body) != `{"items":[{"model_group":"img.gpt-image-2"}]}` {
		t.Errorf("readiness body = %s", (*log)[1].body)
	}
	_, err = c.Costs(context.Background(), CostsQuery{AfterID: 10, Limit: 100})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("costs err = %v", err)
	}
	if (*log)[2].rawQuery != "after_id=10&limit=100" {
		t.Errorf("costs query = %q", (*log)[2].rawQuery)
	}
}

func TestErrorIdempotencyConflict(t *testing.T) {
	c, _ := newGateway(t, func(w http.ResponseWriter, s seen) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"IDEMPOTENCY_CONFLICT","check":"idempotency.request_hash","message":"same key, different request","call_id":"call_01J8Z3K6W9ABCDEF"}}`)
	})
	_, err := c.Submit(context.Background(), CallRequest{Operation: "image.generate", ModelGroup: "g"}, SubmitOptions{IdempotencyKey: "k1"})
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("err = %T %v", err, err)
	}
	if ge.HTTPStatus != 409 || !ge.IsIdempotencyConflict() || ge.Check != "idempotency.request_hash" || ge.CallID != "call_01J8Z3K6W9ABCDEF" {
		t.Errorf("err = %+v", ge)
	}
	if !errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrRetryNotAllowed) || errors.Is(err, ErrUnauthorized) {
		t.Error("errors.Is 判别错误")
	}
	if ge.Temporary() || ge.RetrySafe {
		t.Error("409 不是暂时错误")
	}
}

func TestErrorAuthFailed(t *testing.T) {
	c, _ := newGateway(t, func(w http.ResponseWriter, s seen) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"AUTH_FAILED","check":"hmac.invalid_signature","message":"bad signature"}}`)
	})
	_, err := c.Get(context.Background(), "call_01J8Z3K6W9ABCDEF", 0)
	var ge *Error
	if !errors.As(err, &ge) || !ge.IsAuth() || ge.Code != CodeAuthFailed || ge.Check != "hmac.invalid_signature" {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, ErrUnauthorized) || ge.Temporary() {
		t.Error("401 应为鉴权失败且非暂时")
	}
}

func TestErrorTemporary(t *testing.T) {
	c, _ := newGateway(t, func(w http.ResponseWriter, s seen) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>502 Bad Gateway</html>")
	})
	_, err := c.Get(context.Background(), "call_01J8Z3K6W9ABCDEF", 0)
	var ge *Error
	if !errors.As(err, &ge) || !ge.Temporary() || !ge.RetrySafe || ge.HTTPStatus != 502 || !strings.Contains(ge.Message, "502") {
		t.Fatalf("err = %+v", err)
	}

	// 网络错误：连不上
	dead := New(Config{BaseURL: "http://127.0.0.1:1", Secret: testSecret, Timeout: time.Second})
	_, err = dead.Get(context.Background(), "call_01J8Z3K6W9ABCDEF", 0)
	if !errors.As(err, &ge) || ge.HTTPStatus != 0 || ge.Code != CodeTransport || !ge.Temporary() || ge.Err == nil {
		t.Fatalf("transport err = %+v", err)
	}

	// 未配置
	if _, err := New(Config{}).Get(context.Background(), "call_01J8Z3K6W9ABCDEF", 0); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestReadinessOpBody(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) {
		_, _ = io.WriteString(w, `{"items":[{"model_group":"img.gpt-image-2","operation":"image.edit","ready":false,"reason":"operation.mismatch"}]}`)
	})
	got, err := c.ReadinessOp(context.Background(), []ReadinessQuery{{ModelGroup: "img.gpt-image-2", Operation: "image.edit"}})
	if err != nil || len(got) != 1 || got[0].Ready || got[0].Reason != "operation.mismatch" || got[0].Operation != "image.edit" {
		t.Fatalf("got = %+v, %v", got, err)
	}
	if string((*log)[0].body) != `{"items":[{"model_group":"img.gpt-image-2","operation":"image.edit"}]}` {
		t.Errorf("body = %s", (*log)[0].body)
	}
	if out, err := c.ReadinessOp(context.Background(), nil); out != nil || err != nil {
		t.Fatal("空查询应直接返回")
	}
}

// X-Trace-Id 带创作任务号（网关据此把调用挂到任务轨迹）；不传时不发该头。
func TestSubmitSendsTraceID(t *testing.T) {
	c, log := newGateway(t, func(w http.ResponseWriter, s seen) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, callJSON)
	})
	req := CallRequest{Operation: "image.generate", ModelGroup: "img.gpt-image-2", Input: map[string]any{"prompt": "p"}}
	if _, err := c.Submit(context.Background(), req, SubmitOptions{IdempotencyKey: "k1", TraceID: " AiCreation_LF_abc "}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(context.Background(), req, SubmitOptions{IdempotencyKey: "k2"}); err != nil {
		t.Fatal(err)
	}
	if got := (*log)[0].header.Get("X-Trace-Id"); got != "AiCreation_LF_abc" {
		t.Errorf("X-Trace-Id = %q", got)
	}
	if _, ok := (*log)[1].header["X-Trace-Id"]; ok {
		t.Error("不传 TraceID 不应发 X-Trace-Id")
	}
}
