package canvasagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/svcauth"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/result"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func internalChain(t *testing.T, rpc *fakeRPC) (http.HandlerFunc, http.HandlerFunc) {
	t.Helper()
	result.Setup()
	var cfg config.CanvasInternalConf
	cfg.Keys = []config.CanvasInternalKey{{KeyId: "java-1", Secret: testSecret}}
	cfg.MaxSkewSeconds, cfg.NonceTTLSeconds = 300, 600
	v := BuildVerifier(cfg, svcauth.NewMemoryNonces())
	if v == nil {
		t.Fatal("应建出校验器")
	}
	s := testSvc(rpc, 100, 100)
	// 与 gateway.go 同序：PreserveInternalBody → jsoncase → 路由
	call := PreserveInternalBody(jsoncase.Middleware(enabled(s, InternalCallHandler(s, v))))
	tools := PreserveInternalBody(jsoncase.Middleware(enabled(s, InternalToolsHandler(v))))
	return call, tools
}

func internalReq(body, nonce string, uid int64, secret string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	svcauth.SignRequest(r, "java-1", secret, uid, nonce, time.Now(), []byte(body))
	return r
}

type envelope struct {
	Code    int            `json:"code"`
	Reason  string         `json:"reason"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	return e
}

func TestInternalCallSignedOK(t *testing.T) {
	rpc := &fakeRPC{}
	call, _ := internalChain(t, rpc)
	// 驼峰键（canvasId / modelId）在 jsoncase 白名单里会被补 snake 别名：验签与解析必须用原始 body
	body := `{"name":"canvas_update_nodes","arguments":{"canvas_id":"cv_0123456789abcdef","updates":[{"node_id":"m_1","data":{"modelId":"x","taskId":"t"}}]},"call_id":"turn-1:tool-1"}`
	rec := httptest.NewRecorder()
	call(rec, internalReq(body, "nonce-internal-ok-01", 42, testSecret))
	if rec.Code != http.StatusOK || len(rpc.got) != 1 {
		t.Fatalf("签名正确应执行: %d %s", rec.Code, rec.Body.String())
	}
	in := rpc.got[0]
	if in.UserId != 42 || in.Client != "svc" || in.Agent != "svc:java-1" || in.CallId != "turn-1:tool-1" {
		t.Fatalf("身份 / 审计标记不对: %+v", in)
	}
	if strings.Contains(in.Arguments, "model_id") || strings.Contains(in.Arguments, "task_id") {
		t.Fatalf("参数被 jsoncase 改写了: %s", in.Arguments)
	}
	e := decode(t, rec)
	if e.Code != 0 || e.Data["name"] != "canvas_update_nodes" || e.Data["is_error"] != false || e.Data["content"] == nil {
		t.Fatalf("出参: %s", rec.Body.String())
	}
	if _, aliased := e.Data["callId"]; aliased {
		t.Fatal("出参不应补驼峰别名")
	}
}

func TestInternalCallRejects(t *testing.T) {
	body := `{"name":"canvas_list","arguments":{},"call_id":"turn-9:tool-1"}`
	cases := []struct {
		name   string
		req    func() *http.Request
		status int
		reason string
	}{
		{"错签", func() *http.Request { return internalReq(body, "nonce-internal-bad-1", 42, testSecret+"x") }, 401, svcauth.ReasonBadSignature},
		{"缺 user", func() *http.Request {
			r := internalReq(body, "nonce-internal-nouser", 42, testSecret)
			r.Header.Del(svcauth.HeaderUserID)
			return r
		}, 401, svcauth.ReasonUserMissing},
		{"过期", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(body))
			svcauth.SignRequest(r, "java-1", testSecret, 42, "nonce-internal-expire", time.Now().Add(-10*time.Minute), []byte(body))
			return r
		}, 401, svcauth.ReasonSkew},
		{"未知 key", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(body))
			svcauth.SignRequest(r, "java-9", testSecret, 42, "nonce-internal-unkkey", time.Now(), []byte(body))
			return r
		}, 401, svcauth.ReasonUnknownKey},
		{"只带用户令牌", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef")
			r.Header.Set("X-User-Id", "42")
			return r
		}, 401, svcauth.ReasonMissing},
		{"请求体不是 Function Call", func() *http.Request { return internalReq(`{"foo":1}`, "nonce-internal-badbody", 42, testSecret) }, 400, "PARAM_INVALID"},
		{"缺 call_id", func() *http.Request {
			return internalReq(`{"name":"canvas_list","arguments":{}}`, "nonce-internal-nocall", 42, testSecret)
		}, 400, "CALL_ID_REQUIRED"},
		{"缺 call_id 时 Idempotency-Key / X-Request-Id 头不顶替", func() *http.Request {
			r := internalReq(`{"name":"canvas_list","arguments":{}}`, "nonce-internal-idemhd", 42, testSecret)
			r.Header.Set("Idempotency-Key", "k-1")
			r.Header.Set("X-Request-Id", "k-1")
			return r
		}, 400, "CALL_ID_REQUIRED"},
		{"call_id 超长", func() *http.Request {
			return internalReq(`{"name":"canvas_list","arguments":{},"call_id":"`+strings.Repeat("x", 65)+`"}`, "nonce-internal-longid", 42, testSecret)
		}, 400, "PARAM_INVALID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rpc := &fakeRPC{}
			call, _ := internalChain(t, rpc)
			rec := httptest.NewRecorder()
			call(rec, c.req())
			e := decode(t, rec)
			if rec.Code != c.status || e.Reason != c.reason || len(rpc.got) != 0 || e.Message == "" {
				t.Fatalf("应 %d %s 且不执行: %d %s rpc=%d", c.status, c.reason, rec.Code, rec.Body.String(), len(rpc.got))
			}
		})
	}
	t.Run("重放", func(t *testing.T) {
		rpc := &fakeRPC{}
		call, _ := internalChain(t, rpc)
		r1 := internalReq(body, "nonce-internal-replay", 42, testSecret)
		r2 := internalReq(body, "nonce-internal-replay", 42, testSecret)
		rec := httptest.NewRecorder()
		call(rec, r1)
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		rec = httptest.NewRecorder()
		call(rec, r2)
		if rec.Code != 401 || decode(t, rec).Reason != svcauth.ReasonReplay || len(rpc.got) != 1 {
			t.Fatalf("重放应 401: %s", rec.Body.String())
		}
	})
}

func TestInternalTools(t *testing.T) {
	_, tools := internalChain(t, &fakeRPC{})
	r := httptest.NewRequest(http.MethodGet, InternalPrefix+"/agent/tools?format=openai", nil)
	svcauth.SignRequest(r, "java-1", testSecret, 0, "nonce-internal-tools1", time.Now(), nil)
	rec := httptest.NewRecorder()
	tools(rec, r)
	e := decode(t, rec)
	if rec.Code != 200 || e.Data["format"] != "openai" {
		t.Fatalf("工具清单: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	tools(rec, httptest.NewRequest(http.MethodGet, InternalPrefix+"/agent/tools", nil))
	if rec.Code != 401 {
		t.Fatalf("未签名应 401: %d", rec.Code)
	}
}

func TestNonceStoreByMode(t *testing.T) {
	if nonceStore("pro", nil) != nil || nonceStore("pre", nil) != nil || nonceStore("rt", nil) != nil {
		t.Fatal("非开发模式没有 Redis 时不应注册（nonce 存储为 nil）")
	}
	if _, ok := nonceStore("dev", nil).(*svcauth.MemoryNonces); !ok {
		t.Fatal("dev 模式退回进程内")
	}
	if _, ok := nonceStore("test", nil).(*svcauth.MemoryNonces); !ok {
		t.Fatal("test 模式退回进程内")
	}
}

// 审计 IP 用 pkg/clientip：直连对端不是可信代理时，客户端自带的 X-Forwarded-For / X-Real-IP 不认。
func TestAuditIPUsesClientIP(t *testing.T) {
	rpc := &fakeRPC{}
	s := testSvc(rpc, 10, 10)
	h := enabled(s, CallHandler(s))
	body := `{"name":"canvas_list","arguments":{}}`
	req := withUser(httptest.NewRequest(http.MethodPost, "/api/v1/canvas/agent/call", strings.NewReader(body)), 42)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "1.2.3.4")
	h(httptest.NewRecorder(), req)
	if len(rpc.got) != 1 || rpc.got[0].ClientIp != "203.0.113.9" {
		t.Fatalf("审计 IP 应取直连对端: %+v", rpc.got)
	}
	// 自报 svc: 前缀被转义
	req = withUser(httptest.NewRequest(http.MethodPost, "/api/v1/canvas/agent/call", strings.NewReader(body)), 42)
	req.Header.Set("X-Agent-Name", "svc:java-1")
	h(httptest.NewRecorder(), req)
	if got := rpc.got[1]; got.Client != "function" || got.Agent != "svc_java-1" {
		t.Fatalf("用户入口不得冒充服务调用: %+v", got)
	}
}

func TestInternalNotRegisteredWithoutKeys(t *testing.T) {
	var cfg config.CanvasInternalConf
	if BuildVerifier(cfg, svcauth.NewMemoryNonces()) != nil {
		t.Fatal("没配 key 不应注册")
	}
	cfg.Keys = []config.CanvasInternalKey{{KeyId: "java-1", Secret: "too-short"}}
	if BuildVerifier(cfg, svcauth.NewMemoryNonces()) != nil {
		t.Fatal("secret 不合格不应注册")
	}
}

// PreserveInternalBody 只管 /internal/v1/canvas/，其它路径照旧被 jsoncase 改写。
func TestPreserveOnlyInternal(t *testing.T) {
	var seen string
	h := PreserveInternalBody(func(w http.ResponseWriter, r *http.Request) {
		_, ok := r.Context().Value(rawBodyKey{}).(*rawBody)
		seen = map[bool]string{true: "kept", false: "none"}[ok]
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/canvas/agent/call", strings.NewReader("{}")))
	if seen != "none" {
		t.Fatal("非内部路径不应处理")
	}
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, InternalPrefix+"/agent/call", strings.NewReader("{}")))
	if seen != "kept" {
		t.Fatal("内部路径应存原始 body")
	}
}
