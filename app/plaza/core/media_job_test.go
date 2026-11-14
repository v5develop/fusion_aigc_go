package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/model"
)

func mjRow(status, mediaType, video, poster string) *model.Template {
	t := &model.Template{MediaStatus: status, MediaType: mediaType}
	if video != "" {
		t.VideoURL.String, t.VideoURL.Valid = video, true
	}
	if poster != "" {
		t.PosterURL.String, t.PosterURL.Valid = poster, true
	}
	return t
}

func TestMediaChangeSet(t *testing.T) {
	status := func(m map[string]any) string {
		if m == nil {
			return ""
		}
		return m["media_status"].(string)
	}
	cases := []struct {
		name                 string
		cur                  *model.Template
		mediaType, video, pt string
		want                 string
	}{
		{"没动：READY 保持", mjRow("READY", "video", "v1", "p"), "video", "v1", "p", ""},
		{"没动：PROCESSING 保持（不打断在途）", mjRow("PROCESSING", "video", "v1", ""), "video", "v1", "", ""},
		{"换视频：READY → PROCESSING", mjRow("READY", "video", "v1", "p"), "video", "v2", "p", "PROCESSING"},
		{"换视频：FAILED → PROCESSING（旧失败作废）", mjRow("FAILED", "video", "v1", ""), "video", "v2", "", "PROCESSING"},
		{"图片改视频", mjRow("READY", "image", "", ""), "video", "v1", "", "PROCESSING"},
		{"FAILED 且补了封面：重新排队", mjRow("FAILED", "video", "v1", ""), "video", "v1", "p", "PROCESSING"},
		{"FAILED 没补封面：不动", mjRow("FAILED", "video", "v1", ""), "video", "v1", "", ""},
		{"READY 只换封面：不重处理", mjRow("READY", "video", "v1", "p1"), "video", "v1", "p2", ""},
		{"视频改图片：FAILED 复位 READY", mjRow("FAILED", "video", "v1", ""), "image", "", "", "READY"},
		{"视频改图片：READY 不动", mjRow("READY", "video", "v1", ""), "image", "", "", ""},
		{"视频清空地址：PROCESSING 复位 READY", mjRow("PROCESSING", "video", "v1", ""), "video", " ", "", "READY"},
	}
	for _, c := range cases {
		got := mediaChangeSet(c.cur, c.mediaType, c.video, c.pt)
		if status(got) != c.want {
			t.Errorf("%s: got %q want %q", c.name, status(got), c.want)
		}
		if got != nil && (got["media_attempts"] != 0 || got["media_lease_token"] != nil || got["media_error"] != nil) {
			t.Errorf("%s: 应清空流水线状态: %v", c.name, got)
		}
	}
}

func TestMediaBackoffAndClip(t *testing.T) {
	if mediaBackoffAfter(1) != time.Minute || mediaBackoffAfter(2) != 5*time.Minute || mediaBackoffAfter(9) != 5*time.Minute || mediaBackoffAfter(0) != time.Minute {
		t.Fatal("退避口径不对")
	}
	if got := clipMediaErr(strings.Repeat("错", 400)); len([]rune(got)) != mediaErrMax {
		t.Fatalf("应截到 %d 个字符: %d", mediaErrMax, len([]rune(got)))
	}
}

type fakeCoverFn func(ctx context.Context, url string) (string, error)

func (f fakeCoverFn) Cover(ctx context.Context, u string) (string, error) { return f(ctx, u) }

type fakeTranscoder struct {
	url   string
	err   error
	calls int
}

func (f *fakeTranscoder) Transcode(context.Context, string) (string, error) {
	f.calls++
	return f.url, f.err
}

