package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// 字节流上传的测试用例需要 ffmpeg 生成合法媒体，并且要有幂等与体积上限，所以用独立的夹具。
func newBytesServer(t *testing.T, maxBytes int64) *http.Server {
	t.Helper()
	return nil
}

func uploadBytes(t *testing.T, base, purpose, key string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/mediasvc/ingest/upload?purpose="+purpose, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func clipBytes(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp4")
	makeAVClip(t, path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 字节上传：合法视频落桶，返回 rid 与摘要；摘要与原始字节一致。
func TestUploadBytesStoresValidatedMedia(t *testing.T) {
	requireFFmpeg(t)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), IdempotencyMinutes: 10})
	data := clipBytes(t)

	resp := uploadBytes(t, ts.URL, "intermediate", "k-bytes-1", data)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body = %s", resp.StatusCode, b)
	}
	var out MediaResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object.RID == "" || out.Object.Class != classWork {
		t.Fatalf("结果 = %+v", out.Object)
	}
	if out.Object.Sha256 != sha256Hex(data) {
		t.Fatalf("sha256 与原始字节不一致: %s", out.Object.Sha256)
	}
}

// 同一个键 + 同一份字节：重放返回同一个结果。
func TestUploadBytesReplaysSameKeyAndBody(t *testing.T) {
	requireFFmpeg(t)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), IdempotencyMinutes: 10})
	data := clipBytes(t)

	first := decodeMediaResult(t, uploadBytes(t, ts.URL, "intermediate", "k-bytes-2", data))
	resp := uploadBytes(t, ts.URL, "intermediate", "k-bytes-2", data)
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("同键同体应标记为重放")
	}
	second := decodeMediaResult(t, resp)
	if second.Object.RID != first.Object.RID {
		t.Fatalf("重放的 rid 应一致: %s vs %s", second.Object.RID, first.Object.RID)
	}
}

// 同一个键但字节不同：拒绝，不覆盖原结果。
func TestUploadBytesRejectsKeyReuseWithDifferentBody(t *testing.T) {
	requireFFmpeg(t)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), IdempotencyMinutes: 10})
	data := clipBytes(t)
	_ = decodeMediaResult(t, uploadBytes(t, ts.URL, "intermediate", "k-bytes-3", data))

	resp := uploadBytes(t, ts.URL, "intermediate", "k-bytes-3", append(append([]byte(nil), data...), 0x00))
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

// 不是合法媒体的字节：拒收为素材被拒，并且不占用幂等键（之后可以用同一个键重试）。
func TestUploadBytesRejectsGarbageAndAllowsRetry(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), IdempotencyMinutes: 10})
	garbage := []byte("this is not a video at all, just text")

	assertAPIError(t, uploadBytes(t, ts.URL, "intermediate", "k-bytes-4", garbage), http.StatusUnprocessableEntity, "source_rejected")
	// 失败不缓存：同一个键的重试会重新执行（这里仍然是垃圾，仍然拒收，但不是"处理中"）
	assertAPIError(t, uploadBytes(t, ts.URL, "intermediate", "k-bytes-4", garbage), http.StatusUnprocessableEntity, "source_rejected")
}

// 用户输入超过体积上限：拒收为 source_rejected（size），不把超出的部分写进磁盘。
func TestUploadBytesEnforcesSizeLimit(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), MaxInputBytes: 1024})
	big := bytes.Repeat([]byte{0xAB}, 4096)

	body := assertAPIError(t, uploadBytes(t, ts.URL, "user_input", "k-bytes-5", big), http.StatusUnprocessableEntity, "source_rejected")
	if body.Error.Check != "source.size_exceeded" {
		t.Fatalf("用户输入超限的判据应为 source.size_exceeded: %+v", body.Error)
	}
}

// 缺少幂等键或用途：400。
func TestUploadBytesRequiresKeyAndPurpose(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir()})
	assertAPIError(t, uploadBytes(t, ts.URL, "intermediate", "", []byte("x")), http.StatusBadRequest, "invalid_request")
	assertAPIError(t, uploadBytes(t, ts.URL, "", "k-bytes-6", []byte("x")), http.StatusBadRequest, "invalid_request")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// 像素上限：超过的素材在入库前被拒（防解码炸弹），不占存储。
func TestIngestRejectsSourceOverPixelCeiling(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "big.mp4"))
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), MaxSourcePixels: 1000})
	resp := postWithKey(t, ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("big.mp4"), "purpose": "intermediate"}, testToken, "k-pixels")
	assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
}
