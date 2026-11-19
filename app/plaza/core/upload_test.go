package core

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/storage"
)

const upBase = "https://cdn.example.com"

// fakeStore 只实现上传用到的方法；其余方法走嵌入的 nil 接口（被调用即 panic，说明用到了不该用的能力）。
type fakeStore struct {
	storage.Store
	prefix   string
	direct   bool
	statSize int64
	statCT   string
	statErr  error
	puts     []string
}

func (f *fakeStore) Owns(u string) bool { return strings.HasPrefix(u, upBase+"/"+f.prefix+"/") }
func (f *fakeStore) PresignPut(_ context.Context, key, ct string, _ time.Duration) (*storage.Presigned, error) {
	if !f.direct {
		return nil, storage.ErrDirectUploadUnsupported
	}
	return &storage.Presigned{PutURL: "https://bkt.oss.example.com/" + key + "?Signature=x", URL: upBase + "/" + key, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (f *fakeStore) Stat(context.Context, string) (int64, string, error) {
	return f.statSize, f.statCT, f.statErr
}
func (f *fakeStore) Put(_ context.Context, key, _ string, _ []byte) (string, error) {
	f.puts = append(f.puts, key)
	return upBase + "/" + key, nil
}

type fakeCover struct {
	url string
	err error
}

func (c fakeCover) Cover(context.Context, string) (string, error) { return c.url, c.err }

func newTestUploader(f *fakeStore) *Uploader {
	f.prefix = "aigc"
	cfg := UploadConfig{}
	return &Uploader{store: f, cfg: cfg, prefix: "aigc", keyRe: compileKeyRe("aigc"), cover: fakeCover{err: ErrCoverUnavailable},
		fetch: func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("no fetch") },
		probe: func(context.Context, string) (*VideoProbe, error) { return nil, errProbeUnavailable }}
}

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

const hex32 = "0123456789abcdef0123456789abcdef"

// objURL 合法的素材地址：扩展名取自 name。
func objURL(name string) string {
	return upBase + "/aigc/" + UploadDir + "/202611/" + hex32 + "." + fileExt(name)
}

func TestPresign(t *testing.T) {
	ctx := context.Background()
	u := newTestUploader(&fakeStore{direct: true})
	r, err := u.Presign(ctx, PresignInput{Kind: "image", ContentType: "image/png", FileName: "A.PNG", Size: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Key, "aigc/"+UploadDir+"/") || !strings.HasSuffix(r.Key, ".png") || r.Headers["Content-Type"] != "image/png" ||
		r.Method != "PUT" || !strings.HasPrefix(r.URL, upBase+"/aigc/"+UploadDir+"/") || r.MaxSize != 10<<20 || r.Kind != "image" {
		t.Fatalf("presign 结果不对: %+v", r)
	}
	if !u.keyRe.MatchString(r.Key) {
		t.Fatalf("presign 的 key 必须能通过 confirm 的严格匹配: %s", r.Key)
	}
	if (UploadConfig{PresignTTLMinutes: 30}).ttl() != 10*time.Minute || (UploadConfig{}).ttl() != 10*time.Minute ||
		(UploadConfig{PresignTTLMinutes: 3}).ttl() != 3*time.Minute {
		t.Fatal("签名有效期上限应为 10 分钟")
	}
	bad := []PresignInput{
		{ContentType: "image/gif", Size: 1},                                   // 不在白名单
		{ContentType: "image/png", Size: 0},                                   // 缺 size
		{ContentType: "image/png", Size: 10<<20 + 1},                          // 图片超 10MB
		{ContentType: "video/mp4", Size: 200<<20 + 1},                         // 视频超 200MB
		{ContentType: "image/png", FileName: "a.jpg", Size: 1},                // 扩展名与类型不符
		{ContentType: "video/mp4", FileName: "a.exe", Size: 1},                // 扩展名不在白名单
		{Kind: "video", ContentType: "image/png", FileName: "a.png", Size: 1}, // kind 与类型不符
	}
	for i, in := range bad {
		if _, err := u.Presign(ctx, in); err == nil {
			t.Errorf("用例 %d 应被拒: %+v", i, in)
		}
	}
	if _, err := u.Presign(ctx, PresignInput{ContentType: "video/mp4", Size: 200 << 20}); err != nil {
		t.Errorf("200MB 视频应放行: %v", err)
	}
	local := newTestUploader(&fakeStore{direct: false})
	if _, err := local.Presign(ctx, PresignInput{ContentType: "image/png", Size: 1}); err == nil {
		t.Error("本地存储应提示不支持直传")
	}
}

func TestConfirmImage(t *testing.T) {
	ctx := context.Background()
	img := pngBytes(600, 700)
	f := &fakeStore{statSize: int64(len(img)), statCT: "image/png"}
	u := newTestUploader(f)
	u.fetch = func(context.Context, string, int64) ([]byte, error) { return img, nil }
	r, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png"), Kind: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Width != 600 || r.Height != 700 || r.MediaStatus != model.MediaStatusReady || r.URL != objURL("a.png") {
		t.Fatalf("结果不对: %+v", r)
	}

	// 不在素材目录 / 外链 / 带查询串 / 路径穿越
	for _, raw := range []string{
		upBase + "/aigc/other/a.png", "https://evil.example.com/aigc/" + UploadDir + "/a.png",
		objURL("a.png") + "?x=1", upBase + "/aigc/" + UploadDir + "/../x.png", "",
		// 用户上传路径里夹带 plaza-template：只要 key 不是严格的 <prefix>/plaza-template/<yyyymm>/<32hex>.<ext> 就拒
		upBase + "/aigc/image/7/20261101/" + UploadDir + "/202611/" + hex32 + ".png",
		upBase + "/aigc/user/" + UploadDir + "/202611/" + hex32 + ".png",
		upBase + "/aigc/" + UploadDir + "/202611/sub/" + hex32 + ".png",
		upBase + "/aigc/" + UploadDir + "/202611/" + hex32 + ".gif",
		upBase + "/aigc/" + UploadDir + "/202611/abc.png",            // 不是 32 位 hex
		upBase + "/aigc/" + UploadDir + "/2026111/" + hex32 + ".png", // yyyymm 位数不对
		upBase + "/aigc/" + UploadDir + "/202611/" + strings.ToUpper(hex32) + ".png",
	} {
		if _, err := u.Confirm(ctx, ConfirmInput{URL: raw}); err == nil {
			t.Errorf("应拒绝 %q", raw)
		}
	}
	// 太小
	u.fetch = func(context.Context, string, int64) ([]byte, error) { return pngBytes(100, 100), nil }
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png")}); err == nil {
		t.Error("100×100 应被拒")
	}
	// Content-Type 与真实内容不符（声明 jpeg，实为 png）
	f.statCT = "image/jpeg"
	u.fetch = func(context.Context, string, int64) ([]byte, error) { return img, nil }
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.jpg")}); err == nil {
		t.Error("真实类型与声明不符应被拒")
	}
	// 超大 / 空 / 对象不存在 / 类型不在白名单
	f.statCT, f.statSize = "image/png", 10<<20+1
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png")}); err == nil {
		t.Error("超 10MB 应被拒")
	}
	f.statSize = 0
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png")}); err == nil {
		t.Error("空文件应被拒")
	}
	f.statSize, f.statErr = 10, errors.New("HTTP 404")
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png")}); err == nil {
		t.Error("对象不存在应被拒")
	}
	f.statErr, f.statCT = nil, "application/pdf"
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png")}); err == nil {
		t.Error("不在白名单的类型应被拒")
	}
}

