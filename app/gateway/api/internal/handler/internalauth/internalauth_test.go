package internalauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/svcauth"
	"aigc-local-life/app/gateway/api/internal/config"
)

// PreserveInternalBody 对 /internal/v1/ 下的所有路径生效（画布、复刻智能体都在其下），其它路径照旧被 jsoncase 改写。
func TestPreserveAllInternalPaths(t *testing.T) {
	var seen string
	h := PreserveInternalBody(func(w http.ResponseWriter, r *http.Request) {
		_, ok := r.Context().Value(rawBodyKey{}).(*rawBody)
		seen = map[bool]string{true: "kept", false: "none"}[ok]
	})
	for path, want := range map[string]string{
		"/api/v1/canvas/agent/call":         "none",
		"/api/v1/replicate/plans":           "none",
		"/internal/v1":                      "none",
		"/internal/v1/canvas/agent/call":    "kept",
		"/internal/v1/replicate/agent/call": "kept",
		"/internal/v1/anything/else":        "kept",
	} {
		seen = ""
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if seen != want {
			t.Fatalf("%s：%s，期望 %s", path, seen, want)
		}
	}
}

func TestNonceStoreByMode(t *testing.T) {
	if NonceStore("pro", nil) != nil || NonceStore("pre", nil) != nil || NonceStore("rt", nil) != nil {
		t.Fatal("非开发模式没有 Redis 时不应注册（nonce 存储为 nil）")
	}
	if _, ok := NonceStore("dev", nil).(*svcauth.MemoryNonces); !ok {
		t.Fatal("dev 模式退回进程内")
	}
	if _, ok := NonceStore("test", nil).(*svcauth.MemoryNonces); !ok {
		t.Fatal("test 模式退回进程内")
	}
}

func TestBuildVerifierNeedsQualifiedKeys(t *testing.T) {
	var cfg config.CanvasInternalConf
	if BuildVerifier(cfg, svcauth.NewMemoryNonces()) != nil {
		t.Fatal("没配 key 不应注册")
	}
	cfg.Keys = []config.CanvasInternalKey{{KeyId: "java-1", Secret: "too-short"}}
	if BuildVerifier(cfg, svcauth.NewMemoryNonces()) != nil {
		t.Fatal("secret 不合格不应注册")
	}
}
