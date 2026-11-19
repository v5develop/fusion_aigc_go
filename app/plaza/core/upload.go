package core

// 模板素材上传（后台）：presign（签直传地址）→ 前端直传公有桶 → confirm（服务端核对对象并回传规范化 URL）。
// 模板素材对所有人可见，按落桶规则放公有桶；对象只能落在 <Storage.Prefix>/plaza-template/<yyyymm>/ 下。
// 不写任何模板表：上传在交接期（AdminWriteEnabled=false）与完整模式下都放行。

import (
	"aigc-local-life/pkg/safehttp"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // 注册解码器：只读宽高
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/videocover"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	_ "golang.org/x/image/webp"
)

// UploadDir 公有桶里模板素材的对象目录（相对 Storage.Prefix）。
const UploadDir = "plaza-template"

const (
	uploadKindImage = "image"
	uploadKindVideo = "video"
	minImageSide    = 512
	maxImageSide    = 8192
)

// 素材类型白名单：Content-Type → 扩展名（对象 key 的扩展名由服务端定，与类型一致）。
var uploadTypes = map[string]struct{ kind, ext string }{
	"image/jpeg": {uploadKindImage, "jpg"},
	"image/png":  {uploadKindImage, "png"},
	"image/webp": {uploadKindImage, "webp"},
	"video/mp4":  {uploadKindVideo, "mp4"},
	"video/webm": {uploadKindVideo, "webm"},
}

// 扩展名别名 → 规范扩展名（文件名与声明类型要对得上）。
var extAlias = map[string]string{"jpg": "jpg", "jpeg": "jpg", "png": "png", "webp": "webp", "mp4": "mp4", "webm": "webm"}

// UploadConfig 素材上传配置（挂在 Plaza.Upload）。
type UploadConfig struct {
	// Storage 对象存储。Mode=oss 才能直传；local（默认）下 presign 返回「功能暂未开放」。
	// 线上必须与 gateway-api 用同一个公有桶（读 afv_system_config 的 storage_public_*）。
	Storage storage.Conf `json:",optional"`
	// MaxImageMB / MaxVideoMB 单文件上限（MB）。
	MaxImageMB int `json:",default=10"`
	MaxVideoMB int `json:",default=200"`
	// PresignTTLMinutes 直传地址有效期（分钟），上限 10：pkg/storage 只有 URL 签名（V1），签不进 content-length-range，
	// 大小只能在 confirm 时核对，所以有效期压短以缩小「拿到地址后乱传」的窗口。
	PresignTTLMinutes int `json:",default=10"`
	// 视频 confirm 上限：时长（秒）与分辨率（长边 / 短边，不区分横竖）。超出拒收；ffprobe 不可用时无法核对。
	MaxVideoSeconds   int `json:",default=120"`
	MaxVideoLongSide  int `json:",default=3840"`
	MaxVideoShortSide int `json:",default=2160"`
	// FFmpeg / FFprobe 可执行文件路径；留空时 ffprobe 取 PATH 里的同名命令，ffmpeg 兜底抽帧默认关闭（只在显式配置 FFmpeg 时启用）。
	FFmpeg  string `json:",optional"`
	FFprobe string `json:",optional"`
	// Mediasvc 媒体服务（视频首帧封面）。BaseURLs 为空 = 不用媒体服务。
	Mediasvc struct {
		BaseURLs []string `json:",optional"`
		Token    string   `json:",optional"`
	} `json:",optional"`
	// Cleanup 废弃对象清理（upload_cleanup.go）。Enabled 默认 false：关闭时只入待清理队列、不删任何对象。
	Cleanup CleanupConfig `json:",optional"`
}

func (c UploadConfig) maxBytes(kind string) int64 {
	mb := c.MaxImageMB
	if kind == uploadKindVideo {
		mb = c.MaxVideoMB
	}
	if mb <= 0 {
		mb = 10
		if kind == uploadKindVideo {
			mb = 200
		}
	}
	return int64(mb) << 20
}

const maxPresignTTL = 10 * time.Minute