func TestConfirmVideo(t *testing.T) {
	ctx := context.Background()
	mp4 := append([]byte{0, 0, 0, 0x20}, []byte("ftypisom\x00\x00\x02\x00")...)
	f := &fakeStore{statSize: 5 << 20, statCT: "video/mp4"}
	u := newTestUploader(f)
	u.fetch = func(context.Context, string, int64) ([]byte, error) { return mp4, nil }

	// ffprobe 不可用 + 无封面能力：通过；视频恒为 PROCESSING（保存后由后台任务处理），不再有 PENDING
	r, err := u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4"), Kind: "video"})
	if err != nil {
		t.Fatal(err)
	}
	if r.MediaStatus != model.MediaStatusProcessing || r.CoverURL != "" || r.Note == "" {
		t.Fatalf("应为 PROCESSING 且带备注: %+v", r)
	}
	// ffprobe 可用 + 封面成功：带时长与封面，状态仍是 PROCESSING（转码 / 封面落库交给后台任务）
	u.probe = func(context.Context, string) (*VideoProbe, error) {
		return &VideoProbe{Width: 1080, Height: 1920, DurationSec: 12.5}, nil
	}
	u.cover = fakeCover{url: objURL("c.jpg")}
	r, err = u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4")})
	if err != nil {
		t.Fatal(err)
	}
	if r.MediaStatus != model.MediaStatusProcessing || r.CoverURL != objURL("c.jpg") || r.Width != 1080 || r.DurationSec != 12.5 {
		t.Fatalf("应为 PROCESSING 带封面: %+v", r)
	}
	// 时长 / 分辨率上限（默认 120 秒、3840×2160，不分横竖）
	u.probe = func(context.Context, string) (*VideoProbe, error) {
		return &VideoProbe{Width: 1080, Height: 1920, DurationSec: 120}, nil
	}
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4")}); err != nil {
		t.Fatalf("120 秒竖屏 1080p 应放行: %v", err)
	}
	for name, p := range map[string]VideoProbe{"超时长": {1080, 1920, 120.5}, "超分辨率": {3841, 2160, 10}, "竖屏超分辨率": {2160, 3841, 10}, "4K 以上短边": {4000, 2200, 10}} {
		pp := p
		u.probe = func(context.Context, string) (*VideoProbe, error) { return &pp, nil }
		if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4")}); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
	u.cfg.MaxVideoSeconds, u.cfg.MaxVideoLongSide, u.cfg.MaxVideoShortSide = 30, 1920, 1080
	u.probe = func(context.Context, string) (*VideoProbe, error) {
		return &VideoProbe{Width: 1920, Height: 1080, DurationSec: 31}, nil
	}
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4")}); err == nil {
		t.Error("可配上限 30 秒，31 秒应被拒")
	}
	u.cfg.MaxVideoSeconds, u.cfg.MaxVideoLongSide, u.cfg.MaxVideoShortSide = 0, 0, 0
	// ffprobe 判定不是视频：拒收
	u.probe = func(context.Context, string) (*VideoProbe, error) { return nil, errors.New("没有视频流") }
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4")}); err == nil {
		t.Error("ffprobe 判定损坏应被拒")
	}
	// 文件头不是 mp4
	u.fetch = func(context.Context, string, int64) ([]byte, error) { return []byte("<html>not a video</html>"), nil }
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4")}); err == nil {
		t.Error("伪装成视频的文件应被拒")
	}
	// 声明 kind=image 但实为视频
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("v.mp4"), Kind: "image"}); err == nil {
		t.Error("kind 不符应被拒")
	}
}

