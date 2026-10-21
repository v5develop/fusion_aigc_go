package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- M1：上游抓取的错误分类 ----

// downloadClass 用真实的 fetcher 下载 rawURL，返回分类后的契约错误。
func downloadClass(t *testing.T, rawURL string) *apiError {
	t.Helper()
	f := newFetcher(Config{AllowPrivateHosts: true, FetchTimeoutSeconds: 1, MaxInputBytes: 1 << 20})
	_, err := f.download(context.Background(), rawURL, t.TempDir(), "x.bin")
	if err == nil {
		t.Fatal("下载应失败")
	}
	return classifyError(err)
}

func TestUpstreamStatusClassification(t *testing.T) {
	transient := []int{http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable}
	permanent := []int{http.StatusNotFound, http.StatusGone, http.StatusBadRequest, http.StatusUnauthorized}

	for _, status := range append(append([]int{}, transient...), permanent...) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		ae := downloadClass(t, srv.URL+"/a.mp4")
		if contains(transient, status) {
			if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable || !ae.Retryable {
				t.Errorf("源站 %d 应为 transient 503 可重试，得到 code=%s status=%d", status, ae.Code, ae.status)
			}
		} else {
			if ae.Code != codeSourceUnavailable || ae.status != http.StatusUnprocessableEntity || ae.Retryable {
				t.Errorf("源站 %d 应为 source_unavailable 422 不可重试，得到 code=%s status=%d", status, ae.Code, ae.status)
			}
			if want := fmt.Sprintf("source.fetch_status=%d", status); ae.Check != want {
				t.Errorf("源站 %d 判据 = %q, want %q", status, ae.Check, want)
			}
		}
	}
}

// 连接被对端中途掐断：暂时失败。
func TestConnectionResetIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	ae := downloadClass(t, srv.URL+"/a.mp4")
	if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable {
		t.Fatalf("连接重置应为 transient: code=%s status=%d", ae.Code, ae.status)
	}
}

// 源站超时（响应慢于下载上限）：暂时失败。
func TestUpstreamTimeoutIsTransient(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	ae := downloadClass(t, srv.URL+"/a.mp4")
	if ae.Code != codeTransient {
		t.Fatalf("超时应为 transient: code=%s check=%s", ae.Code, ae.Check)
	}
}

func contains(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---- M2：排队上限与 I/O 不占计算名额 ----

func TestQueueWaitCappedAt120Seconds(t *testing.T) {
	cases := []struct {
		opTimeout int
		want      time.Duration
	}{
		{900, 120 * time.Second}, // 默认 30 分钟的处理时长，排队仍只等 120 秒
		{60, 60 * time.Second},
		{1, time.Second},
		{0, 120 * time.Second}, // 未配置
		{-5, 120 * time.Second},
	}
	for _, c := range cases {
		svc := &Service{cfg: Config{OpTimeoutSeconds: c.opTimeout}}
		if got := svc.queueWait(); got != c.want {
			t.Errorf("OpTimeoutSeconds=%d 排队上限 = %v, want %v", c.opTimeout, got, c.want)
		}
	}
}

// 输入读取（下载 / 请求体）阻塞期间，计算名额必须空闲：I/O 不占计算槽。
func TestInputReadDoesNotHoldComputeSlot(t *testing.T) {
	svc := NewService(Config{MaxConcurrent: 1, OpTimeoutSeconds: 60, MaxInputBytes: 1 << 20, TempDir: t.TempDir()})
	pr, pw := io.Pipe()
	started := make(chan struct{})
	var once sync.Once
	body := &signalReader{r: pr, onRead: func() { once.Do(func() { close(started) }) }}

	dir := t.TempDir()
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		p, err := svc.FromBody(body)(context.Background(), dir)
		done <- result{p, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("读取请求体没有开始")
	}
	// 读取还没结束：计算名额应当空闲（MaxConcurrent=1）
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := svc.acquire(ctx)
	if err != nil {
		t.Fatalf("输入读取占用了计算名额: %v", err)
	}
	release()

	if _, err := pw.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("读取完成后应成功: %v", res.err)
	}
}

