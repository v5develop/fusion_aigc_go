package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// 契约校验与公共步骤。写操作的输出都经 storeFile 落桶。

const (
	maxFramesPerCall = 32  // 契约 frames.count 上限
	maxSecondsParam  = 600 // 契约 seconds 上限
)

var validPurposes = map[string]bool{"user_input": true, "intermediate": true, "asset_library": true, "third_party": true}

func invalid(check string) error { return newAPIError(codeInvalidRequest, check) }

// requireIdempotencyKey 写操作必须带 Idempotency-Key。
// TODO: 结果缓存未做；同一个 key 重放会重新计算（对象内容寻址，结果一致，但不省算力）。
func checkKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return invalid("request.idempotency_key_missing")
	}
	return nil
}

func checkPurpose(p string) error {
	if !validPurposes[p] {
		return invalid("request.purpose")
	}
	return nil
}

// checkSource 一般来源校验：不接受下载头（只有 ingest 的 url 来源可以带头，见 checkIngestSource）。
func checkSource(src SourceRef) error {
	if len(src.Headers) > 0 {
		return invalid("source.headers_unsupported")
	}
	return checkSourceBase(src)
}

// checkIngestSource ingest 的来源校验：url 来源可以带白名单下载头；rid / key 来源不允许带头。
func checkIngestSource(src SourceRef) error {
	if len(src.Headers) == 0 {
		return checkSourceBase(src)
	}
	if src.Url == "" {
		return invalid("source.headers_need_url")
	}
	if err := checkSourceBase(src); err != nil {
		return err
	}
	_, err := validateHeaders(src.Headers)
	return err
}

func checkSourceBase(src SourceRef) error {
	n := 0
	for _, set := range []bool{src.Rid != "", src.Url != "", src.Key != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return invalid("request.source_exactly_one")
	}
	if src.Rid != "" {
		if _, _, err := ParseRID(src.Rid); err != nil {
			return invalid("request.rid")
		}
	}
	if src.Key != "" {
		if classOfPath(src.Key) == "" {
			return invalid("request.key")
		}
		if !validStore(src.Store) {
			return invalid("request.source_store")
		}
	}
	return nil
}

func checkSeconds(v float64, lo, hi float64, strictLo bool) error {
	if v > hi || v < lo || (strictLo && v <= lo) {
		return invalid("request.seconds_range")
	}
	return nil
}

func checkRule(r RuleSpec) error {
	if r.TargetFps < 0 || r.MinSide < 0 || r.MaxSide < 0 || r.MinPixels < 0 || r.MaxPixels < 0 ||
		r.MinRatio < 0 || r.MaxRatio < 0 {
		return invalid("request.rule_negative")
	}
	return nil
}

// sourceFn 把契约来源变成 Service 认的 Source：url 走 v1 的缓存下载（SSRF 检查同 v1），key 从本地对象取。
func (s *Server) sourceFn(src SourceRef) Source {
	if src.Url != "" {
		// 头已在 checkIngestSource 里校验过；这里只取规范化结果
		h, _ := validateHeaders(src.Headers)
		return s.svc.FromURLWithHeaders(src.Url, h)
	}
	if src.Rid != "" {
		_, class, _ := ParseRID(src.Rid)
		path := storePath(class, s.id.ObjectKey(src.Rid))
		store := storeForClass(class, src.Store)
		return func(ctx context.Context, dir string) (string, error) {
			if err := s.checkReadable(ctx, store, path); err != nil {
				return "", err
			}
			return s.fetchObject(ctx, store, path, dir)
		}
	}
	key, store := src.Key, src.Store
	return func(ctx context.Context, dir string) (string, error) {
		if err := s.checkReadable(ctx, store, key); err != nil {
			return "", err
		}
		return s.fetchObject(ctx, store, key, dir)
	}
}

// fetchObject 从对象存储取回对象到 dir。属于 I/O，占输入名额而不是计算名额。
func (s *Server) fetchObject(ctx context.Context, store, path, dir string) (string, error) {
	var local string
	err := s.svc.withIO(ctx, func() error {
		var err error
		local, err = s.objects.Fetch(ctx, store, path, dir)
		return err
	})
	return local, err
}

// storeForClass 类别对应的存储。deliver 可以是私有或第三方公有，由调用方指定（默认私有）。
func storeForClass(class, requested string) string {
	switch class {
	case classAsset:
		return storeAsset
	case classDeliver:
		if requested == storeThirdParty {
			return storeThirdParty
		}
		return storePrivate
	default:
		return storePrivate
	}
}

