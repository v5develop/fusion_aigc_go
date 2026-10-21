package media

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// 直传的用户对象：按 rid 推出落桶路径，模拟客户端已经把字节直传到该位置。
func putUserObject(t *testing.T, f *apiFixture, rid string, content []byte) {
	t.Helper()
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.objects, storePrivate, filepath.FromSlash(storePath(classUser, id.ObjectKey(rid))))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func userRID(t *testing.T, key string) string {
	t.Helper()
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := id.RID("tst001", classUser, key)
	if err != nil {
		t.Fatal(err)
	}
	return rid
}

// 合法视频直传后 register：返回 mime、大小、sha256 与媒体属性，url 为空（kind none）。
func TestRegisterValidVideoReturnsMime(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	clip := filepath.Join(f.dir, "clip.mp4")
	makeAVClip(t, clip)
	b, err := os.ReadFile(clip)
	if err != nil {
		t.Fatal(err)
	}
	rid := userRID(t, "k-reg-ok")
	putUserObject(t, f, rid, b)

	res := decodeMediaResult(t, post(t, f.ts.URL, "/v1/mediasvc/resources/register", map[string]any{"rid": rid}, testToken))
	if res.Object.RID != rid || res.Object.Class != classUser || res.Object.Store != storePrivate {
		t.Fatalf("对象引用不对: %+v", res.Object)
	}
	if res.Object.Mime != "video/mp4" {
		t.Fatalf("mime = %q, want video/mp4", res.Object.Mime)
	}
	if res.Object.Size != int64(len(b)) || res.Object.Sha256 != fileSHA(t, clip) {
		t.Fatalf("大小或 sha256 不对: size=%d sha=%s", res.Object.Size, res.Object.Sha256)
	}
	if res.Object.DurationS <= 0 || res.Object.Width <= 0 || res.Object.Height <= 0 {
		t.Fatalf("媒体属性缺失: %+v", res.Object)
	}
	if res.Url.Kind != "none" || res.Url.Value != "" {
		t.Fatalf("register 不应返回地址: %+v", res.Url)
	}
}

// 非法字节：422 source_rejected，且对象仍在（不自动删除用户对象）。
func TestRegisterRejectsInvalidBytesAndKeepsObject(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	rid := userRID(t, "k-reg-bad")
	garbage := []byte("this is not a media file at all, just text bytes ............")
	putUserObject(t, f, rid, garbage)

	resp := post(t, f.ts.URL, "/v1/mediasvc/resources/register", map[string]any{"rid": rid}, testToken)
	assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")

	id, _ := newIdentity("tst001", testSecret)
	p := filepath.Join(f.objects, storePrivate, filepath.FromSlash(storePath(classUser, id.ObjectKey(rid))))
	if _, err := os.Stat(p); err != nil {
		t.Fatal("非法对象必须保留，由调用方决定是否删除")
	}
}

// 对象不存在：404 not_found（不是 source_unavailable）。
func TestRegisterMissingObjectIs404(t *testing.T) {
	f := newAPIFixture(t)
	rid := userRID(t, "k-reg-missing")
	resp := post(t, f.ts.URL, "/v1/mediasvc/resources/register", map[string]any{"rid": rid}, testToken)
	assertAPIError(t, resp, http.StatusNotFound, "not_found")
}

// 目标对象已存在时申请直传返回 409 conflict，不签发地址、不覆盖已登记的对象。
func TestUploadTargetExistingObjectIs409(t *testing.T) {
	f := newAPIFixture(t)
	key := "k-up-dup"
	putUserObject(t, f, userRID(t, key), []byte("already uploaded"))

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/resources/upload-target", map[string]any{
		"class": classUser, "store": storePrivate, "idem_key": key, "mime": "video/mp4", "size": 16,
	}, testToken, "")
	assertAPIError(t, resp, http.StatusConflict, codeConflict)
}
