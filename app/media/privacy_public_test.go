package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// 用户输入不能经任何"原样落桶"的路由发布到公有桶：ingest 的 rid / key 来源与 copy 用同一判据（403 purpose.user_input_not_public），
// 且判据在读写任何对象之前执行（计数存储证明没有 Put）。

func newPrivacyFixture(t *testing.T) (*Server, *countingStore, *httptest.Server, string) {
	t.Helper()
	requireFFmpeg(t)
	srv, cs := newCachedServer(t, filepath.Join(t.TempDir(), "objects"), 0)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	makeAVClip(t, clip)
	return srv, cs, ts, clip
}

func storeClip(t *testing.T, srv *Server, purpose, key, clip string) MediaResult {
	t.Helper()
	res, err := srv.storeFile(withIdemKey(context.Background(), key), purpose, clip)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func postIngestSource(t *testing.T, base string, src map[string]any, purpose, key string) *http.Response {
	t.Helper()
	return postWithKey(t, base, "/v1/mediasvc/ingest", map[string]any{"source": src, "purpose": purpose}, testToken, key)
}

func TestIngestRejectsUserRidToPublicStores(t *testing.T) {
	srv, cs, ts, clip := newPrivacyFixture(t)
	user := storeClip(t, srv, "user_input", "k-priv-user", clip)
	if user.Object.Class != classUser {
		t.Fatalf("前置：应得到 user 类别: %+v", user.Object)
	}
	for _, purpose := range []string{"asset_library", "third_party"} {
		cs.puts.Store(0)
		fetchesBefore := cs.fetches.Load()
		body := assertAPIError(t, postIngestSource(t, ts.URL, map[string]any{"rid": user.Object.RID}, purpose, "k-priv-"+purpose),
			http.StatusForbidden, "forbidden_purpose")
		if body.Error.Check != "purpose.user_input_not_public" {
			t.Fatalf("%s: check = %q, want purpose.user_input_not_public", purpose, body.Error.Check)
		}
		if n := cs.puts.Load(); n != 0 {
			t.Fatalf("%s: 被拒的 ingest 不应落桶，Put 次数 = %d", purpose, n)
		}
		if n := cs.fetches.Load() - fetchesBefore; n != 0 {
			t.Fatalf("%s: 被拒的 ingest 不应读取源对象，Fetch 次数 = %d", purpose, n)
		}
	}
}

// key 来源指向 user 类别路径同样拒绝（同一判据）。
func TestIngestRejectsUserKeyToPublicStore(t *testing.T) {
	srv, cs, ts, clip := newPrivacyFixture(t)
	user := storeClip(t, srv, "user_input", "k-priv-user-key", clip)
	path, found, err := srv.readIndex(context.Background(), user.Object.RID, classUser)
	if err != nil || !found {
		t.Fatalf("前置：user 对象没有索引: %v", err)
	}
	cs.puts.Store(0)
	body := assertAPIError(t, postIngestSource(t, ts.URL, map[string]any{"key": path, "store": storePrivate}, "asset_library", "k-priv-key"),
		http.StatusForbidden, "forbidden_purpose")
	if body.Error.Check != "purpose.user_input_not_public" || cs.puts.Load() != 0 {
		t.Fatalf("check=%q puts=%d", body.Error.Check, cs.puts.Load())
	}
}

// user 类别 rid 落私有用途（intermediate）照常允许；work 类别 rid 落素材库照常允许。
func TestIngestRidAllowedWhenNotPublishingUserInput(t *testing.T) {
	srv, cs, ts, clip := newPrivacyFixture(t)
	user := storeClip(t, srv, "user_input", "k-priv-user-ok", clip)
	cs.puts.Store(0)
	res := decodeMediaResult(t, postIngestSource(t, ts.URL, map[string]any{"rid": user.Object.RID}, "intermediate", "k-priv-mid"))
	if res.Object.Class != classWork || res.Object.Store != storePrivate || cs.puts.Load() == 0 {
		t.Fatalf("user→intermediate 应允许并落私有桶: %+v puts=%d", res.Object, cs.puts.Load())
	}

	work := storeClip(t, srv, "intermediate", "k-priv-work", clip)
	cs.puts.Store(0)
	res = decodeMediaResult(t, postIngestSource(t, ts.URL, map[string]any{"rid": work.Object.RID}, "asset_library", "k-priv-asset"))
	if res.Object.Class != classAsset || res.Object.Store != storeAsset || cs.puts.Load() == 0 {
		t.Fatalf("work→asset_library 应允许并落素材库: %+v puts=%d", res.Object, cs.puts.Load())
	}
}

// copy 的行为不变：user 来源仍 403 同一判据，不落桶。
func TestCopyUserRidStillRejectedWithoutWrite(t *testing.T) {
	srv, cs, ts, clip := newPrivacyFixture(t)
	user := storeClip(t, srv, "user_input", "k-priv-copy-user", clip)
	cs.puts.Store(0)
	body := assertAPIError(t, postCopy(t, ts.URL, user.Object.RID, "asset_library", "k-priv-copy"), http.StatusForbidden, "forbidden_purpose")
	if body.Error.Check != "purpose.user_input_not_public" || cs.puts.Load() != 0 {
		t.Fatalf("check=%q puts=%d", body.Error.Check, cs.puts.Load())
	}
}

// 纯函数判据：只有 user 来源 + 公有用途拒绝。
func TestCheckUserInputNotPublic(t *testing.T) {
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	userRid, err := id.RID("tst001", classUser, "u")
	if err != nil {
		t.Fatal(err)
	}
	workRid, err := id.RID("tst001", classWork, "w")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		src     SourceRef
		purpose string
		reject  bool
	}{
		{SourceRef{Rid: userRid}, "asset_library", true},
		{SourceRef{Rid: userRid}, "third_party", true},
		{SourceRef{Rid: userRid}, "intermediate", false},
		{SourceRef{Rid: userRid}, "user_input", false},
		{SourceRef{Rid: workRid}, "asset_library", false},
		{SourceRef{Url: "https://example.com/a.mp4"}, "asset_library", false},
	}
	for _, c := range cases {
		if got := checkUserInputNotPublic(c.src, c.purpose) != nil; got != c.reject {
			t.Fatalf("%+v %s: reject=%v, want %v", c.src, c.purpose, got, c.reject)
		}
	}
}