// probeMeta 对本地文件取元数据（不落桶）。
func (s *Server) probeMeta(ctx context.Context, path string) (MediaMeta, error) {
	p, err := s.svc.Probe(ctx, path)
	if err != nil {
		return MediaMeta{}, err
	}
	// 像素上限在这里统一检查：所有读取素材的入口（ingest、probe、字节上传）都经过它
	if limit := s.svc.cfg.MaxSourcePixels; limit > 0 && int64(p.Width)*int64(p.Height) > limit {
		return MediaMeta{}, newAPIError(codeSourceRejected, "source.pixels_exceeded")
	}
	mime, size, err := mimeAndSize(path)
	if err != nil {
		return MediaMeta{}, err
	}
	return MediaMeta{
		Mime: mime, Size: size, DurationS: p.DurationSeconds, Width: p.Width, Height: p.Height,
		Fps: p.FPS, HasAudio: p.HasAudio,
	}, nil
}

// storeFile 把本地文件落桶，返回契约的 MediaResult。
func (s *Server) storeFile(ctx context.Context, purpose, path string) (MediaResult, error) {
	meta, err := s.probeMeta(ctx, path)
	if err != nil {
		return MediaResult{}, err
	}
	return s.storeWithMeta(ctx, purpose, path, meta)
}

// storeWithMeta 落桶的公共部分：元数据由调用方给（媒体用 probe，字体用文件头识别）。
func (s *Server) storeWithMeta(ctx context.Context, purpose, path string, meta MediaMeta) (MediaResult, error) {
	sum, _, err := hashFile(path)
	if err != nil {
		return MediaResult{}, err
	}
	class, store := classFor(purpose), storeFor(purpose)
	// rid 由幂等键派生；没有幂等键（如抽帧）时由内容摘要派生，同内容同类别得到同一个 rid
	source := idemKeyOf(ctx)
	if source == "" {
		source = sum
	}
	rid, err := s.id.RID(s.owners.ownerFor(ctx, source), class, source)
	if err != nil {
		return MediaResult{}, err
	}
	objPath := storePath(class, s.id.ObjectKey(rid))
	obj, err := s.objects.Put(ctx, store, objPath, path, meta.Mime)
	if err != nil {
		return MediaResult{}, err
	}
	// 同一幂等键重新写入：若对象之前被标记为待删除，写入即恢复为可用，否则宽限期结束会被误删
	if err := s.objects.ClearPending(ctx, store, objPath); err != nil {
		return MediaResult{}, err
	}
	obj.RID, obj.Class = rid, class
	u, err := s.objects.URL(ctx, obj)
	if err != nil {
		return MediaResult{}, err
	}
	return MediaResult{
		Object: ObjectRef{
			RID: rid, Class: class,
			Store: obj.Store, Sha256: sum, Size: obj.Size, Mime: obj.Mime,
			DurationS: meta.DurationS, Width: meta.Width, Height: meta.Height, Fps: meta.Fps, HasAudio: meta.HasAudio,
		},
		Url: URLRef{Kind: u.Kind, Value: u.Value, ExpiresAt: u.ExpiresAt},
	}, nil
}

// ffmpegFailure 转码失败：外部中断或进程被杀死是暂时的（与 classifyFFmpeg 同一判据）；缺 ffmpeg 是内部错误；
// 其余视为素材不合规。
func ffmpegFailure(ctx context.Context, err error, check string) error {
	var oe *opError
	if errors.As(err, &oe) {
		return err
	}
	if e := interruptedFailure(ctx, err, check); e != nil {
		return e
	}
	if errors.Is(err, exec.ErrNotFound) {
		return err // 落到 classifyError 的 internal
	}
	return newAPIError(codeSourceRejected, check)
}

// ftypMime 按 ISO BMFF 的 ftyp 主品牌细分类型（品牌为 4 字节，末尾可能补空格）。
// 其余 ftyp 一律按 MP4 处理，与历史行为一致。
func ftypMime(brand string) string {
	switch strings.TrimRight(brand, " ") {
	case "M4A", "M4B", "M4P":
		return "audio/mp4"
	case "heic", "heix", "mif1", "msf1":
		return "image/heic"
	case "avif", "avis":
		return "image/avif"
	case "qt":
		return "video/quicktime"
	default:
		return "video/mp4"
	}
}