// 计算名额被占满时，输入读取不受影响。
func TestComputeSlotDoesNotBlockInputRead(t *testing.T) {
	svc := NewService(Config{MaxConcurrent: 1, OpTimeoutSeconds: 60, MaxInputBytes: 1 << 20, TempDir: t.TempDir()})
	release, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	path, err := svc.FromBody(bytes.NewReader([]byte("abc")))(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("计算名额被占满时输入读取不应失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

type signalReader struct {
	r      io.Reader
	onRead func()
}

func (s *signalReader) Read(p []byte) (int, error) {
	s.onRead()
	return s.r.Read(p)
}

// ---- M3：ffmpeg 失败分类 ----

func TestFFmpegInterruptionsAreTransientNotRejected(t *testing.T) {
	// 上下文已取消（上游断开）
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// 进程被信号杀死（模拟 OOM 的 SIGKILL）
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	if killed == nil {
		t.Fatal("子进程应被杀死")
	}
	killedErr := fmt.Errorf("%w: %s", killed, "")

	for name, err := range map[string]error{"上下文取消": fmt.Errorf("exit status 1"), "信号杀死": killedErr} {
		ctx := context.Background()
		if name == "上下文取消" {
			ctx = canceled
		}
		ae := classifyError(classifyFFmpeg(ctx, err, "封面抽帧失败"))
		if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable || !ae.Retryable {
			t.Errorf("%s 应为 transient 503: code=%s status=%d", name, ae.Code, ae.status)
		}
		ae = classifyError(ffmpegFailure(ctx, err, "media.trim_failed"))
		if ae.Code != codeTransient {
			t.Errorf("%s（ffmpegFailure）应为 transient: code=%s", name, ae.Code)
		}
	}
}

func TestFFmpegDecodeFailureStaysRejected(t *testing.T) {
	// ffmpeg 正常退出但非零：这是输入解不开，仍是 422
	err := exec.Command("sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatal("退出码应非零")
	}
	ae := classifyError(classifyFFmpeg(context.Background(), err, "封面抽帧失败"))
	if ae.Code != codeSourceRejected || ae.status != http.StatusUnprocessableEntity {
		t.Fatalf("解码失败应为 source_rejected 422: code=%s status=%d", ae.Code, ae.status)
	}
	ae = classifyError(ffmpegFailure(context.Background(), err, "media.trim_failed"))
	if ae.Code != codeSourceRejected {
		t.Fatalf("ffmpegFailure 解码失败应为 source_rejected: code=%s", ae.Code)
	}
}

// 排队超时这类契约错误经过 classifyFFmpeg 时不能被改判成输入问题。
func TestContractErrorsPassThroughClassifyFFmpeg(t *testing.T) {
	ae := classifyError(classifyFFmpeg(context.Background(), unavailable("处理任务过多，请稍后重试"), "合成失败"))
	if ae.Code != codeTransient || ae.status != http.StatusServiceUnavailable {
		t.Fatalf("排队超时应保持 503 transient: code=%s status=%d", ae.Code, ae.status)
	}
}

// 抽帧：本机 ffmpeg 缺失是服务端问题（internal），不是素材问题。
func TestFramesMissingFFmpegIsInternalNotRejected(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mp4")
	makeClip(t, clip, "2", false)

	svc := NewService(Config{MaxConcurrent: 2, OpTimeoutSeconds: 60, TempDir: dir,
		FFmpeg: filepath.Join(dir, "no-such-ffmpeg")})
	_, err := svc.Frames(context.Background(), clip, 2)
	ae := classifyError(err)
	if ae.Code != codeInternal {
		t.Fatalf("本机缺 ffmpeg 应为 internal，得到 code=%s check=%s", ae.Code, ae.Check)
	}
}

// 抽帧：上下文已取消，判为瞬时而不是素材问题。
func TestFramesCanceledIsTransient(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mp4")
	makeClip(t, clip, "2", false)

	svc := NewService(Config{MaxConcurrent: 2, OpTimeoutSeconds: 60, TempDir: dir})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Frames(ctx, clip, 2)
	if ae := classifyError(err); ae.Code != codeTransient {
		t.Fatalf("取消的抽帧应为 transient，得到 code=%s check=%s", ae.Code, ae.Check)
	}
}

// ---- M4：ftyp 品牌细分 ----

// ftypFile 写一个最小的 ftyp 头：size(4) + "ftyp" + 主品牌(4) + 次版本(4) + 兼容品牌。
func ftypFile(t *testing.T, brand string) string {
	t.Helper()
	if len(brand) != 4 {
		t.Fatalf("品牌必须 4 字节: %q", brand)
	}
	b := []byte{0, 0, 0, 0x18}
	b = append(b, "ftyp"...)
	b = append(b, brand...)
	b = append(b, 0, 0, 0, 0)
	b = append(b, "isom"...)
	path := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMimeFromFtypBrand(t *testing.T) {
	cases := []struct{ brand, mime string }{
		{"M4A ", "audio/mp4"},
		{"M4B ", "audio/mp4"},
		{"M4P ", "audio/mp4"},
		{"heic", "image/heic"},
		{"heix", "image/heic"},
		{"mif1", "image/heic"},
		{"msf1", "image/heic"},
		{"avif", "image/avif"},
		{"avis", "image/avif"},
		{"qt  ", "video/quicktime"},
		{"isom", "video/mp4"},
		{"mp42", "video/mp4"},
		{"M4V ", "video/mp4"},
	}
	for _, c := range cases {
		path := ftypFile(t, c.brand)
		mime, size, err := mimeAndSize(path)
		if err != nil {
			t.Fatal(err)
		}
		if mime != c.mime {
			t.Errorf("品牌 %q 的 mime = %s, want %s", c.brand, mime, c.mime)
		}
		if st, _ := os.Stat(path); size != st.Size() {
			t.Errorf("品牌 %q 的大小 = %d, want %d", c.brand, size, st.Size())
		}
	}
}

// ---- M5：显示旋转 ----

func TestParseProbeSwapsDimensionsForQuarterTurns(t *testing.T) {
	stream := func(extra string) []byte {
		return []byte(`{"format":{"duration":"3.0"},"streams":[{"codec_type":"video","width":1920,"height":1080,` +
			`"avg_frame_rate":"30/1"` + extra + `}]}`)
	}
	cases := []struct {
		name  string
		extra string
		w, h  int
	}{
		{"displaymatrix -90", `,"side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]`, 1080, 1920},
		{"displaymatrix 90", `,"side_data_list":[{"side_data_type":"Display Matrix","rotation":90}]`, 1080, 1920},
		{"displaymatrix -270", `,"side_data_list":[{"side_data_type":"Display Matrix","rotation":-270}]`, 1080, 1920},
		{"displaymatrix 180", `,"side_data_list":[{"side_data_type":"Display Matrix","rotation":180}]`, 1920, 1080},
		{"无旋转", ``, 1920, 1080},
		{"旧版 rotate 标签 270", `,"tags":{"rotate":"270"}`, 1080, 1920},
		{"旧版 rotate 标签 0", `,"tags":{"rotate":"0"}`, 1920, 1080},
		{"侧数据优先于标签", `,"side_data_list":[{"side_data_type":"Display Matrix","rotation":0}],"tags":{"rotate":"90"}`, 1920, 1080},
	}
	for _, c := range cases {
		p, err := parseProbe(stream(c.extra))
		if err != nil {
			t.Fatal(err)
		}
		if p.Width != c.w || p.Height != c.h {
			t.Errorf("%s: 宽高 = %dx%d, want %dx%d", c.name, p.Width, p.Height, c.w, c.h)
		}
	}
}

// ---- M7：字节上传的体积上限按用途区分 ----

func TestStoreLocalLimitIsSeparateFromInputLimit(t *testing.T) {
	s := &Server{svc: &Service{cfg: Config{}}}
	if limit, _ := s.uploadLimit("intermediate"); limit != 2<<30 {
		t.Fatalf("我方产物默认上限 = %d, want 2 GiB", limit)
	}
	s = &Server{svc: &Service{cfg: Config{MaxInputBytes: 1024, StoreLocalMaxBytes: 4096}}}
	if limit, _ := s.uploadLimit("user_input"); limit != 1024 {
		t.Fatalf("用户输入上限 = %d, want MaxInputBytes", limit)
	}
	if limit, _ := s.uploadLimit("asset_library"); limit != 4096 {
		t.Fatalf("我方产物上限 = %d, want StoreLocalMaxBytes", limit)
	}
}

func TestUploadBytesSizeLimitsDependOnPurpose(t *testing.T) {
	requireFFmpeg(t) // 第三段用例靠媒体校验拒收垃圾字节
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(),
		MaxInputBytes: 1024, StoreLocalMaxBytes: 2048})

	// 用户输入超过 MaxInputBytes：422 size_exceeded
	body := assertAPIError(t, uploadBytes(t, ts.URL, "user_input", "k-limit-1", bytes.Repeat([]byte{1}, 4096)),
		http.StatusUnprocessableEntity, "source_rejected")
	if body.Error.Check != "source.size_exceeded" {
		t.Fatalf("用户输入超限判据 = %q", body.Error.Check)
	}

	// 我方产物超过 StoreLocalMaxBytes：400，不是素材拒收
	body = assertAPIError(t, uploadBytes(t, ts.URL, "intermediate", "k-limit-2", bytes.Repeat([]byte{1}, 4096)),
		http.StatusBadRequest, "invalid_request")
	if body.Error.Check != "request.body_too_large" {
		t.Fatalf("产物超限判据 = %q", body.Error.Check)
	}

	// 我方产物大于 MaxInputBytes 但小于 StoreLocalMaxBytes：不因体积被拒（之后按媒体校验，垃圾字节被拒为素材问题）
	body = assertAPIError(t, uploadBytes(t, ts.URL, "intermediate", "k-limit-3", bytes.Repeat([]byte{1}, 1500)),
		http.StatusUnprocessableEntity, "source_rejected")
	if body.Error.Check == "source.size_exceeded" || body.Error.Check == "request.body_too_large" {
		t.Fatalf("产物在 StoreLocalMaxBytes 内不应因体积被拒: %q", body.Error.Check)
	}
}

// ---- M8：同键处理中的状态码是 503 + Retry-After ----

func TestIdempotentBusyIsServiceUnavailable(t *testing.T) {
	s := &Server{idem: newMemIdemStore(time.Minute)}
	if _, state, _, _ := s.idem.begin(context.Background(), "route|k", [32]byte{}); state != idemFresh {
		t.Fatal("第一次应占用成功")
	}
	h := s.idempotent("route", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := idemRequest(h, "k", `{}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("处理中应为 503 + Retry-After: 5，得到 %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), `"retryable":true`) {
		t.Fatalf("处理中应可重试: %s", rec.Body.String())
	}
}
