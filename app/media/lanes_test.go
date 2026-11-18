package media

import (
	"context"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func TestLaneSizes(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		cpu  int
		want [laneCount]int
	}{
		{"全默认 8 核", Config{FFmpegThreads: 2}, 8, [laneCount]int{4, 16, 32}},
		{"全默认 32 核", Config{FFmpegThreads: 2}, 32, [laneCount]int{16, 64, 32}},
		{"全默认 1 核，视频不低于旧默认 4", Config{FFmpegThreads: 2}, 1, [laneCount]int{4, 2, 32}},
		{"FFmpegThreads 未配按 2", Config{}, 16, [laneCount]int{8, 32, 32}},
		{"MaxConcurrent 只作视频别名", Config{MaxConcurrent: 3, FFmpegThreads: 2}, 8, [laneCount]int{3, 16, 32}},
		{"VideoCompute 优先于 MaxConcurrent", Config{MaxConcurrent: 3, VideoCompute: 5}, 8, [laneCount]int{5, 16, 32}},
		{"全部显式", Config{VideoCompute: 1, LightCompute: 7, IO: 9}, 64, [laneCount]int{1, 7, 9}},
	}
	for _, c := range cases {
		if got := laneSizes(c.cfg, c.cpu); got != c.want {
			t.Errorf("%s: 得到 %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestQueueWaitConfigurable(t *testing.T) {
	cases := []struct {
		cfg  Config
		want time.Duration
	}{
		{Config{}, 120 * time.Second},
		{Config{QueueWaitSeconds: 30}, 30 * time.Second},
		{Config{QueueWaitSeconds: 300}, 300 * time.Second},
		{Config{QueueWaitSeconds: 300, OpTimeoutSeconds: 60}, 60 * time.Second},
		{Config{OpTimeoutSeconds: 1800}, 120 * time.Second},
	}
	for _, c := range cases {
		if got := (&Service{cfg: c.cfg}).queueWait(); got != c.want {
			t.Errorf("%+v: 得到 %v，期望 %v", c.cfg, got, c.want)
		}
	}
}

// 写一个假的 ffmpeg / ffprobe：ffprobe 输出最小 JSON，ffmpeg 直接成功。
func stubTools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	dir := t.TempDir()
	ffmpeg = filepath.Join(dir, "ffmpeg")
	ffprobe = filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\necho '{\"streams\":[],\"format\":{\"duration\":\"1\"}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func writeLanePNG(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return p
}

// 通道分类表：视频算力通道被占满时，每个操作要么照常完成（轻算力 / I/O），要么排队超时 503 transient（视频算力）。
func TestLaneClassification(t *testing.T) {
	ffmpeg, ffprobe := stubTools(t)
	svc := NewService(Config{VideoCompute: 1, LightCompute: 4, IO: 4, OpTimeoutSeconds: 1,
		FFmpeg: ffmpeg, FFprobe: ffprobe, TempDir: t.TempDir()})
	release, err := svc.acquire(context.Background()) // 占满视频算力
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	img := writeLanePNG(t)
	txt := filepath.Join(t.TempDir(), "v.mp4")
	if err := os.WriteFile(txt, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cases := []struct {
		name      string
		run       func() error
		wantBlock bool
	}{
		{"Probe(ffprobe)", func() error { _, err := svc.Probe(ctx, img); return err }, false},
		{"hasAudio(ffprobe)", func() error { _, err := svc.hasAudio(ctx, txt); return err }, false},
		{"Validate 图片", func() error { return svc.Validate(ctx, img) }, false},
		{"runLight(图片 ffmpeg)", func() error { return svc.runLight(ctx, ffmpeg) }, false},
		{"computeLight(图片像素)", func() error { return svc.computeLight(ctx, func() error { return nil }) }, false},
		{"withIO(下载 / 读 / 写)", func() error { return svc.withIO(ctx, func() error { return nil }) }, false},
		{"Validate 视频", func() error { return svc.Validate(ctx, txt) }, true},
		{"run(视频 ffmpeg)", func() error { return svc.run(ctx, ffmpeg) }, true},
		{"output(抽帧)", func() error { _, err := svc.output(ctx, ffmpeg); return err }, true},
		{"runIn(字幕烧录)", func() error { return svc.runIn(ctx, t.TempDir(), ffmpeg) }, true},
		{"compute(视频检查)", func() error { return svc.compute(ctx, func() error { return nil }) }, true},
	}
	for _, c := range cases {
		err := c.run()
		if !c.wantBlock {
			if err != nil {
				t.Errorf("%s: 视频通道占满时不应受影响，得到 %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 应排在视频通道后面并超时", c.name)
			continue
		}
		ae := classifyError(err)
		if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable || !ae.Retryable {
			t.Errorf("%s: 应为 503 transient 可重试，得到 %+v status=%d", c.name, ae, ae.status)
		}
	}
}

// 轻算力通道被占满时同样排队超时 503 transient，且不影响视频算力。
func TestLightLaneTimeoutIs503AndIsolated(t *testing.T) {
	svc := NewService(Config{VideoCompute: 1, LightCompute: 1, OpTimeoutSeconds: 1, TempDir: t.TempDir()})
	rel, err := svc.takeSlot(context.Background(), laneLight)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	err = svc.computeLight(context.Background(), func() error { return nil })
	if err == nil {
		t.Fatal("轻算力占满应超时")
	}
	if ae := classifyError(err); ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable || !ae.Retryable {
		t.Fatalf("应为 503 transient，得到 %+v", ae)
	}
	if err := svc.compute(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("轻算力占满不应影响视频算力: %v", err)
	}
}

// 轻算力排队阶段名为 queue_light。
func TestLightLaneStageName(t *testing.T) {
	svc := NewService(Config{VideoCompute: 1, LightCompute: 1, OpTimeoutSeconds: 5, TempDir: t.TempDir()})
	ctx, timings := withStageTimings(context.Background())
	if err := svc.computeLight(ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := len(queueStages(timings.snapshot(), "queue_light")); n != 1 {
		t.Fatalf("应记录一条 queue_light，得到 %+v", timings.snapshot())
	}
}

func gaugeValue(t *testing.T, name, lane string) float64 {
	t.Helper()
	mfs, err := metricsRegistry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelOf(m, "lane") == lane {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func labelOf(m *dto.Metric, k string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == k {
			return l.GetValue()
		}
	}
	return ""
}

// 占用量 gauge 随名额占用 / 释放增减；等待时长直方图有记录。
func TestLaneMetrics(t *testing.T) {
	svc := NewService(Config{VideoCompute: 2, LightCompute: 2, OpTimeoutSeconds: 5, TempDir: t.TempDir()})
	base := gaugeValue(t, "media_lane_inuse", "light_compute")
	rel, err := svc.takeSlot(context.Background(), laneLight)
	if err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, "media_lane_inuse", "light_compute"); got != base+1 {
		t.Fatalf("占用后 inuse 应为 %v，得到 %v", base+1, got)
	}
	rel()
	if got := gaugeValue(t, "media_lane_inuse", "light_compute"); got != base {
		t.Fatalf("释放后 inuse 应回到 %v，得到 %v", base, got)
	}
	if got := gaugeValue(t, "media_lane_capacity", "light_compute"); got != 2 {
		t.Fatalf("capacity 应为 2，得到 %v", got)
	}
	mfs, _ := metricsRegistry().Gather()
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "media_lane_wait_seconds" && len(mf.GetMetric()) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("缺少 media_lane_wait_seconds")
	}
}

// 存储客户端的连接池：每主机空闲连接数远大于 Go 默认的 2。
func TestStorageTransportSettings(t *testing.T) {
	d := newStorageTransport(0)
	if d.MaxIdleConnsPerHost != defaultStorageIdleConnsPerHost || d.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Fatalf("默认每主机空闲连接应为 %d，得到 %d", defaultStorageIdleConnsPerHost, d.MaxIdleConnsPerHost)
	}
	if d.MaxIdleConns < d.MaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConns(%d) 不应小于每主机上限(%d)", d.MaxIdleConns, d.MaxIdleConnsPerHost)
	}
	if d.ResponseHeaderTimeout != 0 {
		t.Fatal("大对象 PUT 不能设 ResponseHeaderTimeout")
	}
	if got := newStorageTransport(200); got.MaxIdleConnsPerHost != 200 || got.MaxIdleConns < 200 {
		t.Fatalf("显式配置应生效，得到 %d / %d", got.MaxIdleConnsPerHost, got.MaxIdleConns)
	}
	o, err := NewOSSStore(OSSConfig{
		Private:        OSSBucket{Endpoint: "https://e", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		AssetLibrary:   OSSBucket{Endpoint: "https://e", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		ThirdParty:     OSSBucket{Endpoint: "https://e", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		PresignMinutes: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := o.hc.Transport.(*http.Transport)
	if !ok || tr.MaxIdleConnsPerHost != defaultStorageIdleConnsPerHost {
		t.Fatalf("OSSStore 应使用调大的 Transport，得到 %T", o.hc.Transport)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	env := map[string]string{"MEDIA_VIDEO_COMPUTE": "6", "MEDIA_LIGHT_COMPUTE": " 12 ", "MEDIA_IO_SLOTS": "", "MEDIA_OSS_MAX_IDLE_CONNS_PER_HOST": "128"}
	var c Config
	if err := ApplyEnvOverrides(&c, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if c.VideoCompute != 6 || c.LightCompute != 12 || c.IO != 0 || c.OSS.MaxIdleConnsPerHost != 128 || c.QueueWaitSeconds != 0 {
		t.Fatalf("覆盖结果不对: %+v", c)
	}
	for _, bad := range []string{"0", "-1", "x"} {
		err := ApplyEnvOverrides(&Config{}, func(k string) string {
			if k == "MEDIA_IO_SLOTS" {
				return bad
			}
			return ""
		})
		if err == nil {
			t.Errorf("MEDIA_IO_SLOTS=%q 应被拒绝", bad)
		}
	}
}

// 轻通道的 ffmpeg 命令行一律限制线程；视频通道不动。
func TestLightFFmpegArgsLimitThreads(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec.txt")
	stub := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$@\" >> "+rec+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := NewService(Config{VideoCompute: 1, LightCompute: 2, FFmpegThreads: 4, OpTimeoutSeconds: 5, FFmpeg: stub, TempDir: t.TempDir()})
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(svc.runLight(ctx, stub, "-y", "-i", "in.png", "-frames:v", "1", "out.png"))
	must(svc.runLight(ctx, stub, "-y", "-threads", "4", "-i", "in.png", "out.png")) // 已带 -threads 的改写
	must(svc.run(ctx, stub, "-y", "-threads", "4", "-i", "in.mp4", "out.mp4"))      // 视频不动
	raw, _ := os.ReadFile(rec)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("应有 3 次调用，得到 %q", lines)
	}
	if !strings.HasPrefix(lines[0], "-threads 1 ") || !strings.Contains(lines[0], " -threads 1 out.png") ||
		!strings.Contains(lines[0], "-filter_threads 1") || strings.Contains(lines[0], "-threads 4") {
		t.Errorf("轻通道命令行应带 -threads 1：%s", lines[0])
	}
	if strings.Contains(lines[1], "-threads 4") || !strings.Contains(lines[1], "-threads 1") {
		t.Errorf("已有 -threads 应被改写为 1：%s", lines[1])
	}
	if !strings.Contains(lines[2], "-threads 4") || strings.Contains(lines[2], "filter_threads") {
		t.Errorf("视频通道命令行不应被改动：%s", lines[2])
	}
	// LightThreads 可配
	svc2 := NewService(Config{LightThreads: 3, TempDir: t.TempDir()})
	if got := svc2.lightFFmpegArgs([]string{"-i", "a", "o"}); got[1] != "3" {
		t.Errorf("LightThreads=3 应生效：%v", got)
	}
}

// 图片类处理文件里不得再直接调用视频通道的 run / compute。
func TestImageFilesUseLightLane(t *testing.T) {
	files, _ := filepath.Glob("api_image_*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f)
		for _, bad := range []string{"svc.run(ctx", "s.run(ctx", "svc.compute(ctx", "s.compute(ctx"} {
			if strings.Contains(string(raw), bad) {
				t.Errorf("%s 含 %q：图片处理应走 runLight / computeLight", f, bad)
			}
		}
	}
}

func TestEffectiveCPUsFromCgroup(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	cases := []struct {
		name  string
		procs int
		fs    map[string]string
		want  int
	}{
		{"无 cgroup 文件", 8, nil, 8},
		{"v2 限 2 核", 8, map[string]string{"/sys/fs/cgroup/cpu.max": "200000 100000\n"}, 2},
		{"v2 限 1.5 核向上取整", 8, map[string]string{"/sys/fs/cgroup/cpu.max": "150000 100000\n"}, 2},
		{"v2 max 不限", 8, map[string]string{"/sys/fs/cgroup/cpu.max": "max 100000\n"}, 8},
		{"v2 配额大于 GOMAXPROCS", 4, map[string]string{"/sys/fs/cgroup/cpu.max": "800000 100000"}, 4},
		{"v2 小于 1 核至少 1", 8, map[string]string{"/sys/fs/cgroup/cpu.max": "50000 100000"}, 1},
		{"v1 限 3 核", 16, map[string]string{
			"/sys/fs/cgroup/cpu/cpu.cfs_quota_us": "300000\n", "/sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n"}, 3},
		{"v1 -1 不限", 16, map[string]string{
			"/sys/fs/cgroup/cpu/cpu.cfs_quota_us": "-1\n", "/sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n"}, 16},
		{"v2 优先于 v1", 16, map[string]string{"/sys/fs/cgroup/cpu.max": "400000 100000",
			"/sys/fs/cgroup/cpu/cpu.cfs_quota_us": "100000", "/sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000"}, 4},
	}
	for _, c := range cases {
		if got := effectiveCPUsFrom(c.procs, files(c.fs)); got != c.want {
			t.Errorf("%s: 得到 %d，期望 %d", c.name, got, c.want)
		}
	}
}
