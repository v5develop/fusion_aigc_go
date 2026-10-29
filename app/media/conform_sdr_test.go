package media

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
)

// 带色彩标记的测试片源：tags 是 ffmpeg 的 -colorspace / -color_trc / -color_primaries 取值，空串表示不写标记。
func makeTaggedClip(t *testing.T, path, tags string) {
	t.Helper()
	args := []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=2:size=640x360:rate=10",
		"-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=none"}
	switch tags {
	case "hdr":
		args = append(args, "-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:log-level=none",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc")
	case "sdr":
		args = append(args, "-pix_fmt", "yuv420p", "-x265-params", "colorprim=bt709:transfer=bt709:colormatrix=bt709:log-level=none",
			"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	}
	args = append(args, path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
}

// HDR 片源：SDR 规整做 tone mapping 转成 bt709 SDR（需要 ffmpeg 带 zscale；镜像内的 ffmpeg 自带）。
func TestConformSdrToneMapsHdrSource(t *testing.T) {
	requireFFmpeg(t)
	requireFFmpegFilter(t, "zscale")
	f := newAPIFixture(t)
	makeTaggedClip(t, filepath.Join(f.dir, "hdr.mp4"), "hdr")

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("hdr.mp4"), "rule": map[string]any{"sdr": true, "max_short_side": 360, "max_long_side": 640},
		"purpose": "intermediate"}, testToken, "k-hdr"))
	if res.Object.Width > 640 || res.Object.Height > 360 {
		t.Fatalf("size = %dx%d", res.Object.Width, res.Object.Height)
	}
}

// 色彩未知：SDR 规整拒收（不能证明是 SDR）。
func TestConformSdrRejectsUntaggedSource(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "plain.mp4"))

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("plain.mp4"), "rule": map[string]any{"sdr": true}, "purpose": "intermediate"}, testToken, "k-plain")
	body := assertAPIError(t, resp, http.StatusUnprocessableEntity, codeSourceRejected)
	if body.Error.Check != "media.conform_colour_unknown" {
		t.Fatalf("check = %s", body.Error.Check)
	}
}

// bt709 标记的 SDR 片源：规整通过，尺寸落在上限内。
func TestConformSdrAcceptsTaggedSdrAndHonoursCaps(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeTaggedClip(t, filepath.Join(f.dir, "sdr.mp4"), "sdr")

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("sdr.mp4"), "rule": map[string]any{"sdr": true, "max_short_side": 360, "max_long_side": 640},
		"purpose": "intermediate"}, testToken, "k-sdr"))
	if res.Object.Width > 640 || res.Object.Height > 360 || res.Object.Width < res.Object.Height {
		t.Fatalf("size = %dx%d", res.Object.Width, res.Object.Height)
	}
}