func (c UploadConfig) ttl() time.Duration {
	d := time.Duration(c.PresignTTLMinutes) * time.Minute
	if d <= 0 || d > maxPresignTTL {
		return maxPresignTTL
	}
	return d
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// CoverMaker 视频首帧封面：返回一个已在我们公有桶里的图片地址。
type CoverMaker interface {
	Cover(ctx context.Context, videoURL string) (string, error)
}

// ErrCoverUnavailable 没有可用的封面来源（媒体服务 / ffmpeg 都没配置）。
var ErrCoverUnavailable = errors.New("plaza: 没有可用的封面抽帧能力")

// Uploader 模板素材上传服务。
type Uploader struct {
	store  storage.Store
	cfg    UploadConfig
	prefix string
	cover  CoverMaker
	keyRe  *regexp.Regexp

	// 可替换，单测注入。
	fetch func(ctx context.Context, rawURL string, max int64) ([]byte, error)
	probe func(ctx context.Context, rawURL string) (*VideoProbe, error)
	// open 流式读对象（去重算 sha256 用）；dedup 为 nil = 不去重。
	open  func(ctx context.Context, rawURL string, max int64) (io.ReadCloser, error)
	dedup uploadDedup
	// orphans 孤儿对象待清理队列；nil = 只记日志（单测 / 没有库）。
	orphans orphanSink

	// 视频后处理（media_job*.go）：transcode 为 nil = 没有转码能力（只抽封面）；codec 为 nil 时用 ffprobe。
	transcode Transcoder
	codec     func(ctx context.Context, rawURL string) (*VideoCodec, error)
}

// VideoProbe ffprobe 结果。
type VideoProbe struct {
	Width, Height int
	DurationSec   float64
}

// errProbeUnavailable ffprobe 不可用（没装 / 没配）：视频不做深度校验，素材标 PENDING。
var errProbeUnavailable = errors.New("plaza: ffprobe 不可用")

// NewUploader 生产构造。store 为 nil（没配存储）时返回 nil，接口回「功能暂未开放」。
func NewUploader(store storage.Store, cfg UploadConfig) *Uploader {
	if store == nil {
		return nil
	}
	u := &Uploader{store: store, cfg: cfg, prefix: strings.Trim(cfg.Storage.Prefix, "/")}
	if u.prefix == "" {
		u.prefix = "aigc-local-life"
	}
	u.keyRe = compileKeyRe(u.prefix)
	u.fetch = httpFetch
	u.open = httpOpen
	u.probe = func(ctx context.Context, rawURL string) (*VideoProbe, error) {
		return ffprobe(ctx, cfg.FFprobe, rawURL, cfg.maxBytes(uploadKindVideo))
	}
	u.cover = newCoverMaker(store, cfg, u.prefix, u.fetch)
	if strings.TrimSpace(cfg.FFmpeg) != "" { // 显式配置 FFmpeg 才有转码能力（媒体服务没有通用转码接口）
		u.transcode = newFFmpegTranscoder(cfg.FFmpeg, store, u.newKey, cfg.maxBytes(uploadKindVideo))
	}
	return u
}

func compileKeyRe(prefix string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "/" + UploadDir + `/\d{6}/[0-9a-f]{32}\.(jpg|png|webp|mp4|webm)$`)
}

// SetUploader 装配上传服务（admin-api 启动时调用；nil = 未开放）。
func (s *Service) SetUploader(u *Uploader) {
	if u != nil && u.dedup == nil && s.db != nil {
		u.dedup = &sqlDedup{db: s.db} // 内容去重登记走 afv_plaza_upload_hash（Go_V1.0.44）
	}
	if u != nil && u.orphans == nil && s.db != nil {
		u.orphans = &sqlOrphans{db: s.db} // 孤儿待清理队列 afv_plaza_upload_orphan（Go_V1.0.48）
	}
	s.uploader = u
}

func (s *Service) requireUploader() (*Uploader, error) {
	if s.uploader == nil {
		return nil, ErrNotReady()
	}
	return s.uploader, nil
}

// PresignInput 申请直传地址。
type PresignInput struct {
	Kind        string `json:"kind"`        // image | video
	ContentType string `json:"contentType"` // 必填，必须在白名单内
	FileName    string `json:"fileName"`    // 可选；给了就校验扩展名与类型一致
	Size        int64  `json:"size"`        // 必填：字节数
}