func TestResolveMediaStatus(t *testing.T) {
	// 视频且有预览视频 → PROCESSING；图片 / 视频草稿还没填视频 → READY
	if resolveMediaStatus("image", "https://x/v.mp4") != model.MediaStatusReady || resolveMediaStatus("video", "") != model.MediaStatusReady ||
		resolveMediaStatus("video", "  ") != model.MediaStatusReady || resolveMediaStatus("video", "https://x/v.mp4") != model.MediaStatusProcessing {
		t.Fatal("resolveMediaStatus 规则不对")
	}
	// 兼容：旧前端带回的 PENDING / READY / PROCESSING 提示合法（被忽略），其它值非法
	for _, v := range []string{"PENDING", "READY", "processing"} {
		in := TemplateInput{Title: "t", MediaStatus: v}
		if err := validateTemplateInput(&in); err != nil {
			t.Fatalf("%s 应放行: %v", v, err)
		}
	}
	in := TemplateInput{Title: "t", MediaStatus: "bogus"}
	if validateTemplateInput(&in) == nil {
		t.Fatal("非法 mediaStatus 应被拒")
	}
}

func TestValidateTemplateInputFields(t *testing.T) {
	ok := func(in TemplateInput) error { return validateTemplateInput(&in) }
	base := TemplateInput{Title: "t"}
	if err := ok(base); err != nil {
		t.Fatal(err)
	}
	bad := map[string]TemplateInput{
		"result 不是数组":     {Title: "t", Payload: []byte(`{"result":"x"}`)},
		"result 非 URL":    {Title: "t", Payload: []byte(`{"result":["javascript:alert(1)"]}`)},
		"image_urls 太多":   {Title: "t", Payload: []byte(`{"image_urls":[` + strings.Repeat(`"https://a/b.png",`, 20) + `"https://a/c.png"]}`)},
		"guidance 太长":     {Title: "t", Payload: []byte(`{"guidance":"` + strings.Repeat("字", 1001) + `"}`)},
		"prompt 不是文本":     {Title: "t", Payload: []byte(`{"prompt":{"a":1}}`)},
		"videoUrl 非 URL":  {Title: "t", MediaType: "video", VideoURL: "ftp://x"},
		"posterUrl 非 URL": {Title: "t", PosterURL: "data:image/png;base64,AAAA"},
		"标签太多":            {Title: "t", Tags: make([]string, 21)},
		"标签太长":            {Title: "t", Tags: []string{strings.Repeat("长", 33)}},
	}
	for name, in := range bad {
		if err := ok(in); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
	good := TemplateInput{Title: "t", Payload: []byte(`{"result":["https://a/b.png"],"image_urls":[],"guidance":"g","prompt":"p","other":{"k":1}}`),
		MediaType: "video", VideoURL: "https://a/v.mp4", PosterURL: "https://a/p.jpg", Tags: []string{"a"}}
	if err := ok(good); err != nil {
		t.Fatalf("合法输入被拒: %v", err)
	}
}

// 探测本地 m3u8（引用网络地址）不得发起任何网络请求：-protocol_whitelist file,pipe 兜住。
func TestRunProbeNoNetworkFromPlaylist(t *testing.T) {
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("没有 ffprobe")
	}
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()
	pl := filepath.Join(t.TempDir(), "a.m3u8")
	_ = os.WriteFile(pl, []byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n"+srv.URL+"/seg.ts\n#EXT-X-ENDLIST\n"), 0o600)
	_, _ = runProbe(context.Background(), fp, pl, 1<<20, "stream=width")
	select {
	case <-hit:
		t.Fatal("ffprobe 对本地 m3u8 发起了网络请求")
	default:
	}
}

// 内网地址（回环）被 safehttp 拒收，httpFetch 与 runProbe 都不例外。
func TestFetchAndProbeRejectPrivateIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer srv.Close()
	if _, err := httpFetch(context.Background(), srv.URL, 1024); err == nil {
		t.Error("httpFetch 应拒绝内网地址")
	}
	if _, err := runProbe(context.Background(), "ffprobe", srv.URL, 1024, "stream=width"); err == nil || errors.Is(err, errProbeUnavailable) {
		t.Errorf("runProbe 应因下载被拒而失败: %v", err)
	}
}
