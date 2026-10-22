package media

import (
	"bytes"
	"encoding/json"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// makeColorImage 生成单色图片，color 为 ffmpeg 颜色表达式（可带 @alpha）；rgba 为真时输出 RGBA 像素格式。
func makeColorImage(t *testing.T, path, color string, w, h int, rgba bool) {
	t.Helper()
	// alpha 必须在 color 源之后用 format=rgba 生成；只在输出端指定 -pix_fmt 会丢掉透明度
	src := "color=c=" + color + ":s=" + strconv.Itoa(w) + "x" + strconv.Itoa(h)
	if rgba {
		src += ",format=rgba"
	}
	args := []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i", src, "-frames:v", "1", path}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("生成测试图片失败: %v: %s", err, out)
	}
}

// pngColorType 读 PNG 的 IHDR 颜色类型：2 为 RGB，6 为 RGBA。
func pngColorType(t *testing.T, path string) byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 26 || string(data[12:16]) != "IHDR" {
		t.Fatalf("不是 PNG: %q", data[:min(len(data), 16)])
	}
	return data[25]
}

// storedObjectPath 落桶后对象在本地对象目录中的路径。
func storedObjectPath(t *testing.T, objects string, res MediaResult) string {
	t.Helper()
	return filepath.Join(objects, res.Object.Store, filepath.FromSlash(storedPathOf(t, objects, res.Object.RID)))
}

func resizeBody(f *apiFixture, name string, w, h int, format string) map[string]any {
	return map[string]any{
		"source": f.url(name), "width": w, "height": h, "format": format, "purpose": "intermediate",
	}
}

// 精确尺寸、不保比例：800x400 缩成 300x300，输出就是 300x300。
func TestImageResizeExactSizeIgnoresAspectRatio(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeImage(t, filepath.Join(f.dir, "a.png"), 800, 400)
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize",
		resizeBody(f, "a.png", 300, 300, "png"), testToken, "k-resize-exact"))
	if res.Object.Width != 300 || res.Object.Height != 300 || res.Object.Mime != "image/png" {
		t.Fatalf("resize = %dx%d %s, want 300x300 image/png", res.Object.Width, res.Object.Height, res.Object.Mime)
	}
}

// 透明源（存在 alpha < 255 的像素）保留 alpha：输出 PNG 是 RGBA（颜色类型 6）。
func TestImageResizePNGKeepsAlphaWhenSourceTranslucent(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeColorImage(t, filepath.Join(f.dir, "t.png"), "red@0.5", 200, 200, true)
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize",
		resizeBody(f, "t.png", 100, 50, "png"), testToken, "k-resize-alpha"))
	if ct := pngColorType(t, storedObjectPath(t, f.objects, res)); ct != 6 {
		t.Fatalf("半透明源输出的颜色类型 = %d, want 6 (RGBA)", ct)
	}
	if res.Object.Width != 100 || res.Object.Height != 50 {
		t.Fatalf("size = %dx%d, want 100x50", res.Object.Width, res.Object.Height)
	}
}

// 不透明源（RGB 图）输出 RGB（颜色类型 2），不多带一个全 255 的通道。
func TestImageResizePNGWritesRGBWhenSourceOpaque(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeColorImage(t, filepath.Join(f.dir, "o.png"), "blue", 200, 200, false)
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize",
		resizeBody(f, "o.png", 120, 80, "png"), testToken, "k-resize-rgb"))
	if ct := pngColorType(t, storedObjectPath(t, f.objects, res)); ct != 2 {
		t.Fatalf("不透明 RGB 源输出的颜色类型 = %d, want 2 (RGB)", ct)
	}
}

// RGBA 源但所有像素 alpha 都是 255：同样写成 RGB，与 Java 现行规则一致。
func TestImageResizePNGWritesRGBWhenRGBAButFullyOpaque(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeColorImage(t, filepath.Join(f.dir, "fo.png"), "green", 200, 200, true)
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize",
		resizeBody(f, "fo.png", 64, 64, "png"), testToken, "k-resize-fullopaque"))
	if ct := pngColorType(t, storedObjectPath(t, f.objects, res)); ct != 2 {
		t.Fatalf("全不透明 RGBA 源输出的颜色类型 = %d, want 2 (RGB)", ct)
	}
}