// PresignResult 直传地址。前端用 method 把文件原样 PUT 到 uploadUrl，必须带 headers（Content-Type 要与签名一致）；
// 传完调 confirm。url 是传完后的公有地址（confirm 通过后才可写进模板）。
type PresignResult struct {
	UploadURL string            `json:"uploadUrl"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	URL       string            `json:"url"`
	Key       string            `json:"key"`
	Kind      string            `json:"kind"`
	MaxSize   int64             `json:"maxSize"`
	ExpiresAt int64             `json:"expiresAt"` // 毫秒时间戳
}

func normalizeContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct
}

func fileExt(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(strings.TrimSpace(name)), "."))
}

// AdminUploadPresign 校验类型 / 大小 / 扩展名后签直传地址。
func (s *Service) AdminUploadPresign(ctx context.Context, in PresignInput) (*PresignResult, error) {
	u, err := s.requireUploader()
	if err != nil {
		return nil, err
	}
	return u.Presign(ctx, in)
}

// Presign 见 Service.AdminUploadPresign。
func (u *Uploader) Presign(ctx context.Context, in PresignInput) (*PresignResult, error) {
	ct := normalizeContentType(in.ContentType)
	t, ok := uploadTypes[ct]
	if !ok {
		return nil, errParam("不支持的文件类型 " + in.ContentType + "（图片 jpg / png / webp，视频 mp4 / webm）")
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind != "" && kind != t.kind {
		return nil, errParam("文件类型与 kind 不一致")
	}
	if in.FileName != "" {
		ext, ok := extAlias[fileExt(in.FileName)]
		if !ok || ext != t.ext {
			return nil, errParam("文件扩展名与类型不一致（" + in.FileName + "）")
		}
	}
	max := u.cfg.maxBytes(t.kind)
	if in.Size <= 0 {
		return nil, errParam("size 必填（文件字节数）")
	}
	if in.Size > max {
		return nil, errParam(fmt.Sprintf("文件太大：%s 最大 %d MB", map[string]string{uploadKindImage: "图片", uploadKindVideo: "视频"}[t.kind], max>>20))
	}
	key := u.newKey(t.ext)
	p, err := u.store.PresignPut(ctx, key, ct, u.cfg.ttl())
	if errors.Is(err, storage.ErrDirectUploadUnsupported) {
		return nil, xerr.New(xerr.NotReady, "当前环境未开启直传（存储为本地模式）")
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 签直传地址失败 判据=store.PresignPut 出错 key=%s: %v", key, err)
		return nil, xerr.New(xerr.ServerError, "获取上传地址失败，请重试")
	}
	return &PresignResult{
		UploadURL: p.PutURL, Method: http.MethodPut, Headers: map[string]string{"Content-Type": ct},
		URL: p.URL, Key: key, Kind: t.kind, MaxSize: max, ExpiresAt: p.ExpiresAt.UnixMilli(),
	}, nil
}

// newKey <prefix>/plaza-template/<yyyymm>/<随机>.<ext>
func (u *Uploader) newKey(ext string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s/%s/%s/%s.%s", u.prefix, UploadDir, time.Now().Format("200601"), hex.EncodeToString(b), ext)
}

// ConfirmInput 确认直传完成。
type ConfirmInput struct {
	URL  string `json:"url"`
	Kind string `json:"kind"` // 可选；给了就必须与对象实际类型一致
}

// ConfirmResult 确认结果：url 可直接写进模板。
type ConfirmResult struct {
	URL         string  `json:"url"`
	Kind        string  `json:"kind"`
	ContentType string  `json:"contentType"`
	Size        int64   `json:"size"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	DurationSec float64 `json:"durationSec,omitempty"`
	// CoverURL 视频首帧封面（公有桶），仅供编辑器即时预览；抽不出来为空，不影响流程：保存模板后由后台任务重新抽帧 / 转码。
	CoverURL string `json:"coverUrl,omitempty"`
	// MediaStatus 视频恒为 PROCESSING（保存进模板后由后台任务处理，完成置 READY）；图片为 READY。仅作提示，状态以服务端为准。
	MediaStatus string `json:"mediaStatus"`
	Note        string `json:"note,omitempty"`
}