// mimeAndSize 只认视频与音频、图片常见容器头；认不出来是 application/octet-stream。
func mimeAndSize(path string) (string, int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	head := make([]byte, 512)
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	n, _ := f.Read(head)
	head = head[:n]
	mime := "application/octet-stream"
	switch {
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		mime = ftypMime(string(head[8:12]))
	case len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3:
		mime = "video/webm"
	default:
		if m := http.DetectContentType(head); m != "application/octet-stream" && !strings.HasPrefix(m, "text/") {
			mime = m
		}
	}
	return mime, st.Size(), nil
}

// workFile 在 dir 下生成输出路径（只是路径，不创建文件）。
func workFile(dir, name string) string { return filepath.Join(dir, name) }

// ---- 规整（移植自网关 VideoConformance；与网关行为一致）----

const (
	durationTolerance = 0.05 // 秒
	fpsTolerance      = 0.01
)

// targetSize 按比例把 (w, h) 缩进 rule 的合法区间（偶数边长）。规则全为零值时原样返回。
// 比例越界返回错误（判据 video.ratio）。
func targetSize(w, h int, r RuleSpec) (int, int, error) {
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("video dimensions must be positive")
	}
	if r.MinSide == 0 && r.MaxSide == 0 && r.MinPixels == 0 && r.MaxPixels == 0 {
		return w, h, nil
	}
	minSide := 2.0
	if r.MinSide > 0 {
		minSide = float64(r.MinSide)
	}
	maxSide := 1_000_000.0
	if r.MaxSide > 0 {
		maxSide = float64(r.MaxSide)
	}
	minRatio := r.MinRatio // 0 即不限
	maxRatio := math.Inf(1)
	if r.MaxRatio > 0 {
		maxRatio = r.MaxRatio
	}
	ratio := float64(w) / float64(h)
	if ratio < minRatio || ratio > maxRatio {
		return 0, 0, fmt.Errorf("video aspect ratio %.3f outside [%g, %g]", ratio, minRatio, maxRatio)
	}
	pixels := float64(w) * float64(h)

	minScale := minSide / float64(min(w, h))
	if r.MinPixels > 0 {
		minScale = math.Max(minScale, math.Sqrt(float64(r.MinPixels)/pixels))
	}
	maxScale := maxSide / float64(max(w, h))
	if r.MaxPixels > 0 {
		maxScale = math.Min(maxScale, math.Sqrt(float64(r.MaxPixels)/pixels))
	}
	scale := math.Min(math.Max(1.0, minScale), maxScale)
	tw := evenAtLeast2(float64(w) * scale)
	th := evenAtLeast2(float64(h) * scale)

	for range 10_000 {
		tooBig := (r.MaxPixels > 0 && float64(tw*th) > float64(r.MaxPixels)) || float64(tw) > maxSide || float64(th) > maxSide
		tooSmall := (r.MinPixels > 0 && float64(tw*th) < float64(r.MinPixels)) || float64(tw) < minSide || float64(th) < minSide
		switch {
		case tooBig:
			tw, th = tw-2, th-2
		case tooSmall:
			tw, th = tw+2, th+2
		case float64(tw)/float64(th) < minRatio:
			th -= 2
		case float64(tw)/float64(th) > maxRatio:
			tw -= 2
		default:
			return tw, th, nil
		}
	}
	return tw, th, nil
}

// evenAtLeast2 与 Python 的 max(2, round(x / 2) * 2) 一致（round 为银行家舍入）。
func evenAtLeast2(x float64) int {
	return max(2, int(math.RoundToEven(x/2))*2)
}