func TestProcessVideo(t *testing.T) {
	ctx := context.Background()
	v := objURL("v.mp4")
	newU := func() *Uploader {
		u := newTestUploader(&fakeStore{})
		u.cover = fakeCover{url: objURL("c.jpg")}
		return u
	}
	// 没有封面能力且无封面 → 永久失败
	u := newU()
	u.cover = fakeCover{err: ErrCoverUnavailable}
	if _, err := u.processVideo(ctx, MediaJobInput{VideoURL: v}); err == nil || !isPermanent(err) {
		t.Fatalf("没有抽帧能力应永久失败: %v", err)
	}
	// 抽帧临时失败 → 可重试
	u.cover = fakeCover{err: errors.New("503")}
	if _, err := u.processVideo(ctx, MediaJobInput{VideoURL: v}); err == nil || isPermanent(err) {
		t.Fatalf("抽帧临时失败应可重试: %v", err)
	}
	// 已有封面：不抽帧、不覆盖；没有转码能力 → 备注「未转码」
	u = newU()
	u.cover = fakeCoverFn(func(context.Context, string) (string, error) { t.Fatal("已有封面不应再抽帧"); return "", nil })
	r, err := u.processVideo(ctx, MediaJobInput{VideoURL: v, PosterURL: objURL("p.jpg")})
	if err != nil || r.PosterURL != "" || r.VideoURL != "" || !strings.Contains(r.Note, "未转码") {
		t.Fatalf("%+v %v", r, err)
	}
	// 抽封面 + 已经是 H.264 MP4 → 不转码
	u = newU()
	tc := &fakeTranscoder{url: objURL("t.mp4")}
	u.transcode = tc
	u.codec = func(context.Context, string) (*VideoCodec, error) {
		return &VideoCodec{Codec: "h264", PixFmt: "yuv420p"}, nil
	}
	r, err = u.processVideo(ctx, MediaJobInput{VideoURL: v})
	if err != nil || r.PosterURL != objURL("c.jpg") || r.VideoURL != "" || tc.calls != 0 || !strings.Contains(r.Note, "无需转码") {
		t.Fatalf("%+v %v calls=%d", r, err, tc.calls)
	}
	// HEVC → 转码；webm 容器的 h264 也转（容器不是 mp4）
	u.codec = func(context.Context, string) (*VideoCodec, error) {
		return &VideoCodec{Codec: "hevc", PixFmt: "yuv420p10le"}, nil
	}
	r, err = u.processVideo(ctx, MediaJobInput{VideoURL: v, PosterURL: "p"})
	if err != nil || r.VideoURL != objURL("t.mp4") || tc.calls != 1 || !strings.Contains(r.Note, "hevc") {
		t.Fatalf("%+v %v", r, err)
	}
	u.codec = func(context.Context, string) (*VideoCodec, error) {
		return &VideoCodec{Codec: "h264", PixFmt: "yuv420p"}, nil
	}
	if r, err = u.processVideo(ctx, MediaJobInput{VideoURL: objURL("w.webm"), PosterURL: "p"}); err != nil || r.VideoURL == "" {
		t.Fatalf("webm 应转码: %+v %v", r, err)
	}
	// 转码失败 → 可重试错误；外部地址 → 不转码；ffprobe 不可用 → 不转码
	tc.err = errors.New("ffmpeg boom")
	if _, err = u.processVideo(ctx, MediaJobInput{VideoURL: objURL("w.webm"), PosterURL: "p"}); err == nil || isPermanent(err) {
		t.Fatalf("转码失败应可重试: %v", err)
	}
	if r, err = u.processVideo(ctx, MediaJobInput{VideoURL: "https://other.example.com/v.mp4", PosterURL: "p"}); err != nil || !strings.Contains(r.Note, "外部地址") {
		t.Fatalf("%+v %v", r, err)
	}
	u.codec = func(context.Context, string) (*VideoCodec, error) { return nil, errProbeUnavailable }
	if r, err = u.processVideo(ctx, MediaJobInput{VideoURL: v, PosterURL: "p"}); err != nil || !strings.Contains(r.Note, "ffprobe") {
		t.Fatalf("%+v %v", r, err)
	}
	u.codec = func(context.Context, string) (*VideoCodec, error) { return nil, errors.New("损坏") }
	if _, err = u.processVideo(ctx, MediaJobInput{VideoURL: v, PosterURL: "p"}); err == nil {
		t.Fatal("读编码失败应报错重试")
	}
	// 没有 Uploader（没配存储）：有封面放行并记备注，没封面永久失败
	s := &Service{}
	if r, err = s.processMedia(ctx, MediaJobInput{VideoURL: v, PosterURL: "p"}); err != nil || r.Note == "" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = s.processMedia(ctx, MediaJobInput{VideoURL: v}); err == nil || !isPermanent(err) {
		t.Fatalf("无能力且无封面应永久失败: %v", err)
	}
}