// checkOwnedURL 返回规范化的对象 key。key 必须整体严格匹配
// <prefix>/plaza-template/<yyyymm>/<32位hex>.<jpg|png|webp|mp4|webm>，且就是 URL 路径本身（去掉前导 /）：
// 不接受路径里任意位置夹带 plaza-template（例如别的业务目录下的同名子目录）。
func (u *Uploader) checkOwnedURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errParam("url 必填")
	}
	if err := storage.Owned(u.store, raw); err != nil {
		return "", errParam("只能确认通过上传接口上传的素材")
	}
	pu, err := url.Parse(raw)
	if err != nil || pu.RawQuery != "" || pu.Fragment != "" || pu.User != nil {
		return "", errParam("url 不合法")
	}
	key := strings.TrimPrefix(pu.EscapedPath(), "/")
	if !strings.HasPrefix(key, u.prefix+"/"+UploadDir+"/") || !u.keyRe.MatchString(key) {
		return "", errParam("素材不在模板素材目录里，请通过上传接口上传")
	}
	return key, nil
}

// AdminUploadConfirm 核对对象并回传规范化 URL。
func (s *Service) AdminUploadConfirm(ctx context.Context, in ConfirmInput) (*ConfirmResult, error) {
	u, err := s.requireUploader()
	if err != nil {
		return nil, err
	}
	return u.Confirm(ctx, in)
}

// Confirm 见 Service.AdminUploadConfirm。对象已在桶里却被拒收时，记一条 PLAZA_UPLOAD_REJECTED_ORPHAN
// 日志并入待清理队列 afv_plaza_upload_orphan；是否真删由 Plaza.Upload.Cleanup.Enabled 决定（默认关）。
// 不在拒收当场删：拒收可能是读对象的瞬时失败，客户端会对同一个 URL 重试 confirm，所以留宽限期（OrphanGraceHours）。
func (u *Uploader) Confirm(ctx context.Context, in ConfirmInput) (*ConfirmResult, error) {
	key, err := u.checkOwnedURL(in.URL)
	if err != nil {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=%v url=%s", err, in.URL)
		return nil, err
	}
	out, existed, err := u.confirm(ctx, key, strings.TrimSpace(in.URL), in.Kind)
	if err != nil && existed {
		logx.WithContext(ctx).Errorf("plaza: PLAZA_UPLOAD_REJECTED_ORPHAN key=%s url=%s 原因=%v（对象已在桶里但被拒收，已入待清理队列）", key, in.URL, err)
		u.enqueueOrphan(ctx, key, in.URL, orphanRejected)
	}
	if err != nil {
		return nil, err
	}
	return u.dedupe(ctx, key, out), nil
}

// confirm 返回 existed=true 表示对象确实在桶里（拒收后会成为孤儿对象）。
func (u *Uploader) confirm(ctx context.Context, key, raw, kindIn string) (*ConfirmResult, bool, error) {
	size, ct, err := u.store.Stat(ctx, raw)
	if err != nil {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=对象不存在或读不到(HEAD) key=%s: %v", key, err)
		return nil, false, errParam("没有找到上传的文件，请重新上传")
	}
	ct = normalizeContentType(ct)
	t, ok := uploadTypes[ct]
	if !ok {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=对象 Content-Type=%q 不在白名单 key=%s", ct, key)
		return nil, true, errParam("文件类型不支持（图片 jpg / png / webp，视频 mp4 / webm）")
	}
	if ext := fileExt(key); extAlias[ext] != t.ext {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=扩展名 %q 与 Content-Type %q 不一致 key=%s", ext, ct, key)
		return nil, true, errParam("文件扩展名与类型不一致")
	}
	if k := strings.ToLower(strings.TrimSpace(kindIn)); k != "" && k != t.kind {
		return nil, true, errParam("文件实际是" + map[string]string{uploadKindImage: "图片", uploadKindVideo: "视频"}[t.kind] + "，与所选类型不符")
	}
	max := u.cfg.maxBytes(t.kind)
	if size <= 0 {
		return nil, true, errParam("文件是空的，请重新上传")
	}
	if size > max {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=大小 %d 超过上限 %d key=%s", size, max, key)
		return nil, true, errParam(fmt.Sprintf("文件太大：最大 %d MB", max>>20))
	}
	out := &ConfirmResult{URL: raw, Kind: t.kind, ContentType: ct, Size: size, MediaStatus: model.MediaStatusReady}
	if t.kind == uploadKindVideo {
		out.MediaStatus = model.MediaStatusProcessing
	}
	if t.kind == uploadKindImage {
		return out, true, u.confirmImage(ctx, key, raw, t.ext, max, out)
	}
	return out, true, u.confirmVideo(ctx, key, raw, t.ext, out)
}

