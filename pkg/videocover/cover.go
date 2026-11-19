// Package videocover 从成片抽一张代表帧当封面，算法与 fusion-aigc（Java）的 MediaPostProcessService.extractCover 一致。
//
// 为什么不抽首帧：AI 生成的视频常以黑场或淡入开场，首帧大概率全黑，等于没有封面，而且不报错。
// 所以取片长 15% 处（上限 3 秒）做 seek，再用 thumbnail 滤镜挑代表帧：seek 跳过开场，thumbnail 避开纯色帧。
// 任何一步失败都只返回错误，由调用方决定降级（封面是展示优化，没有它视频照常交付）。
package videocover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/pkg/safehttp"
)

const (
	seekRatio      = 0.15 // seek 位点占片长的比例
	seekMaxSeconds = 3.0  // seek 位点上限（秒）：主体通常在前段，seek 越远解码越慢
	extractTimeout = 120 * time.Second
	stderrTailSize = 300
)

// Extractor 调用本机的 ffmpeg / ffprobe（需已安装，路径可配；留空用 PATH 里的同名命令）。
//
// Extract 只接受本地文件；远端地址用 ExtractURL：先经 safehttp（拦内网 / 限重定向 / 限体积）下载到临时文件，
// 再抽帧。ffmpeg 自己永远不碰网络，所以视频里的 HLS / concat 清单也跳不到内网或别的协议（SSRF 防护）。
type Extractor struct {
	FFmpeg  string
	FFprobe string
	// MaxRemoteBytes ExtractURL 下载体积上限（字节）；<=0 用 DefaultMaxRemoteBytes
	MaxRemoteBytes int64
	// HTTP ExtractURL 用的客户端；nil 用 safehttp.Shared()（线上规则）。仅测试注入放行回环的客户端
	HTTP *http.Client
}

// DefaultMaxRemoteBytes 远端视频下载体积上限默认值。
const DefaultMaxRemoteBytes int64 = 300 << 20

// ErrNotLocalFile 输入不是本地常规文件（比如 URL）；远端输入请用 ExtractURL。
var ErrNotLocalFile = errors.New("videocover: 输入必须是本地文件（远端地址请用 ExtractURL）")

// ErrRemoteTooLarge 远端视频超过体积上限。
var ErrRemoteTooLarge = errors.New("videocover: 远端视频超过体积上限")

// Extract 从本地视频文件抽一帧 JPEG 字节。探不到片长（ffprobe 缺失、输出非数字等）或 seek 那次失败，退回开头再抽一次。
func (e Extractor) Extract(ctx context.Context, path string) ([]byte, error) {
	abs, err := localFile(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	seek := seekFor(e.duration(ctx, abs))
	data, err := e.frame(ctx, abs, seek)
	if err == nil || seek == 0 {
		return data, err
	}
	return e.frame(ctx, abs, 0)
}

// ExtractURL 下载 http(s) 视频到临时文件（safehttp，体积封顶）后抽帧；临时文件用完即删。
func (e Extractor) ExtractURL(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("videocover: 只支持 http(s) 地址")
	}
	max := e.MaxRemoteBytes
	if max <= 0 {
		max = DefaultMaxRemoteBytes
	}
	hc := e.HTTP
	if hc == nil {
		hc = safehttp.Shared()
	}
	f, err := os.CreateTemp("", "videocover-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	dctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("videocover: 下载视频失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("videocover: 下载视频失败: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, ErrRemoteTooLarge
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("videocover: 下载视频失败: %w", err)
	}
	if n > max {
		return nil, ErrRemoteTooLarge
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return e.Extract(ctx, f.Name())
}

// localFile 校验是本地常规文件并返回绝对路径：绝对路径以 / 开头，不会被 ffmpeg 当成 scheme:xxx 之类的协议输入。
func localFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", ErrNotLocalFile
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("videocover: 读取视频文件: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", ErrNotLocalFile
	}
	return abs, nil
}

// inputProtocols ffmpeg / ffprobe 只许读本地文件与管道，不许任何网络协议；必须出现在 -i 之前。
const inputProtocols = "file,pipe"

// seekFor 片长 15% 处，上限 3 秒；片长未知（<=0）时不 seek。
func seekFor(duration float64) float64 {
	if duration <= 0 {
		return 0
	}
	return math.Min(duration*seekRatio, seekMaxSeconds)
}

// duration 探测片长（秒）；失败返回 0，由调用方退回不 seek 的抽帧。
func (e Extractor) duration(ctx context.Context, videoURL string) float64 {
	out, err := exec.CommandContext(ctx, binOr(e.FFprobe, "ffprobe"),
		"-protocol_whitelist", inputProtocols, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", videoURL).Output()
	if err != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return d
}

// frame 在指定位点抽一帧，结果直接写到 stdout，不落临时文件。
func (e Extractor) frame(ctx context.Context, videoURL string, seek float64) ([]byte, error) {
	args := []string{"-y"}
	if seek > 0 {
		// -ss 放在 -i 之前是关键帧快速 seek：封面不需要帧级精确，放后面会逐帧解码到该位点，长视频上慢一个数量级
		args = append(args, "-ss", strconv.FormatFloat(seek, 'f', 3, 64))
	}
	args = append(args, "-protocol_whitelist", inputProtocols, "-i", videoURL, "-vf", "thumbnail", "-frames:v", "1", "-q:v", "2",
		"-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1")

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binOr(e.FFmpeg, "ffmpeg"), args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg 抽帧失败(seek=%.3fs): %w: %s", seek, err, tail(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, errors.New("ffmpeg 没有输出任何帧")
	}
	return stdout.Bytes(), nil
}

func binOr(path, def string) string {
	if strings.TrimSpace(path) == "" {
		return def
	}
	return path
}

// tail 只留 stderr 的末尾：ffmpeg 的日志很长，报错信息在最后。
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > stderrTailSize {
		return "…" + s[len(s)-stderrTailSize:]
	}
	return s
}
