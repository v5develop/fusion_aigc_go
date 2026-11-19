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
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	seekRatio      = 0.15 // seek 位点占片长的比例
	seekMaxSeconds = 3.0  // seek 位点上限（秒）：主体通常在前段，seek 越远解码越慢
	extractTimeout = 120 * time.Second
	stderrTailSize = 300
)

// Extractor 调用本机的 ffmpeg / ffprobe（需已安装，路径可配；留空用 PATH 里的同名命令）。
// 输入可以是本地路径，也可以是 http(s) 公网地址：ffmpeg 直接读远端，不用先下载整段视频。
type Extractor struct {
	FFmpeg  string
	FFprobe string
}

// Extract 抽一帧 JPEG 字节。探不到片长（ffprobe 缺失、输出非数字等）或 seek 那次失败，退回开头再抽一次。
func (e Extractor) Extract(ctx context.Context, videoURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	seek := seekFor(e.duration(ctx, videoURL))
	data, err := e.frame(ctx, videoURL, seek)
	if err == nil || seek == 0 {
		return data, err
	}
	return e.frame(ctx, videoURL, 0)
}

// inputProtocols 输入只许 http(s)（及其底层 tcp/tls/crypto）与本地文件：封面源多为网络地址，
// 但不允许清单文件再跳到 file:// 等本地协议读文件；必须出现在 -i 之前。
const inputProtocols = "file,http,https,tcp,tls,crypto"

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
