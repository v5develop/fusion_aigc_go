package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"aigc-local-life/pkg/videocover"
)

// Output 处理结果：落在临时目录里的文件，用完调 Cleanup。
type Output struct {
	Path    string
	Cleanup func()
}

// Service 三种无状态操作：封面、水印、合成。所有输入都先经 fetcher 下载到本机，ffmpeg 只读本地文件，
// 不让 ffmpeg 自己去拉远端地址（那样就绕过了 SSRF 检查）。
type Service struct {
	cfg   Config
	fetch *fetcher
	cache *inputCache
	slots chan struct{}
}

func NewService(cfg Config) *Service {
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = 1
	}
	f := newFetcher(cfg)
	removeOrphanCaches(cfg.TempDir)
	svc := &Service{
		cfg:   cfg,
		fetch: f,
		cache: newInputCache(cfg.TempDir, time.Duration(cfg.InputCacheMinutes)*time.Minute, cfg.InputCacheMaxBytes, f),
		slots: make(chan struct{}, n),
	}
	go func() {
		// 后台清扫过期的缓存文件；正在使用的文件不会被删
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for now := range t.C {
			svc.cache.sweep(now)
		}
	}()
	return svc
}

// acquire 占一个并发名额；满了就等到 ctx 结束为止，等不到返回 unavailable（503）。
func (s *Service) acquire(ctx context.Context) (func(), error) {
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-ctx.Done():
		return nil, unavailable("处理任务过多，请稍后重试")
	}
}

// begin 占名额 + 建工作目录 + 套上整体超时。调用方必须执行返回的 done。
func (s *Service) begin(ctx context.Context) (context.Context, string, func(), error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	opCtx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.OpTimeoutSeconds)*time.Second)
	dir, err := os.MkdirTemp(s.cfg.TempDir, "media-")
	if err != nil {
		cancel()
		release()
		return nil, "", nil, internal(err)
	}
	// 任务借到的缓存文件挂在 ctx 上，任务结束时统一释放
	pn := &pins{}
	opCtx = context.WithValue(opCtx, pinKey{}, pn)
	done := func() {
		cancel()
		pn.releaseAll()
		_ = os.RemoveAll(dir)
		release()
	}
	return opCtx, dir, done, nil
}

// Source 把输入落到工作目录里，返回本地路径。输入可以是公网地址（服务端下载），也可以是请求体（调用方直接传文件）。
type Source func(ctx context.Context, dir string) (string, error)

// FromURL 公网地址输入，经 SSRF 检查后下载。
func (s *Service) FromURL(videoURL string) Source {
	return func(ctx context.Context, _ string) (string, error) {
		return s.fetchCached(ctx, videoURL)
	}
}

// FromBody 请求体输入（视频文件本体），同样受体积上限约束。
func (s *Service) FromBody(body io.Reader) Source {
	return func(_ context.Context, dir string) (string, error) {
		path := filepath.Join(dir, "src.mp4")
		out, err := os.Create(path)
		if err != nil {
			return "", internal(err)
		}
		n, copyErr := io.Copy(out, io.LimitReader(body, s.cfg.MaxInputBytes+1))
		closeErr := out.Close()
		if copyErr != nil {
			return "", badInput("读取视频内容失败")
		}
		if closeErr != nil {
			return "", internal(closeErr)
		}
		if n == 0 {
			return "", badInput("视频内容为空")
		}
		if n > s.cfg.MaxInputBytes {
			return "", badInput("视频体积超过上限")
		}
		return path, nil
	}
}

// Cover 抽一张代表帧（JPEG 字节）。
func (s *Service) Cover(ctx context.Context, in Source) ([]byte, error) {
	opCtx, dir, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	src, err := in(opCtx, dir)
	if err != nil {
		return nil, err
	}
	data, err := videocover.Extractor{FFmpeg: s.cfg.FFmpeg, FFprobe: s.cfg.FFprobe}.Extract(opCtx, src)
	if err != nil {
		return nil, classifyFFmpeg(opCtx, err, "封面抽帧失败")
	}
	return data, nil
}

// Watermark 右下角叠加 logo。
func (s *Service) Watermark(ctx context.Context, in Source) (*Output, error) {
	if !s.logoAvailable() {
		return nil, logoUnavailable()
	}
	opCtx, dir, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	src, err := in(opCtx, dir)
	if err != nil {
		done()
		return nil, err
	}
	out := filepath.Join(dir, "watermarked.mp4")
	if err := s.run(opCtx, s.ffmpeg(), "-y", "-i", src, "-i", s.cfg.LogoPath,
		"-filter_complex", "overlay=W-w-12:H-h-12", out); err != nil {
		done()
		return nil, classifyFFmpeg(opCtx, err, "水印叠加失败")
	}
	return &Output{Path: out, Cleanup: done}, nil
}