// JPEG 白底：全透明的源缩放后中心像素接近白色，而不是黑色。
func TestImageResizeJPEGFlattensOnWhite(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeColorImage(t, filepath.Join(f.dir, "clear.png"), "red@0.0", 200, 200, true)
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize",
		resizeBody(f, "clear.png", 100, 100, "jpeg"), testToken, "k-resize-jpeg"))
	if res.Object.Mime != "image/jpeg" || res.Object.Width != 100 || res.Object.Height != 100 {
		t.Fatalf("object = %+v, want 100x100 image/jpeg", res.Object)
	}
	fh, err := os.Open(storedObjectPath(t, f.objects, res))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	img, err := jpeg.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(50, 50).RGBA()
	if r>>8 < 230 || g>>8 < 230 || b>>8 < 230 {
		t.Fatalf("透明区域应铺白底，中心像素 = (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

// 参数越界：宽高 0 或超过 16384、格式不在 png/jpeg、缺幂等键，都是 400。
func TestImageResizeRejectsBadArgs(t *testing.T) {
	f := newAPIFixture(t)
	cases := []struct {
		name string
		body map[string]any
		key  string
	}{
		{"width 0", resizeBody(f, "a.png", 0, 10, "png"), "k1"},
		{"height 0", resizeBody(f, "a.png", 10, 0, "png"), "k2"},
		{"width too large", resizeBody(f, "a.png", 16385, 10, "png"), "k3"},
		{"height too large", resizeBody(f, "a.png", 10, 16385, "png"), "k4"},
		{"format gif", resizeBody(f, "a.png", 10, 10, "gif"), "k5"},
		{"no key", resizeBody(f, "a.png", 10, 10, "png"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize", c.body, testToken, c.key)
			assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
		})
	}
}

// 超像素：目标 宽×高 超过 MaxSourcePixels，在下载前就是 422。
func TestImageResizeRejectsTargetPixelsOverLimit(t *testing.T) {
	dir := t.TempDir()
	origin := fileServer(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, MaxSourcePixels: 1_000_000})
	body := map[string]any{"source": map[string]string{"url": origin.URL + "/a.png"},
		"width": 2000, "height": 1000, "format": "png", "purpose": "intermediate"}
	resp := postWithKey(t, ts.URL, "/v1/mediasvc/image/resize", body, testToken, "k-pixels")
	b := assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
	if b.Error.Check != "image.target_pixels_exceeded" {
		t.Fatalf("check = %s", b.Error.Check)
	}
}

// 非图片（视频、文本）是 422，不能靠 ffmpeg 去猜。
func TestImageResizeRejectsNonImage(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeClip(t, filepath.Join(f.dir, "clip.mp4"), "1", false)
	if err := os.WriteFile(filepath.Join(f.dir, "note.txt"), []byte("not an image at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"clip.mp4", "note.txt"} {
		resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize", resizeBody(f, name, 10, 10, "png"), testToken, "k-nonimg-"+name)
		b := assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
		if b.Error.Check != "image.not_image" {
			t.Fatalf("%s: check = %s, want image.not_image", name, b.Error.Check)
		}
	}
}

// 文件头像 PNG 但内容坏了：解码失败映射为 source_rejected（422）。
func TestImageResizeCorruptPNGIsSourceRejected(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	corrupt := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00, 0x13, 0x37}, 64)...)
	if err := os.WriteFile(filepath.Join(f.dir, "bad.png"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/resize", resizeBody(f, "bad.png", 10, 10, "png"), testToken, "k-corrupt")
	assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
}

// imageSize 能力：probe 已经返回宽高，不需要新接口。
func TestProbeReturnsImageSize(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeImage(t, filepath.Join(f.dir, "a.png"), 320, 240)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/probe", map[string]any{"source": f.url("a.png")}, testToken, "")
	var m MediaMeta
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || m.Width != 320 || m.Height != 240 || m.Mime != "image/png" {
		t.Fatalf("probe = %d %+v, want 320x240 image/png", resp.StatusCode, m)
	}
}

// 能力声明里登记了 image_resize 与 font。
func TestCapabilitiesAdvertiseResizeAndFont(t *testing.T) {
	ts := newTestServer(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/mediasvc/capabilities", nil)
	var caps map[string]bool
	_ = json.NewDecoder(send(t, req, testToken).Body).Decode(&caps)
	if !caps["image_resize"] || !caps["font"] {
		t.Fatalf("caps = %v", caps)
	}
}
