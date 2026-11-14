package svcauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	secretA = "0123456789abcdef0123456789abcdef-A"
	secretB = "0123456789abcdef0123456789abcdef-B"
	path    = "/internal/v1/canvas/agent/call"
)

func newV(t *testing.T, now time.Time) *Verifier {
	t.Helper()
	v, skipped := NewVerifier([]Key{{"java-2026a", secretA}, {"java-2026b", secretB}, {"short", "x"}, {"bad id!", secretA}},
		300*time.Second, 600*time.Second, NewMemoryNonces())
	if v == nil || len(skipped) != 2 {
		t.Fatalf("应加载两把合格 key、跳过两把: %v %v", v, skipped)
	}
	v.SetClock(func() time.Time { return now })
	return v
}

func signed(keyID, secret string, uid int64, nonce string, ts time.Time, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	SignRequest(r, keyID, secret, uid, nonce, ts, []byte(body))
	return r
}

func reasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// 固定向量：Java 侧实现照此自测（文档 §12 示例同值）。
func TestKnownVector(t *testing.T) {
	body := []byte(`{"name":"canvas_get","arguments":{"canvas_id":"cv_0123456789abcdef"},"call_id":"turn-1:tool-1"}`)
	sts := StringToSign("post", path, "1794600000", "n0nce-0123456789abcdef", "42", body)
	want := "POST\n" + path + "\n1794600000\nn0nce-0123456789abcdef\n42\n"
	if !strings.HasPrefix(sts, want) || len(sts) != len(want)+64 {
		t.Fatalf("待签名串: %q", sts)
	}
	if got := Sign("0123456789abcdef0123456789abcdef", "POST", path, "1794600000", "n0nce-0123456789abcdef", "42", body); got != knownSig {
		t.Fatalf("签名向量变了: %s", got)
	}
}