// conformVideo 按规则规整输入视频，返回输出路径（不需要处理时返回输入本身）。
func (s *Server) conformVideo(ctx context.Context, dir, in string, req conformReq) (string, error) {
	p, err := s.svc.Probe(ctx, in)
	if err != nil {
		return "", err
	}
	if !p.HasVideo || p.Width <= 0 || p.Height <= 0 {
		return "", newAPIError(codeSourceRejected, "media.conform_video_stream")
	}
	tw, th, err := targetSize(p.Width, p.Height, req.Rule)
	if err != nil {
		return "", newAPIError(codeSourceRejected, "video.ratio")
	}
	needTrim := false
	if req.Seconds > 0 {
		if p.DurationSeconds <= 0 {
			return "", newAPIError(codeSourceRejected, "media.conform_video_duration")
		}
		if p.DurationSeconds < req.Seconds-durationTolerance {
			return "", newAPIError(codeSourceTooShort, "reference_video.shorter_than_seconds")
		}
		needTrim = p.DurationSeconds > req.Seconds+durationTolerance
	}
	needAudio := req.Rule.StripAudio && p.HasAudio
	needFps := req.Rule.TargetFps > 0 && (p.FPS == 0 || math.Abs(p.FPS-float64(req.Rule.TargetFps)) > fpsTolerance)
	needScale := tw != p.Width || th != p.Height
	if !needAudio && !needFps && !needScale && !needTrim {
		return in, nil
	}

	out := workFile(dir, "conform.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-threads", strconv.Itoa(max(1, s.svc.cfg.FFmpegThreads)), "-i", in}
	if needScale {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", tw, th))
	}
	if needFps {
		args = append(args, "-r", strconv.Itoa(req.Rule.TargetFps))
	}
	if req.Rule.StripAudio {
		args = append(args, "-an")
	} else {
		args = append(args, "-c:a", "aac")
	}
	if needTrim {
		args = append(args, "-t", strconv.FormatFloat(req.Seconds, 'f', -1, 64))
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart", out)
	if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
		return "", ffmpegFailure(ctx, err, "media.conform_transcode")
	}
	return out, nil
}

// imageSupported 图片规整能输出的格式（与网关 _PIL_FORMAT 一致）。
var imageSupported = []string{"image/png", "image/jpeg", "image/webp"}

// checkImageRule 长边区间必须非负且 min <= max（max 为 0 表示不限）。
func checkImageRule(r imageRuleReq) error {
	if r.MinLongSide < 0 || r.MaxLongSide < 0 {
		return invalid("request.rule_negative")
	}
	if r.MinLongSide > 0 && r.MaxLongSide > 0 && r.MinLongSide > r.MaxLongSide {
		return invalid("request.rule_range")
	}
	return nil
}

// conformImage 按规则缩放图片：长边落进 [min, max] 区间，等比缩放。
// 不需要缩放且格式允许时原样返回源文件（不重新编码）。
func (s *Server) conformImage(ctx context.Context, dir, in string, req imageConformReq) (string, error) {
	p, err := s.svc.Probe(ctx, in)
	if err != nil {
		return "", err
	}
	if p.Width <= 0 || p.Height <= 0 {
		return "", newAPIError(codeSourceRejected, "media.conform_image_size")
	}
	srcMime, _, err := mimeAndSize(in)
	if err != nil {
		return "", err
	}
	target := srcMime
	if len(req.Rule.MimeTypes) > 0 && !containsString(req.Rule.MimeTypes, srcMime) {
		target = firstSupported(req.Rule.MimeTypes)
	}
	w, h, changed := scaleLongSide(p.Width, p.Height, req.Rule.MinLongSide, req.Rule.MaxLongSide)
	if !changed && target == srcMime {
		return in, nil
	}
	out := filepath.Join(dir, "conformed"+extOf(target))
	args := []string{"-y", "-threads", s.svc.threads(), "-i", in}
	if changed {
		args = append(args, "-vf", "scale="+strconv.Itoa(w)+":"+strconv.Itoa(h))
	}
	args = append(args, "-frames:v", "1", out)
	if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
		return "", classifyFFmpeg(ctx, err, "图片规整失败")
	}
	return out, nil
}

// scaleLongSide 返回缩放后的尺寸和是否需要缩放。长边按区间等比缩放，另一边四舍五入，至少 1 像素。
func scaleLongSide(w, h, minLong, maxLong int) (int, int, bool) {
	long := max(w, h)
	scale := 1.0
	switch {
	case maxLong > 0 && long > maxLong:
		scale = float64(maxLong) / float64(long)
	case minLong > 0 && long < minLong:
		scale = float64(minLong) / float64(long)
	}
	if scale == 1.0 {
		return w, h, false
	}
	nw := max(1, int(math.Round(float64(w)*scale)))
	nh := max(1, int(math.Round(float64(h)*scale)))
	return nw, nh, true
}

func firstSupported(list []string) string {
	for _, m := range list {
		if containsString(imageSupported, m) {
			return m
		}
	}
	return "image/png"
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
