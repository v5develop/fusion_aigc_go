package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	// slots 计算名额（ffmpeg / ffprobe / 图像运算）；只在叶子调用处占用，不跨越下载与上传。
	slots chan struct{}
	// ioSlots 输入的下载与字节读取名额。I/O 不占计算名额，但仍设上限，防止大量并发下载写满磁盘。
	ioSlots chan struct{}
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
		cache: newInputCache(cfg.TempDir, time.Duration(cfg.InputCacheMinutes)*time.Minute,
			effectiveCacheCap(cfg.InputCacheMaxBytes, cfg.TempDir), f),
		slots:   make(chan struct{}, n),
		ioSlots: make(chan struct{}, ioSlotFactor*n),
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

// queueWaitCap 名额排队等待的上限：超过就返回 503 transient，不把请求挂满整个处理时长。
const queueWaitCap = 120 * time.Second

// ioSlotFactor 输入名额数 = ioSlotFactor × MaxConcurrent。
const ioSlotFactor = 4

// queueWait 排队上限 = min(OpTimeoutSeconds, 120s)；OpTimeoutSeconds 未配置时取 120s。
func (s *Service) queueWait() time.Duration {
	if s.cfg.OpTimeoutSeconds > 0 {
		if d := time.Duration(s.cfg.OpTimeoutSeconds) * time.Second; d < queueWaitCap {
			return d
		}
	}
	return queueWaitCap
}

// takeSlot 占 ch 里的一个名额。满了就排队，但不超过 queueWait，也不超过 ctx 的截止时间。
// 等不到返回 unavailable（503 transient，带 Retry-After），调用方可用同一键重试。
func (s *Service) takeSlot(ctx context.Context, ch chan struct{}) (func(), error) {
	timer := time.NewTimer(s.queueWait())
	defer timer.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, unavailable("处理任务过多，请稍后重试")
	case <-timer.C:
		return nil, unavailable("处理任务过多，请稍后重试")
	}
}

// acquire 占一个计算名额。
func (s *Service) acquire(ctx context.Context) (func(), error) {
	return s.takeSlot(ctx, s.slots)
}

// compute 在计算名额内执行 fn。只用于叶子操作（单次 ffmpeg / ffprobe / 图像进程），叶子之间不嵌套，
// 否则同一个请求会占着名额再去等名额，并发为 1 时就死锁。
func (s *Service) compute(ctx context.Context, fn func() error) error {
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// withIO 在输入名额内执行 fn（下载、读取请求体、读取对象存储）。与计算名额互不占用。
func (s *Service) withIO(ctx context.Context, fn func() error) error {
	release, err := s.takeSlot(ctx, s.ioSlots)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// begin 建工作目录 + 套上整体超时（OpTimeoutSeconds）。不占名额：名额只在 compute / withIO 里按需占用。
// 调用方必须执行返回的 done。
func (s *Service) begin(ctx context.Context) (context.Context, string, func(), error) {
	opCtx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.OpTimeoutSeconds)*time.Second)
	dir, err := os.MkdirTemp(s.cfg.TempDir, "media-")
	if err != nil {
		cancel()
		return nil, "", nil, internal(err)
	}
	// 任务借到的缓存文件挂在 ctx 上，任务结束时统一释放
	pn := &pins{}
	opCtx = context.WithValue(opCtx, pinKey{}, pn)
	done := func() {
		cancel()
		pn.releaseAll()
		_ = os.RemoveAll(dir)
	}
	return opCtx, dir, done, nil
}

// Source 把输入落到工作目录里，返回本地路径。输入可以是公网地址（服务端下载），也可以是请求体（调用方直接传文件）。
type Source func(ctx context.Context, dir string) (string, error)

// FromURL 公网地址输入，经 SSRF 检查后下载。
func (s *Service) FromURL(videoURL string) Source {
	return s.FromURLWithHeaders(videoURL, nil)
}

// FromURLWithHeaders 公网地址输入，下载时带上白名单请求头（已校验）。
func (s *Service) FromURLWithHeaders(videoURL string, headers http.Header) Source {
	return func(ctx context.Context, dir string) (string, error) {
		var path string
		err := s.withIO(ctx, func() error {
			var err error
			if headers != nil {
				// 带调用方凭证的下载不进共享缓存：凭证不会被同一服务的其它调用方借用，也不会留在缓存里
				path, err = s.fetch.downloadWithHeaders(ctx, videoURL, headers, dir, randomName()+".bin")
			} else {
				path, err = s.fetchCached(ctx, videoURL, headers)
			}
			return err
		})
		return path, err
	}
}

