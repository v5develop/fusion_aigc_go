package videocover

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSeekFor(t *testing.T) {
	cases := map[float64]float64{
		-1:  0,   // 片长未知：不 seek
		0:   0,   // 探测失败
		4:   0.6, // 短片：15%
		10:  1.5, // 15%
		30:  3,   // 长片封顶 3 秒
		600: 3,
	}
	for in, want := range cases {
		if got := seekFor(in); got != want {
			t.Errorf("seekFor(%v) = %v, want %v", in, got, want)
		}
	}
}

// 用 ffmpeg 生成一段真实的测试视频再抽帧，验证输出是合法 JPEG。没装 ffmpeg 就跳过。
func TestExtractFromRealVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	src := filepath.Join(t.TempDir(), "src.mp4")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=4:size=160x120:rate=10",
		"-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v: %s", err, out)
	}
	data, err := Extractor{}.Extract(context.Background(), src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("输出不是 JPEG：前 4 字节 % x", data[:min(4, len(data))])
	}
}

func TestExtractReportsMissingVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	if _, err := (Extractor{}).Extract(context.Background(), filepath.Join(t.TempDir(), "missing.mp4")); err == nil {
		t.Fatal("不存在的视频应当返回错误，由调用方降级")
	}
}
