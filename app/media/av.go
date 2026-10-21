package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ProbeResult 容器与首条音视频流的元数据。无对应流时对应字段为 0 / false。
type ProbeResult struct {
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	DurationSeconds float64 `json:"duration_seconds"`
	FPS             float64 `json:"fps"`
	HasVideo        bool    `json:"has_video"`
	HasAudio        bool    `json:"has_audio"`
	SampleRate      int     `json:"sample_rate"`
	Channels        int     `json:"channels"`
}

// Frame 一张抽帧：时间点（秒）与 JPEG 字节。
type Frame struct {
	Timestamp float64
	JPEG      []byte
}

// Probe 用 ffprobe 读元数据（JSON 输出，与原 Java 的 MediaToolkit.probe 字段一致）。
func (s *Service) Probe(ctx context.Context, file string) (ProbeResult, error) {
	out, err := s.output(ctx, binOr(s.cfg.FFprobe, "ffprobe"), "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", file)
	if err != nil {
		return ProbeResult{}, classifyFFmpeg(ctx, err, "读取媒体信息失败")
	}
	return parseProbe(out)
}

// probeStream ffprobe 输出的单条流（只取用到的字段）。
type probeStream struct {
	CodecType    string         `json:"codec_type"`
	Width        int            `json:"width"`
	Height       int            `json:"height"`
	AvgFrameRate string         `json:"avg_frame_rate"`
	SampleRate   string         `json:"sample_rate"`
	Channels     int            `json:"channels"`
	Tags         map[string]any `json:"tags"`
	SideData     []struct {
		Type     string   `json:"side_data_type"`
		Rotation *float64 `json:"rotation"`
	} `json:"side_data_list"`
}

// displayRotated 流是否因旋转（手机竖拍常见的 90 / 270 度）而需要交换宽高。
// 优先取 Display Matrix 侧数据；没有时回退到旧版 rotate 标签。0 / 180 度不交换。
func displayRotated(st probeStream) bool {
	for _, sd := range st.SideData {
		if sd.Rotation != nil {
			return quarterTurn(int(math.Round(*sd.Rotation)))
		}
	}
	if v, ok := st.Tags["rotate"]; ok {
		if s, ok := v.(string); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				return quarterTurn(n)
			}
		}
	}
	return false
}

// quarterTurn 旋转角度（可为负）是否是 90 度的奇数倍。
func quarterTurn(deg int) bool {
	r := ((deg % 360) + 360) % 360
	return r == 90 || r == 270
}