// FromBody 请求体输入（视频文件本体），同样受体积上限约束。
func (s *Service) FromBody(body io.Reader) Source {
	return func(ctx context.Context, dir string) (string, error) {
		path := filepath.Join(dir, "src.mp4")
		err := s.withIO(ctx, func() error {
			out, err := os.Create(path)
			if err != nil {
				return internal(err)
			}
			n, copyErr := io.Copy(out, io.LimitReader(body, s.cfg.MaxInputBytes+1))
			closeErr := out.Close()
			if copyErr != nil {
				return badInput("读取视频内容失败")
			}
			if closeErr != nil {
				return internal(closeErr)
			}
			if n == 0 {
				return badInput("视频内容为空")
			}
			if n > s.cfg.MaxInputBytes {
				return badInput("视频体积超过上限")
			}
			return nil
		})
		if err != nil {
			return "", err
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
	srcs := make([]Source, 0, len(urls))
	for _, u := range urls {
		srcs = append(srcs, s.FromURL(u))
	}
	return s.ComposeSources(ctx, srcs)
}

// ComposeSources 同 Compose，输入可以是任意 Source（公网地址或本地对象）。
func (s *Service) ComposeSources(ctx context.Context, in []Source) (*Output, error) {
	if len(in) == 0 || len(in) > s.cfg.MaxComposeInputs {
		return nil, badInput(fmt.Sprintf("合成片段数须在 1 到 %d 之间", s.cfg.MaxComposeInputs))
	}
	opCtx, dir, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(in))
	for _, src := range in {
		f, err := src(opCtx, dir)
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
		has, err := s.hasAudio(ctx, f)
		if err != nil {
			return err
		}
		if !has {
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
func (s *Service) hasAudio(ctx context.Context, file string) (bool, error) {
	var has bool
	err := s.compute(ctx, func() error {
		out, err := exec.CommandContext(ctx, binOr(s.cfg.FFprobe, "ffprobe"), "-v", "error",
			"-select_streams", "a", "-show_entries", "stream=index", "-of", "csv=p=0", file).Output()
		has = err == nil && len(bytes.TrimSpace(out)) > 0
		return nil
	})
	return has, err
}

func (s *Service) run(ctx context.Context, bin string, args ...string) error {
	return s.compute(ctx, func() error {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdout = nil
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, tailOf(stderr.String()))
		}
		return nil
	})
}

func (s *Service) ffmpeg() string { return binOr(s.cfg.FFmpeg, "ffmpeg") }

func (s *Service) logoAvailable() bool {
	if s.cfg.LogoPath == "" {
		return false
	}
	st, err := os.Stat(s.cfg.LogoPath)
	return err == nil && !st.IsDir()
}

// classifyFFmpeg 区分几种情况：外部中断或被信号杀死（503，瞬时）、本机缺 ffmpeg（500）、输入解不开（422）。
// 已经是契约错误（如排队超时）的原样返回，不能再被改判成输入问题。
func classifyFFmpeg(ctx context.Context, err error, msg string) error {
	var oe *opError
	if errors.As(err, &oe) {
		return err
	}
	// 失败路径：原始 ffmpeg 错误（含 stderr 尾部）只进日志，并带上追踪号，便于按任务号找到
	log.Printf("media ffmpeg 失败 trace_id=%s step=%q: %v", TraceIDFromContext(ctx), msg, err)
	if e := interruptedFailure(ctx, err, msg); e != nil {
		return e
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return internal(err)
	}
	return badInput(msg + "：视频无法处理")
}

// interruptedFailure 判断 ffmpeg / ffprobe 的失败是否来自外部中断：上下文超时或取消（上游断开、关机），
// 或进程被信号杀死（如内核 OOM 的 SIGKILL）。这类失败与输入内容无关，返回瞬时错误（503，可用同一键重试）；否则返回 nil。
// 判据统一用 ctx.Err()，不依赖退出码的文字。
func interruptedFailure(ctx context.Context, err error, msg string) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return unavailable(msg + "：处理超时")
	case ctx.Err() != nil:
		return unavailable(msg + "：处理被取消")
	}
	if killedBySignal(err) {
		return unavailable(msg + "：进程被终止")
	}
	return nil
}

// killedBySignal 进程是否被信号杀死（如内核 OOM 的 SIGKILL）。
func killedBySignal(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ProcessState == nil {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
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
