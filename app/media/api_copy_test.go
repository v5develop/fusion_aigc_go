package media

import (
	"net/http"
	"path/filepath"
	"testing"
)

// 幂等缓存默认 TTL 为 0（测试配置不开缓存），复制的重放测试需要开启缓存的实例。
func newCopyFixture(t *testing.T) *apiFixture {
	t.Helper()
	dir := t.TempDir()
	origin := fileServer(t, dir)
	objects := filepath.Join(t.TempDir(), "objects")
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: objects, IdempotencyMinutes: 10})
	return &apiFixture{dir: dir, origin: origin, ts: ts, objects: objects}
}

// 复制用的源：先 ingest 一段真实视频拿到中间产物的 rid（用户输入不能复制到公有桶，见下面的用例）。
func ingestCopySource(t *testing.T, f *apiFixture) MediaResult {
	t.Helper()
	makeAVClip(t, filepath.Join(f.dir, "src.mp4"))
	return decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("src.mp4"), "purpose": "intermediate"}, testToken, "k-copy-src"))
}

// 用户输入的 rid 不能复制到公有桶：复制会让私有素材公开可读。
func TestCopyRejectsUserInputSource(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "u.mp4"))
	user := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("u.mp4"), "purpose": "user_input"}, testToken, "k-copy-user-src"))

	resp := postCopy(t, f.ts.URL, user.Object.RID, "asset_library", "k-copy-user")
	assertAPIError(t, resp, http.StatusForbidden, "forbidden_purpose")
}

func postCopy(t *testing.T, base string, rid, purpose, key string) *http.Response {
	t.Helper()
	return postWithKey(t, base, "/v1/mediasvc/resources/copy", map[string]any{
		"rid": rid, "purpose": purpose}, testToken, key)
}

// 复制到素材库：新 rid 与源不同、内容 sha256 一致、parents 指向源 rid。
func TestCopyToAssetLibraryReturnsNewRidWithParents(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	src := ingestCopySource(t, f)

	res := decodeMediaResult(t, postCopy(t, f.ts.URL, src.Object.RID, "asset_library", "k-copy-1"))
	if res.Object.RID == "" || res.Object.RID == src.Object.RID {
		t.Fatalf("复制必须得到新 rid: src=%s new=%s", src.Object.RID, res.Object.RID)
	}
	if res.Object.Class != classAsset || res.Object.Store != storeAsset {
		t.Fatalf("素材库复制的类别/存储不对: %+v", res.Object)
	}
	if res.Object.Sha256 != src.Object.Sha256 {
		t.Fatal("复制后的 sha256 与源不一致")
	}
	if got := fileSHA(t, filepath.Join(f.objects, storeAsset, filepath.FromSlash(pathOfRID(t, res.Object.RID)))); got != src.Object.Sha256 {
		t.Fatal("落桶内容与源内容不一致")
	}
	if len(res.Parents) != 1 || res.Parents[0] != src.Object.RID {
		t.Fatalf("parents = %v, want [%s]", res.Parents, src.Object.RID)
	}
}

// 复制到第三方交付：类别为 deliver，存储为第三方桶。
func TestCopyToThirdPartyUsesDeliverClass(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	src := ingestCopySource(t, f)

	res := decodeMediaResult(t, postCopy(t, f.ts.URL, src.Object.RID, "third_party", "k-copy-3p"))
	if res.Object.Class != classDeliver || res.Object.Store != storeThirdParty {
		t.Fatalf("第三方复制的类别/存储不对: %+v", res.Object)
	}
	if len(res.Parents) != 1 || res.Parents[0] != src.Object.RID {
		t.Fatalf("parents = %v", res.Parents)
	}
}

// 同一幂等键重放：得到同一个 rid，并标记为回放。
func TestCopyIdempotentReplayReturnsSameRid(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyFixture(t)
	src := ingestCopySource(t, f)

	first := decodeMediaResult(t, postCopy(t, f.ts.URL, src.Object.RID, "asset_library", "k-copy-replay"))
	resp := postCopy(t, f.ts.URL, src.Object.RID, "asset_library", "k-copy-replay")
	second := decodeMediaResult(t, resp)
	if second.Object.RID != first.Object.RID {
		t.Fatalf("重放的 rid 不同: %s vs %s", first.Object.RID, second.Object.RID)
	}
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("同一幂等键的重放应带 Idempotent-Replayed")
	}
}

// 源不存在：404。
func TestCopyMissingSourceIs404(t *testing.T) {
	f := newAPIFixture(t)
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := id.RID("tst001", classWork, "never-stored")
	if err != nil {
		t.Fatal(err)
	}
	resp := postCopy(t, f.ts.URL, rid, "asset_library", "k-copy-missing")
	assertAPIError(t, resp, http.StatusNotFound, "not_found")
}

// 目标 user 类别（及其它非法用途）被拒；缺幂等键也被拒。
func TestCopyRejectsUserTargetAndMissingKey(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	src := ingestCopySource(t, f)

	for _, purpose := range []string{"user_input", "intermediate"} {
		resp := postCopy(t, f.ts.URL, src.Object.RID, purpose, "k-copy-bad-"+purpose)
		assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
	}
	resp := postCopy(t, f.ts.URL, src.Object.RID, "asset_library", "")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

// 第三方交付的产物（deliver 类别，落在第三方桶）可以再次复制：私有桶没有时回退查第三方桶。
func TestCopyDeliverSourceFromThirdPartyBucket(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	src := ingestCopySource(t, f)
	deliver := decodeMediaResult(t, postCopy(t, f.ts.URL, src.Object.RID, "third_party", "k-copy-deliver-src"))
	if deliver.Object.Store != storeThirdParty {
		t.Fatalf("前置条件：deliver 应落在第三方桶，实际 %s", deliver.Object.Store)
	}

	res := decodeMediaResult(t, postCopy(t, f.ts.URL, deliver.Object.RID, "asset_library", "k-copy-deliver-2"))
	if res.Object.Class != classAsset || res.Object.Store != storeAsset {
		t.Fatalf("复制结果的类别/存储不对: %+v", res.Object)
	}
	if res.Object.Sha256 != src.Object.Sha256 {
		t.Fatal("复制后的内容与源不一致")
	}
	if len(res.Parents) != 1 || res.Parents[0] != deliver.Object.RID {
		t.Fatalf("parents = %v, want [%s]", res.Parents, deliver.Object.RID)
	}
}

// deliver 对象在私有桶与第三方桶都不存在：404。
func TestCopyDeliverMissingInBothBucketsIs404(t *testing.T) {
	f := newAPIFixture(t)
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := id.RID("tst001", classDeliver, "never-stored-deliver")
	if err != nil {
		t.Fatal(err)
	}
	resp := postCopy(t, f.ts.URL, rid, "asset_library", "k-copy-deliver-missing")
	assertAPIError(t, resp, http.StatusNotFound, "not_found")
}
