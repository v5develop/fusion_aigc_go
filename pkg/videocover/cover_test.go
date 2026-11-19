package videocover

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"aigc-local-life/pkg/safehttp"
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

func TestExtractRejectsURLAndMissing(t *testing.T) {
	for _, in := range []string{"http://127.0.0.1/a.mp4", "concat:/etc/passwd|/x", "", "https://example.com/a.mp4"} {
		if _, err := (Extractor{}).Extract(context.Background(), in); err == nil {
			t.Fatalf("Extract(%q) 应拒绝非本地文件", in)
		}
	}
	if _, err := (Extractor{}).Extract(context.Background(), t.TempDir()); !errors.Is(err, ErrNotLocalFile) {
		t.Fatalf("目录应报 ErrNotLocalFile，得 %v", err)
	}
}

func TestExtractURLRejectsBadScheme(t *testing.T) {
	for _, in := range []string{"file:///etc/passwd", "ftp://x/a.mp4", "/tmp/a.mp4", "gopher://x"} {
		if _, err := (Extractor{}).ExtractURL(context.Background(), in); err == nil {
			t.Fatalf("ExtractURL(%q) 应拒绝", in)
		}
	}
}

// 默认客户端（线上规则）拦回环地址：测试服务器在 127.0.0.1，必须被拒。
func TestExtractURLBlocksPrivateByDefault(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer ts.Close()
	if _, err := (Extractor{}).ExtractURL(context.Background(), ts.URL+"/a.mp4"); err == nil {
		t.Fatal("回环地址应被 safehttp 拦截")
	}
}

func TestExtractURLSizeCapAndSuccess(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	src := filepath.Join(t.TempDir(), "src.mp4")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=160x120:rate=10",
		"-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v: %s", err, out)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(src))))
	defer ts.Close()
	hc := safehttp.NewClient(30*time.Second, true) // 仅测试放行回环
	data, err := (Extractor{HTTP: hc}).ExtractURL(context.Background(), ts.URL+"/src.mp4")
	if err != nil || !bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("远端抽帧失败: %v", err)
	}
	if _, err := (Extractor{HTTP: hc, MaxRemoteBytes: 100}).ExtractURL(context.Background(), ts.URL+"/src.mp4"); !errors.Is(err, ErrRemoteTooLarge) {
		t.Fatalf("超限应报 ErrRemoteTooLarge，得 %v", err)
	}
}

// ffmpeg 的 http 协议被白名单挡掉：本地文件是指向内网地址的 HLS 清单也读不出来。
func TestExtractDoesNotFollowNetworkFromManifest(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("未安装 ffmpeg，跳过")
	}
	hit := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
	}))
	defer ts.Close()
	m3u8 := filepath.Join(t.TempDir(), "x.m3u8")
	_ = os.WriteFile(m3u8, []byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n"+ts.URL+"/seg.ts\n#EXT-X-ENDLIST\n"), 0o600)
	_, _ = (Extractor{}).Extract(context.Background(), m3u8)
	select {
	case <-hit:
		t.Fatal("ffmpeg 不应向清单里的网络地址发请求")
	default:
	}
}
