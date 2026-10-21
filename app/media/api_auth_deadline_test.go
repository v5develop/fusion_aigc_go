package media

import (
	"net/http"
	"path/filepath"
	"testing"
)

// 服务间令牌：Authorization: Bearer <token> 与 X-Media-Token 等价。
func TestBearerTokenAuthorizes(t *testing.T) {
	ts := newTestServer(t, Config{ObjectDir: t.TempDir()})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mediasvc/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp := send(t, req, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer 令牌应通过鉴权，status = %d", resp.StatusCode)
	}
	bad, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mediasvc/capabilities", nil)
	bad.Header.Set("Authorization", "Bearer wrong")
	if got := send(t, bad, "").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("错误的 Bearer 令牌 status = %d, want 401", got)
	}
}

// deadline_ms 很短：素材读取与处理来不及完成，返回 503 transient（可用同一个键重试）。
func TestDeadlineMsCancelsWithTransient(t *testing.T) {
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/trim", map[string]any{
		"source": f.url("a.mp4"), "seconds": 2, "purpose": "intermediate", "deadline_ms": 1,
	}, testToken, "k-deadline")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("deadline_ms=1 的请求 status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("503 必须带 Retry-After")
	}
}

// 没有 deadline_ms 的请求不受影响。
func TestNoDeadlineKeepsNormalBehaviour(t *testing.T) {
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/trim", map[string]any{
		"source": f.url("a.mp4"), "seconds": 2, "purpose": "intermediate",
	}, testToken, "k-nodeadline")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
