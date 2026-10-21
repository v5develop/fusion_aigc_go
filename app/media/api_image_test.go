package media

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// makeImage 生成一张纯色图片（ffmpeg 按扩展名选格式）。
func makeImage(t *testing.T, path string, w, h int) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i",
		"color=c=red:s="+strconv.Itoa(w)+"x"+strconv.Itoa(h), "-frames:v", "1", path).CombinedOutput(); err != nil {
		t.Fatalf("生成测试图片失败: %v: %s", err, out)
	}
}

// 长边超过上限：等比缩小到上限，另一边按比例。
func TestImageConformShrinksLongSide(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeImage(t, filepath.Join(f.dir, "a.png"), 800, 400)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/conform", map[string]any{
		"source": f.url("a.png"), "purpose": "intermediate", "rule": map[string]any{"max_long_side": 400},
	}, testToken, "k-img-shrink")
	res := decodeMediaResult(t, resp)
	if res.Object.Width != 400 || res.Object.Height != 200 {
		t.Fatalf("shrink = %dx%d, want 400x200", res.Object.Width, res.Object.Height)
	}
}

// 长边不足下限：等比放大到下限。
func TestImageConformEnlargesToMinLongSide(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeImage(t, filepath.Join(f.dir, "a.png"), 800, 400)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/conform", map[string]any{
		"source": f.url("a.png"), "purpose": "intermediate", "rule": map[string]any{"min_long_side": 1000},
	}, testToken, "k-img-enlarge")
	res := decodeMediaResult(t, resp)
	if res.Object.Width != 1000 || res.Object.Height != 500 {
		t.Fatalf("enlarge = %dx%d, want 1000x500", res.Object.Width, res.Object.Height)
	}
}

// 不需要处理（区间内、格式允许）：原样落桶，内容 sha256 与源文件一致（不重新编码）。
func TestImageConformWithoutWorkKeepsSourceBytes(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	src := filepath.Join(f.dir, "a.png")
	makeImage(t, src, 800, 400)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/conform", map[string]any{
		"source": f.url("a.png"), "purpose": "intermediate", "rule": map[string]any{"min_long_side": 100, "max_long_side": 2000},
	}, testToken, "k-img-keep")
	res := decodeMediaResult(t, resp)
	if res.Object.Sha256 != fileSHA(t, src) {
		t.Fatal("不需要处理时必须保留源内容")
	}
}

// 限定格式：源是 PNG、只允许 JPEG 时转码为 JPEG。
func TestImageConformTranscodesToAllowedMime(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeImage(t, filepath.Join(f.dir, "a.png"), 320, 240)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/conform", map[string]any{
		"source": f.url("a.png"), "purpose": "intermediate", "rule": map[string]any{"mime_types": []string{"image/jpeg"}},
	}, testToken, "k-img-mime")
	res := decodeMediaResult(t, resp)
	if res.Object.Mime != "image/jpeg" {
		t.Fatalf("mime = %s, want image/jpeg", res.Object.Mime)
	}
}

// 长边区间写反：请求无效。
func TestImageConformRejectsInvertedRange(t *testing.T) {
	f := newAPIFixture(t)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/conform", map[string]any{
		"source": f.url("a.png"), "purpose": "intermediate", "rule": map[string]any{"min_long_side": 900, "max_long_side": 100},
	}, testToken, "k-img-range")
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
