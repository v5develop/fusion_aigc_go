package core

// 视频后处理的外部能力：codec 探测（ffprobe）与转码（本机 ffmpeg）。
//
// 媒体服务（app/media）目前没有「把任意视频统一转成 H.264 MP4」的接口：video/conform 只有在需要去音轨 / 变帧率 / 缩放 / SDR 重打标时才重编码，
// 否则原样落桶，不能当转码用。所以转码走本机 ffmpeg（显式配置 Plaza.Upload.FFmpeg 才启用）；没配置时流水线只抽封面并在 media_note 里记「未转码」。

import (
	"aigc-local-life/pkg/safehttp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"aigc-local-life/pkg/storage"
)

// VideoCodec 首个视频流的编码信息。
type VideoCodec struct {
	Codec  string // 如 h264 / hevc / vp9
	PixFmt string // 如 yuv420p
}

// isPlainH264 已经是 H.264 + yuv420p（MP4 容器由调用方另行判断）。
func (c VideoCodec) isPlainH264() bool {
	return strings.EqualFold(c.Codec, "h264") && strings.EqualFold(c.PixFmt, "yuv420p")
}

// Transcoder 把视频转成 H.264 MP4，产物落我们的公有桶，返回新地址。
type Transcoder interface {
	Transcode(ctx context.Context, videoURL string) (string, error)
}

// ffprobeCodec 读首个视频流的 codec / pix_fmt。没装 / 没配返回 errProbeUnavailable；文件无视频流 / 解析失败返回其它错误。
func ffprobeCodec(ctx context.Context, bin, rawURL string, maxBytes int64) (*VideoCodec, error) {
	if strings.TrimSpace(bin) == "" {
		bin = "ffprobe"
	}
	out, err := runProbe(ctx, bin, rawURL, maxBytes, "stream=codec_name,pix_fmt")
	if err != nil {
		return nil, err
	}
	c := &VideoCodec{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "codec_name":
			c.Codec = v
		case "pix_fmt":
			c.PixFmt = v
		}
	}
	if c.Codec == "" {
		return nil, errors.New("ffprobe: 没有视频流")
	}
	return c, nil
}

// ffmpegTranscoder 下载到临时文件 → ffmpeg 转码 → 传回公有桶。
type ffmpegTranscoder struct {
	ffmpeg   string
	store    storage.Store
	newKey   func(ext string) string
	maxBytes int64
	timeout  time.Duration

	// 可替换，单测注入。
	run      func(ctx context.Context, bin string, args ...string) ([]byte, error)
	download func(ctx context.Context, rawURL, dst string, max int64) error
}

func newFFmpegTranscoder(ffmpeg string, store storage.Store, newKey func(string) string, maxBytes int64) *ffmpegTranscoder {
	return &ffmpegTranscoder{ffmpeg: ffmpeg, store: store, newKey: newKey, maxBytes: maxBytes, timeout: 12 * time.Minute,
		run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, bin, args...).CombinedOutput()
		},
		download: downloadTo,
	}
}

func (t *ffmpegTranscoder) Transcode(ctx context.Context, videoURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "plaza-transcode-*")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out.mp4")
	if err := t.download(ctx, videoURL, in, t.maxBytes); err != nil {
		return "", fmt.Errorf("下载视频失败: %w", err)
	}
	if b, err := t.run(ctx, t.ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", in,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", out); err != nil {
		return "", fmt.Errorf("ffmpeg 转码失败: %w: %s", err, lastLine(string(b)))
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() <= 0 {
		return "", errors.New("ffmpeg 没有产出文件")
	}
	if st.Size() > t.maxBytes {
		return "", permanentErr(fmt.Errorf("转码后文件 %d MB 超过上限 %d MB", st.Size()>>20, t.maxBytes>>20))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return "", err
	}
	u, err := t.store.Put(ctx, t.newKey("mp4"), "video/mp4", data)
	if err != nil {
		return "", fmt.Errorf("转码产物上传失败: %w", err)
	}
	return u, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// downloadTo 流式下载到文件，至多 max 字节（超出视为失败，不留半截文件）。
func downloadTo(ctx context.Context, rawURL, dst string, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := safehttp.Shared().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return err
	}
	if n > max {
		return permanentErr(fmt.Errorf("视频超过 %d MB 上限", max>>20))
	}
	return nil
}

// runProbe 探测视频：远程地址先经 safehttp 下载到临时文件（体积封顶，用完即删），再对本地文件跑 ffprobe，
// 并用 -protocol_whitelist file,pipe 禁掉一切网络协议（防 m3u8 / concat 等清单把 ffprobe 引向内网）。
// 本地路径（非 http(s)）直接探测。没装 / 没配 ffprobe 返回 errProbeUnavailable。
func runProbe(ctx context.Context, bin, src string, maxBytes int64, entries string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if maxBytes <= 0 {
		maxBytes = 200 << 20
	}
	path := src
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		dir, err := os.MkdirTemp("", "plaza-probe-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "src.bin")
		if err := downloadTo(ctx, src, path, maxBytes); err != nil {
			return nil, fmt.Errorf("ffprobe: 下载失败: %w", err)
		}
	}
	out, err := exec.CommandContext(ctx, bin, "-v", "error", "-protocol_whitelist", "file,pipe", "-select_streams", "v:0",
		"-show_entries", entries, "-of", "default=noprint_wrappers=1", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("ffprobe: %w", err)
		}
		return nil, errProbeUnavailable
	}
	return out, nil
}