// parseProbe 解析 ffprobe 的 JSON。拿不到有效 JSON 视为输入问题。
// 宽高是播放时的显示尺寸：有 90 / 270 度旋转的视频，宽高与编码尺寸互换（与 ffmpeg 自动旋转后的帧一致）。
func parseProbe(raw []byte) (ProbeResult, error) {
	var doc struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []probeStream `json:"streams"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ProbeResult{}, badInput("媒体信息不可解析")
	}
	var res ProbeResult
	res.DurationSeconds, _ = strconv.ParseFloat(doc.Format.Duration, 64)
	for _, st := range doc.Streams {
		switch st.CodecType {
		case "video":
			if !res.HasVideo {
				res.HasVideo = true
				res.Width, res.Height = st.Width, st.Height
				if displayRotated(st) {
					res.Width, res.Height = st.Height, st.Width
				}
				res.FPS = parseFPS(st.AvgFrameRate)
			}
		case "audio":
			if !res.HasAudio {
				res.HasAudio = true
				res.SampleRate, _ = strconv.Atoi(st.SampleRate)
				res.Channels = st.Channels
			}
		}
	}
	return res, nil
}

// parseFPS 解析 "30000/1001" 之类的帧率；解析失败或分母为 0 返回 0。
func parseFPS(value string) float64 {
	text := strings.ToLower(strings.TrimSpace(value))
	if text == "" {
		return 0
	}
	if num, den, ok := strings.Cut(text, "/"); ok {
		d, err1 := strconv.ParseFloat(den, 64)
		n, err2 := strconv.ParseFloat(num, 64)
		if err1 != nil || err2 != nil || d == 0 {
			return 0
		}
		return n / d
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0
	}
	return v
}

// FrameTimestamps 与原 Java VideoFrameExtractor.computeTimestamps 一致：在片长内均匀取 n 个点，
// 保留一位小数去重；时长未知（<=0）退化为单帧 0。
func FrameTimestamps(durationSeconds float64, count int) []float64 {
	n := max(1, count)
	if durationSeconds <= 0 {
		return []float64{0}
	}
	seen := make(map[float64]bool)
	var out []float64
	for i := range n {
		t := math.Round(durationSeconds*(float64(i)+0.5)/float64(n)*10) / 10
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Frames 在片长内均匀抽 count 张 JPEG（审核用）。单帧失败跳过，与原 Java 行为一致。
// 全部失败时才报错，且只在确为解码错误（ffmpeg 非零退出）时判为输入问题；中断、进程被杀、
// 本机异常、空输出都是服务侧的暂时问题，不能把合法视频拒掉。
func (s *Service) Frames(ctx context.Context, file string, count int) ([]Frame, error) {
	probe, err := s.Probe(ctx, file)
	if err != nil {
		return nil, err
	}
	var frames []Frame
	var lastErr error
	for _, ts := range FrameTimestamps(probe.DurationSeconds, count) {
		jpeg, err := s.output(ctx, s.ffmpeg(), "-nostdin", "-y", "-ss", strconv.FormatFloat(ts, 'f', 1, 64),
			"-i", file, "-frames:v", "1", "-c:v", "mjpeg", "-q:v", "3", "-f", "mjpeg", "pipe:1")
		if err != nil {
			if e := interruptedFailure(ctx, err, "抽帧失败"); e != nil {
				return nil, e
			}
			lastErr = err
			continue
		}
		if len(jpeg) == 0 {
			continue
		}
		frames = append(frames, Frame{Timestamp: ts, JPEG: jpeg})
	}
	if len(frames) > 0 {
		return frames, nil
	}
	var exitErr *exec.ExitError
	switch {
	case lastErr == nil:
		return nil, unavailable("抽帧结果为空")
	case errors.As(lastErr, &exitErr) && !killedBySignal(lastErr):
		return nil, badInput("视频无法抽帧")
	default:
		return nil, internal(lastErr)
	}
}

// Validate 完整解码一遍（只看有没有报错）。解码失败是输入问题（422）。
func (s *Service) Validate(ctx context.Context, file string) error {
	if err := s.run(ctx, s.ffmpeg(), "-nostdin", "-v", "error", "-i", file,
		"-map", "0:v?", "-map", "0:a?", "-f", "null", "-"); err != nil {
		return classifyFFmpeg(ctx, err, "完整解码失败")
	}
	return nil
}

// ExtractAudio 抽音轨为 mp3（libmp3lame）。
func (s *Service) ExtractAudio(ctx context.Context, file, out string) error {
	return s.run(ctx, s.ffmpeg(), "-y", "-threads", s.threads(), "-i", file, "-vn", "-c:a", "libmp3lame", out)
}

// AudioTimeline 按开始时间把配音片段拼成整条音轨（aac，m4a）。sampleRate / channels 为 0 时不指定。
func (s *Service) AudioTimeline(ctx context.Context, files []string, starts []float64, total float64,
	sampleRate, channels int, out string) error {
	filter, err := TimelineFilter(starts, total)
	if err != nil {
		return badInput(err.Error())
	}
	args := []string{"-y", "-threads", s.threads()}
	for _, f := range files {
		args = append(args, "-i", f)
	}
	args = append(args, "-filter_complex", filter, "-map", "[aout]", "-c:a", "aac")
	if sampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(sampleRate))
	}
	if channels > 0 {
		args = append(args, "-ac", strconv.Itoa(channels))
	}
	args = append(args, out)
	return s.run(ctx, s.ffmpeg(), args...)
}

// TimelineFilter 与原 Java FfmpegCommands.audioTimelineFilter 一致：adelay 各段 → amix（不归一）→ apad → atrim 到总时长。
func TimelineFilter(starts []float64, total float64) (string, error) {
	if len(starts) == 0 {
		return "", errors.New("音频片段不能为空")
	}
	var parts []string
	var mixed strings.Builder
	for i, st := range starts {
		start := math.Max(st, 0)
		delayMs := max(int64(math.RoundToEven(start*1000)), 0)
		parts = append(parts, fmt.Sprintf("[%d:a]adelay=%d:all=1[a%d]", i, delayMs, i))
		fmt.Fprintf(&mixed, "[a%d]", i)
	}
	parts = append(parts, fmt.Sprintf("%samix=inputs=%d:normalize=0,apad,atrim=duration=%s,asetpts=N/SR/TB[aout]",
		mixed.String(), len(starts), ffmpegNumber(total)))
	return strings.Join(parts, ";"), nil
}

// AdjustDuration 视频变速到 factor 倍（setpts），丢掉原音轨。
func (s *Service) AdjustDuration(ctx context.Context, file string, factor float64, out string) error {
	return s.run(ctx, s.ffmpeg(), "-y", "-threads", s.threads(), "-i", file,
		"-filter:v", "setpts="+ffmpegNumber(factor)+"*PTS", "-an", "-c:v", "libx264", out)
}

// Trim 截到前 seconds 秒。重编码（libx264 + aac）以保证切点精确；比 seconds 短的输入原样截成更短的输出，
// 由调用方（Java 工作流）在调用前用 Probe 判断是否要拒收，这里不做拒收。
func (s *Service) Trim(ctx context.Context, file string, seconds float64, out string) error {
	return s.run(ctx, s.ffmpeg(), "-y", "-threads", s.threads(), "-i", file,
		"-t", ffmpegNumber(seconds), "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-movflags", "+faststart", out)
}

// ReplaceAudio 视频流拷贝，换成新音轨（aac），按短的一方截断。
func (s *Service) ReplaceAudio(ctx context.Context, video, audio, out string) error {
	return s.run(ctx, s.ffmpeg(), "-y", "-threads", s.threads(), "-i", video, "-i", audio,
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-c:a", "aac", "-shortest", out)
}

// BurnSubtitles 烧录字幕（固定样式，libx264 重编码，音频拷贝）。字幕文件在工作目录里以固定文件名引用，
// 避免路径里的特殊字符要转义（与原 Java 的 escapeSubtitlePath 同一效果）。
func (s *Service) BurnSubtitles(ctx context.Context, dir, video, subtitle, out string) error {
	subName := "subtitle.srt"
	if err := copyFile(subtitle, filepath.Join(dir, subName)); err != nil {
		return internal(err)
	}
	filter := fmt.Sprintf("subtitles=%s:force_style='FontName=%s,FontSize=%d,Alignment=2,MarginL=40,MarginR=40,MarginV=28,Outline=1,Shadow=0,WrapStyle=0'",
		subName, s.subtitleFontName(), s.subtitleFontSize())
	return s.runIn(ctx, dir, s.ffmpeg(), "-y", "-threads", s.threads(), "-i", video, "-vf", filter,
		"-c:v", "libx264", "-c:a", "copy", out)
}

func (s *Service) threads() string {
	return strconv.Itoa(max(1, s.cfg.FFmpegThreads))
}

func (s *Service) subtitleFontName() string {
	if strings.TrimSpace(s.cfg.SubtitleFontName) != "" {
		return s.cfg.SubtitleFontName
	}
	return "Noto Sans CJK SC"
}

func (s *Service) subtitleFontSize() int {
	if s.cfg.SubtitleFontSize > 0 {
		return s.cfg.SubtitleFontSize
	}
	return 12
}

// ffmpegNumber 与原 Java PyNumbers.ffmpegDuration 一致：固定六位小数，去掉末尾的 0 与小数点。
func ffmpegNumber(v float64) string {
	text := strconv.FormatFloat(v, 'f', 6, 64)
	text = strings.TrimRight(text, "0")
	return strings.TrimSuffix(text, ".")
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o600)
}

// output 在计算名额内执行命令并返回 stdout；stderr 只保留尾部用于错误说明。
func (s *Service) output(ctx context.Context, bin string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	err := s.compute(ctx, func() error {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, tailOf(stderr.String()))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// runIn 在计算名额内、指定工作目录执行命令。
func (s *Service) runIn(ctx context.Context, dir, bin string, args ...string) error {
	return s.compute(ctx, func() error {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, tailOf(stderr.String()))
		}
		return nil
	})
}