func (u *Uploader) confirmImage(ctx context.Context, key, raw, ext string, max int64, out *ConfirmResult) error {
	data, err := u.fetch(ctx, raw, max)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 上传确认拒收 判据=读取图片失败 key=%s: %v", key, err)
		return errParam("读取图片失败，请重新上传")
	}
	sniff := http.DetectContentType(data)
	if sniff != out.ContentType {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=真实类型 %q 与声明 %q 不一致 key=%s", sniff, out.ContentType, key)
		return errParam("图片内容与类型不符（不是真正的 " + ext + " 图片）")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=图片解码失败 key=%s: %v", key, err)
		return errParam("图片已损坏或格式不受支持")
	}
	if cfg.Width < minImageSide || cfg.Height < minImageSide || cfg.Width > maxImageSide || cfg.Height > maxImageSide {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=宽高 %dx%d 不在 [%d,%d] key=%s", cfg.Width, cfg.Height, minImageSide, maxImageSide, key)
		return errParam(fmt.Sprintf("图片宽高需在 %d~%d 像素之间（当前 %d×%d）", minImageSide, maxImageSide, cfg.Width, cfg.Height))
	}
	out.Width, out.Height = cfg.Width, cfg.Height
	return nil
}

func sniffVideo(head []byte, ct string) bool {
	switch ct {
	case "video/mp4":
		return len(head) >= 12 && string(head[4:8]) == "ftyp"
	case "video/webm":
		return len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3
	}
	return false
}

func (u *Uploader) confirmVideo(ctx context.Context, key, raw, ext string, out *ConfirmResult) error {
	head, err := u.fetch(ctx, raw, 4096)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 上传确认拒收 判据=读取视频头失败 key=%s: %v", key, err)
		return errParam("读取视频失败，请重新上传")
	}
	if !sniffVideo(head, out.ContentType) {
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=视频文件头与声明类型 %q 不一致 key=%s", out.ContentType, key)
		return errParam("视频内容与类型不符（不是真正的 " + ext + " 视频）")
	}
	pr, err := u.probe(ctx, raw)
	switch {
	case errors.Is(err, errProbeUnavailable):
		out.Note = "未做视频深度校验（ffprobe 不可用）"
		logx.WithContext(ctx).Errorf("plaza: 视频 %s 未能探测时长 判据=ffprobe 不可用，跳过时长 / 分辨率校验", key)
	case err != nil:
		logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=ffprobe 判定不是可播放视频 key=%s: %v", key, err)
		return errParam("视频无法解析，请确认文件完整可播放")
	default:
		out.Width, out.Height, out.DurationSec = pr.Width, pr.Height, pr.DurationSec
		maxSec := orDefault(u.cfg.MaxVideoSeconds, 120)
		long, short := orDefault(u.cfg.MaxVideoLongSide, 3840), orDefault(u.cfg.MaxVideoShortSide, 2160)
		l, sh := pr.Width, pr.Height
		if sh > l {
			l, sh = sh, l
		}
		if pr.DurationSec > float64(maxSec) {
			logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=视频时长 %.1fs 超过上限 %ds key=%s", pr.DurationSec, maxSec, key)
			return errParam(fmt.Sprintf("视频时长不能超过 %d 秒（当前 %.0f 秒）", maxSec, pr.DurationSec))
		}
		if l > long || sh > short {
			logx.WithContext(ctx).Infof("plaza: 上传确认拒收 判据=视频分辨率 %dx%d 超过上限 %dx%d key=%s", pr.Width, pr.Height, long, short, key)
			return errParam(fmt.Sprintf("视频分辨率不能超过 %d×%d（当前 %d×%d）", long, short, pr.Width, pr.Height))
		}
	}
	cover, cerr := u.cover.Cover(ctx, raw)
	if cerr != nil || cover == "" {
		if out.Note == "" {
			out.Note = "封面暂未生成，保存后由后台任务重试；多次失败可手动上传封面"
		}
		logx.WithContext(ctx).Errorf("plaza: 视频 %s 确认时封面抽帧不可用 判据=%v，交给后台视频处理任务", key, cerr)
		return nil
	}
	out.CoverURL = cover
	return nil
}