func TestFFmpegTranscoderWithFakes(t *testing.T) {
	f := &fakeStore{}
	f.prefix = "aigc"
	var args []string
	tc := newFFmpegTranscoder("ffmpeg", f, func(ext string) string { return "aigc/" + UploadDir + "/202611/" + hex32 + "." + ext }, 1<<20)
	tc.download = func(_ context.Context, _ string, dst string, _ int64) error {
		return os.WriteFile(dst, []byte("in"), 0o600)
	}
	tc.run = func(_ context.Context, _ string, a ...string) ([]byte, error) {
		args = a
		return nil, os.WriteFile(a[len(a)-1], []byte("OUTPUT"), 0o600)
	}
	u, err := tc.Transcode(context.Background(), "https://x/v.webm")
	if err != nil || !strings.HasSuffix(u, ".mp4") || len(f.puts) != 1 {
		t.Fatalf("%q %v puts=%v", u, err, f.puts)
	}
	if !hasArg(args, "libx264") || !hasArg(args, "yuv420p") || !hasArg(args, "+faststart") {
		t.Fatalf("参数应含 H.264 / yuv420p / faststart: %v", args)
	}
	// ffmpeg 失败：错误里带最后一行
	tc.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("a\nInvalid data found"), errors.New("exit 1")
	}
	if _, err = tc.Transcode(context.Background(), "https://x/v.webm"); err == nil || !strings.Contains(err.Error(), "Invalid data found") {
		t.Fatalf("%v", err)
	}
	// 产物超上限：永久失败
	tc.maxBytes = 3
	tc.run = func(_ context.Context, _ string, a ...string) ([]byte, error) {
		return nil, os.WriteFile(a[len(a)-1], []byte("OUTPUT"), 0o600)
	}
	if _, err = tc.Transcode(context.Background(), "https://x/v.webm"); err == nil || !isPermanent(err) {
		t.Fatalf("产物超限应永久失败: %v", err)
	}
}

func hasArg(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// 真实 ffmpeg / ffprobe：生成一个 1 秒 mpeg4（非 H.264）MP4，探测 codec，再转码成 H.264 + yuv420p。没装则跳过。
func TestFFmpegRealTranscode(t *testing.T) {
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("没有 ffmpeg / ffprobe")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=1",
		"-c:v", "mpeg4", "-y", src).CombinedOutput(); err != nil {
		t.Skipf("生成测试视频失败: %v %s", err, out)
	}
	c, err := ffprobeCodec(context.Background(), fp, src)
	if err != nil || c.Codec != "mpeg4" || c.isPlainH264() {
		t.Fatalf("%+v %v", c, err)
	}
	f := &fakeStore{}
	f.prefix = "aigc"
	tc := newFFmpegTranscoder(ff, f, func(ext string) string { return "aigc/" + UploadDir + "/202611/" + hex32 + "." + ext }, 50<<20)
	tc.download = func(_ context.Context, _ string, dst string, _ int64) error {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	}
	if _, err := tc.Transcode(context.Background(), "https://x/src.mp4"); err != nil || len(f.puts) != 1 {
		t.Fatalf("%v puts=%v", err, f.puts)
	}
}

func TestAuditMatch(t *testing.T) {
	c, a := auditMatch("al.action", " TEMPLATE_UPDATE ")
	if c != "al.action = ?" || a[0] != "TEMPLATE_UPDATE" {
		t.Fatalf("%s %v", c, a)
	}
	c, a = auditMatch("al.action", "TEMPLATE_*")
	if c != "al.action LIKE ?" || a[0] != `TEMPLATE\_%` {
		t.Fatalf("前缀应转义下划线: %s %v", c, a)
	}
	if _, a = auditMatch("x", "*MEDIA*"); a[0] != "%MEDIA%" {
		t.Fatalf("%v", a)
	}
	if _, a = auditMatch("x", "50%*"); a[0] != `50\%%` {
		t.Fatalf("%% 应被转义: %v", a)
	}
}