// Compose 按顺序拼接。先用 concat demuxer 零转码；失败再回退到 filter_complex 重新编码（与 Java 的 VideoComposeService 一致）。
func (s *Service) Compose(ctx context.Context, urls []string) (*Output, error) {
	if len(urls) == 0 || len(urls) > s.cfg.MaxComposeInputs {
		return nil, badInput(fmt.Sprintf("合成片段数须在 1 到 %d 之间", s.cfg.MaxComposeInputs))
	}
	opCtx, dir, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(urls))
	for _, u := range urls {
		f, err := s.fetchCached(opCtx, u)
		if err != nil {
			done()
			return nil, err
		}
		files = append(files, f)
	}
	out := filepath.Join(dir, "output.mp4")

	if err := s.concatDemuxer(opCtx, dir, files, out); err != nil {
		if opCtx.Err() != nil {
			done()
			return nil, classifyFFmpeg(opCtx, err, "合成失败")
		}
		if err := s.concatFilter(opCtx, files, out); err != nil {
			done()
			return nil, classifyFFmpeg(opCtx, err, "合成失败")
		}
	}
	return &Output{Path: out, Cleanup: done}, nil
}

func (s *Service) concatDemuxer(ctx context.Context, dir string, files []string, out string) error {
	var list strings.Builder
	for _, f := range files {
		// 单引号按 concat 列表的转义规则处理，路径里出现引号也不会断
		list.WriteString("file '" + strings.ReplaceAll(f, "'", "'\\''") + "'\n")
	}
	listFile := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listFile, []byte(list.String()), 0o600); err != nil {
		return internal(err)
	}
	return s.run(ctx, s.ffmpeg(), "-y", "-f", "concat", "-safe", "0", "-i", listFile, "-c", "copy", out)
}

func (s *Service) concatFilter(ctx context.Context, files []string, out string) error {
	withAudio := true
	for _, f := range files {
		if !s.hasAudio(ctx, f) {
			withAudio = false
			break
		}
	}
	args := []string{"-y"}
	var filter strings.Builder
	for i, f := range files {
		args = append(args, "-i", f)
		filter.WriteString(fmt.Sprintf("[%d:v]", i))
		if withAudio {
			filter.WriteString(fmt.Sprintf("[%d:a]", i))
		}
	}
	a := 0
	outMap := []string{"-map", "[outv]"}
	outLabels := "[outv]"
	if withAudio {
		a = 1
		outLabels = "[outv][outa]"
		outMap = append(outMap, "-map", "[outa]")
	}
	filter.WriteString(fmt.Sprintf("concat=n=%d:v=1:a=%d%s", len(files), a, outLabels))
	args = append(args, "-filter_complex", filter.String())
	args = append(args, outMap...)
	args = append(args, "-c:v", "libx264", "-preset", "veryfast")
	if withAudio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, "-pix_fmt", "yuv420p", out)
	return s.run(ctx, s.ffmpeg(), args...)
}

// hasAudio 用 ffprobe 看有没有音轨；探测失败按无音轨处理（宁可丢声音，也不让整段合成失败）。
func (s *Service) hasAudio(ctx context.Context, file string) bool {
	out, err := exec.CommandContext(ctx, binOr(s.cfg.FFprobe, "ffprobe"), "-v", "error",
		"-select_streams", "a", "-show_entries", "stream=index", "-of", "csv=p=0", file).Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

func (s *Service) run(ctx context.Context, bin string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = nil
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, tailOf(stderr.String()))
	}
	return nil
}

func (s *Service) ffmpeg() string { return binOr(s.cfg.FFmpeg, "ffmpeg") }

func (s *Service) logoAvailable() bool {
	if s.cfg.LogoPath == "" {
		return false
	}
	st, err := os.Stat(s.cfg.LogoPath)
	return err == nil && !st.IsDir()
}

// classifyFFmpeg 区分三种情况：超时（503）、本机缺 ffmpeg（500）、输入解不开（422）。
func classifyFFmpeg(ctx context.Context, err error, msg string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return unavailable(msg + "：处理超时")
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return internal(err)
	}
	return badInput(msg + "：视频无法处理")
}

func binOr(path, def string) string {
	if strings.TrimSpace(path) == "" {
		return def
	}
	return path
}

func tailOf(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return "…" + s[len(s)-300:]
	}
	return s
}