// ---------------------------------------------------------------- 默认实现

// uploadHTTP 走 safehttp：按实际解析 IP 拦内网 / 保留地址、限重定向；我们的桶与媒体服务地址都是公网 https，不受影响。
var uploadHTTP = safehttp.NewClient(2*time.Minute, false)

// httpFetch 读取至多 max 字节；对象超过 max 时图片调用方会在后续校验里发现不完整，这里用 Range 限量，不会整段拉大文件。
func httpFetch(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(max-1, 10))
	resp, err := uploadHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// ffprobe 读首个视频流的宽高与总时长。没装 / 没配返回 errProbeUnavailable；文件无视频流 / 解析失败返回其它错误。
func ffprobe(ctx context.Context, bin, rawURL string, maxBytes int64) (*VideoProbe, error) {
	if strings.TrimSpace(bin) == "" {
		bin = "ffprobe"
	}
	out, err := runProbe(ctx, bin, rawURL, maxBytes, "stream=width,height:format=duration")
	if err != nil {
		return nil, err
	}
	p := &VideoProbe{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "width":
			p.Width, _ = strconv.Atoi(v)
		case "height":
			p.Height, _ = strconv.Atoi(v)
		case "duration":
			p.DurationSec, _ = strconv.ParseFloat(v, 64)
		}
	}
	if p.Width <= 0 || p.Height <= 0 {
		return nil, errors.New("ffprobe: 没有视频流")
	}
	return p, nil
}

// coverMaker 先走媒体服务，再（显式配了 FFmpeg 时）本机 ffmpeg 抽帧；产物统一落我们的公有桶。
type coverMaker struct {
	store  storage.Store
	prefix string
	ms     *mediasvc.Client
	ex     *videocover.Extractor
	fetch  func(ctx context.Context, rawURL string, max int64) ([]byte, error)
}

func newCoverMaker(store storage.Store, cfg UploadConfig, prefix string, fetch func(context.Context, string, int64) ([]byte, error)) CoverMaker {
	m := &coverMaker{store: store, prefix: prefix, fetch: fetch}
	if len(cfg.Mediasvc.BaseURLs) > 0 {
		m.ms = mediasvc.New(cfg.Mediasvc.BaseURLs, cfg.Mediasvc.Token)
	}
	if strings.TrimSpace(cfg.FFmpeg) != "" {
		m.ex = &videocover.Extractor{FFmpeg: cfg.FFmpeg, FFprobe: cfg.FFprobe}
	}
	return m
}

func (m *coverMaker) Cover(ctx context.Context, videoURL string) (string, error) {
	var lastErr error = ErrCoverUnavailable
	if m.ms != nil {
		u, err := m.ms.CoverURL(ctx, videoURL)
		if err == nil {
			if m.store.Owns(u) {
				return u, nil
			}
			// 媒体服务给的可能是签名地址（会过期）：转存到我们的公有桶
			if data, ferr := m.fetch(ctx, u, 10<<20); ferr == nil && len(data) > 0 {
				return m.put(ctx, data)
			} else {
				err = fmt.Errorf("转存封面失败: %v", ferr)
			}
		}
		lastErr = err
	}
	if m.ex != nil {
		// 本机 ffmpeg 不碰网络：先经 safehttp 下载到临时文件（体积封顶）再抽帧
		data, err := m.ex.ExtractURL(ctx, videoURL)
		if err == nil {
			return m.put(ctx, data)
		}
		lastErr = err
	}
	return "", lastErr
}

func (m *coverMaker) put(ctx context.Context, jpg []byte) (string, error) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := fmt.Sprintf("%s/%s/%s/%s.jpg", m.prefix, UploadDir, time.Now().Format("200601"), hex.EncodeToString(b))
	return m.store.Put(ctx, key, "image/jpeg", jpg)
}