func TestVerify(t *testing.T) {
	now := time.Unix(1794600000, 0)
	body := `{"name":"canvas_list","arguments":{}}`
	ctx := context.Background()

	t.Run("ok + 轮换的两把 key 都认", func(t *testing.T) {
		v := newV(t, now)
		for i, k := range [][2]string{{"java-2026a", secretA}, {"java-2026b", secretB}} {
			id, err := v.Verify(ctx, signed(k[0], k[1], 42, "nonce-ok-000000000"+string(rune('a'+i)), now.Add(-299*time.Second), body), []byte(body), true)
			if err != nil || id.KeyID != k[0] || id.UserID != 42 {
				t.Fatalf("%s 应通过: %+v %v", k[0], id, err)
			}
		}
	})
	cases := []struct {
		name   string
		req    func() *http.Request
		body   string
		user   bool
		reason string
	}{
		{"错签（用别的 key 的 secret）", func() *http.Request { return signed("java-2026a", secretB, 42, "nonce-bad-sig-00001", now, body) }, body, true, ReasonBadSignature},
		{"body 被改", func() *http.Request { return signed("java-2026a", secretA, 42, "nonce-tamper-000001", now, body) }, `{"name":"canvas_delete_nodes"}`, true, ReasonBadSignature},
		{"user 被改", func() *http.Request {
			r := signed("java-2026a", secretA, 42, "nonce-user-swap-001", now, body)
			r.Header.Set(HeaderUserID, "43")
			return r
		}, body, true, ReasonBadSignature},
		{"路径不同", func() *http.Request {
			r := signed("java-2026a", secretA, 42, "nonce-path-0000001", now, body)
			r.URL.Path = "/internal/v1/canvas/agent/tools"
			return r
		}, body, true, ReasonBadSignature},
		{"过期", func() *http.Request {
			return signed("java-2026a", secretA, 42, "nonce-expired-00001", now.Add(-301*time.Second), body)
		}, body, true, ReasonSkew},
		{"未来", func() *http.Request {
			return signed("java-2026a", secretA, 42, "nonce-future-000001", now.Add(301*time.Second), body)
		}, body, true, ReasonSkew},
		{"未知 key", func() *http.Request { return signed("java-old", secretA, 42, "nonce-unknown-00001", now, body) }, body, true, ReasonUnknownKey},
		{"不合格被跳过的 key 也是未知", func() *http.Request { return signed("short", "x", 42, "nonce-short-0000001", now, body) }, body, true, ReasonUnknownKey},
		{"缺 user", func() *http.Request {
			r := signed("java-2026a", secretA, 42, "nonce-no-user-00001", now, body)
			r.Header.Del(HeaderUserID)
			return r
		}, body, true, ReasonUserMissing},
		{"user=0 且要求用户", func() *http.Request { return signed("java-2026a", secretA, 0, "nonce-user-zero-001", now, body) }, body, true, ReasonUserMissing},
		{"user 非数字", func() *http.Request {
			r := signed("java-2026a", secretA, 42, "nonce-user-nan-0001", now, body)
			r.Header.Set(HeaderUserID, "42abc")
			return r
		}, body, true, ReasonUserMissing},
		{"缺签名", func() *http.Request {
			r := signed("java-2026a", secretA, 42, "nonce-no-sig-000001", now, body)
			r.Header.Del(HeaderSignature)
			return r
		}, body, true, ReasonMissing},
		{"nonce 太短", func() *http.Request { return signed("java-2026a", secretA, 42, "short", now, body) }, body, true, ReasonMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := newV(t, now)
			_, err := v.Verify(ctx, c.req(), []byte(c.body), c.user)
			if reasonOf(err) != c.reason {
				t.Fatalf("应拒绝 %s，得到 %v", c.reason, err)
			}
		})
	}
	t.Run("重放", func(t *testing.T) {
		v := newV(t, now)
		r1 := signed("java-2026a", secretA, 42, "nonce-replay-000001", now, body)
		if _, err := v.Verify(ctx, r1, []byte(body), true); err != nil {
			t.Fatal(err)
		}
		r2 := signed("java-2026a", secretA, 42, "nonce-replay-000001", now, body)
		if _, err := v.Verify(ctx, r2, []byte(body), true); reasonOf(err) != ReasonReplay {
			t.Fatalf("同 nonce 第二次应判重放: %v", err)
		}
		// 不同 key 下同 nonce 互不影响
		r3 := signed("java-2026b", secretB, 42, "nonce-replay-000001", now, body)
		if _, err := v.Verify(ctx, r3, []byte(body), true); err != nil {
			t.Fatalf("另一把 key 的同名 nonce 应通过: %v", err)
		}
	})
	t.Run("错签不占 nonce", func(t *testing.T) {
		v := newV(t, now)
		bad := signed("java-2026a", secretB, 42, "nonce-not-burned-01", now, body)
		if _, err := v.Verify(ctx, bad, []byte(body), true); reasonOf(err) != ReasonBadSignature {
			t.Fatal(err)
		}
		good := signed("java-2026a", secretA, 42, "nonce-not-burned-01", now, body)
		if _, err := v.Verify(ctx, good, []byte(body), true); err != nil {
			t.Fatalf("伪造请求不应烧掉真 nonce: %v", err)
		}
	})
	t.Run("tools 可不带用户（0）", func(t *testing.T) {
		v := newV(t, now)
		r := httptest.NewRequest(http.MethodGet, "/internal/v1/canvas/agent/tools?format=openai", nil)
		SignRequest(r, "java-2026a", secretA, 0, "nonce-tools-000001", now, nil)
		if id, err := v.Verify(ctx, r, nil, false); err != nil || id.UserID != 0 {
			t.Fatalf("GET tools 应通过: %v", err)
		}
	})
	t.Run("nonce 存储故障", func(t *testing.T) {
		v, _ := NewVerifier([]Key{{"java-2026a", secretA}}, 300*time.Second, 600*time.Second, brokenNonces{})
		v.SetClock(func() time.Time { return now })
		_, err := v.Verify(ctx, signed("java-2026a", secretA, 42, "nonce-store-down-01", now, body), []byte(body), true)
		if !IsServerSide(err) {
			t.Fatalf("nonce 存储故障应是服务端错误（拒绝而非放行）: %v", err)
		}
	})
}

func TestNoUsableKey(t *testing.T) {
	if v, _ := NewVerifier(nil, 0, 0, NewMemoryNonces()); v != nil {
		t.Fatal("没有 key 不应建出校验器")
	}
	v, _ := NewVerifier([]Key{{"k", secretA}}, 300*time.Second, 10*time.Second, NewMemoryNonces())
	if v.nonceTTL < 630*time.Second {
		t.Fatalf("nonce TTL 应至少覆盖整个时间窗: %s", v.nonceTTL)
	}
}

type brokenNonces struct{}

func (brokenNonces) Claim(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("redis down")
}

// knownSig 文档 §12.3 示例的签名（Java 侧单测用同一组输入比对）。
const knownSig = "d0764d169799dae9acef87b4961fe1e6e6fb0a2803fab46640c211915f83991d"
