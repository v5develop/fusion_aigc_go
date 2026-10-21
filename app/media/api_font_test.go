package media

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// fontFile 生成一个只有文件头有效的字体文件：头 + 填充。
func fontFile(magic string, size int) []byte {
	b := make([]byte, 0, size)
	b = append(b, magic...)
	for len(b) < size {
		b = append(b, byte(len(b)%251))
	}
	return b
}

// postRaw 直接发请求体字节（字节流上传）。
func postRaw(t *testing.T, url string, body []byte, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return send(t, req, testToken)
}

var fontCases = []struct {
	name, magic, mime string
}{
	{"truetype", "\x00\x01\x00\x00", "font/ttf"},
	{"apple-truetype", "true", "font/ttf"},
	{"opentype-cff", "OTTO", "font/otf"},
	{"collection", "ttcf", "font/collection"},
	{"woff", "wOFF", "font/woff"},
	{"woff2", "wOF2", "font/woff2"},
}

// 各类字体文件头：字节流上传落桶，mime 正确，内容 sha256 与源一致，宽高为 0（不做媒体探测）。
func TestFontUploadStoresByMagic(t *testing.T) {
	f := newAPIFixture(t)
	for _, c := range fontCases {
		t.Run(c.name, func(t *testing.T) {
			body := fontFile(c.magic, 4096)
			resp := postRaw(t, f.ts.URL+"/v1/mediasvc/ingest/upload?purpose=asset_library", body, "k-font-"+c.name)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var res MediaResult
			if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
				t.Fatal(err)
			}
			if res.Object.Mime != c.mime || res.Object.Width != 0 || res.Object.Size != int64(len(body)) {
				t.Fatalf("object = %+v, want mime %s size %d", res.Object, c.mime, len(body))
			}
			if res.Object.Store != storeAsset {
				t.Fatalf("store = %s, want %s", res.Object.Store, storeAsset)
			}
			if got := fileSHA(t, storedObjectPath(t, f.objects, res)); got != res.Object.Sha256 {
				t.Fatal("落桶内容与 sha256 不一致")
			}
		})
	}
}

// URL 源同样支持字体：ingest 按文件头分流，不走 ffmpeg。
func TestFontIngestFromURL(t *testing.T) {
	f := newAPIFixture(t)
	if err := os.WriteFile(filepath.Join(f.dir, "brand.woff2"), fontFile("wOF2", 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest",
		map[string]any{"source": f.url("brand.woff2"), "purpose": "third_party"}, testToken, "k-font-url"))
	if res.Object.Mime != "font/woff2" || res.Object.Store != storeThirdParty {
		t.Fatalf("object = %+v, want font/woff2 in third_party store", res.Object)
	}
}

// 非字体（也不是媒体）：422 source_rejected。
func TestFontRejectsNonFontAndNonMedia(t *testing.T) {
	f := newAPIFixture(t)
	resp := postRaw(t, f.ts.URL+"/v1/mediasvc/ingest/upload?purpose=user_input", []byte("plain text, not a font"), "k-nofont")
	assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")

	if err := os.WriteFile(filepath.Join(f.dir, "fake.ttf"), []byte("<html>not a font</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp = postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{"source": f.url("fake.ttf"), "purpose": "user_input"}, testToken, "k-nofont-url")
	assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
}

// 字体同样受体积上限约束：超过 MaxInputBytes 是 422 source_rejected。
func TestFontOverSizeLimitRejected(t *testing.T) {
	ts := newTestServer(t, Config{AllowPrivateHosts: true, MaxInputBytes: 1024})
	resp := postRaw(t, ts.URL+"/v1/mediasvc/ingest/upload?purpose=user_input", fontFile("OTTO", 4096), "k-font-big")
	b := assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
	if b.Error.Check != "source.size_exceeded" {
		t.Fatalf("check = %s", b.Error.Check)
	}
}

// 字体入口的 purpose 规则不变：非法 purpose 是 400。
func TestFontPurposeRulesUnchanged(t *testing.T) {
	f := newAPIFixture(t)
	resp := postRaw(t, f.ts.URL+"/v1/mediasvc/ingest/upload?purpose=bogus", fontFile("wOFF", 512), "k-font-purpose")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}
