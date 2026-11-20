package logic

import (
	"context"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/mediasvc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 候选里可能存的引用（长期，不是签名地址）：
//   - mrw://{rid}[.ext]  studio 免费档水印视图：读时只能签成带 x-oss-process 水印参数的地址（FreeWatermark.Process），
//     签不出 / 没配处理串 → 空串，绝不签干净原件；
//   - mr://{rid}[.ext]   私有资源引用：读时现签。
//
//   - 画布自己的产物在异步转存素材库完成前也是 mr://{rid}.{ext}（见 library_copy.go），转存完成后换成素材库公开地址。
//
// 绝不把 600 秒的签名地址落库；读出口（RunResult 候选与视频 / 画布回填 / 上游候选引用）统一过 resolveMediaRefs。

const (
	refPrefix   = "mr://"
	refWMPrefix = "mrw://"
	refURLTTL   = 10 * time.Minute
)

// refRID mr:// / mrw:// 引用 → (rid, 是否水印视图, 是否引用)。
func refRID(v string) (rid string, watermark, ok bool) {
	v = strings.TrimSpace(v)
	body := ""
	switch {
	case strings.HasPrefix(v, refWMPrefix):
		body, watermark = strings.TrimPrefix(v, refWMPrefix), true
	case strings.HasPrefix(v, refPrefix):
		body = strings.TrimPrefix(v, refPrefix)
	default:
		return "", false, false
	}
	if i := strings.LastIndexByte(body, '.'); i > 0 {
		body = body[:i]
	}
	if !mediasvc.ValidRID(body) {
		return "", watermark, true // 引用但不合法 → 调用方置空
	}
	return body, watermark, true
}

func isMediaRef(v string) bool { _, _, ok := refRID(v); return ok }

// resolveMediaRefs 把 urls 里的 mr:// / mrw:// 就地换成现签地址；签不出置空（前端当占位）。其它值不动。
func (c *core) resolveMediaRefs(ctx context.Context, urls []*string) {
	type key struct {
		rid string
		wm  bool
	}
	got := map[key]string{}
	var plain, wm []string
	for _, p := range urls {
		if p == nil {
			continue
		}
		rid, w, ok := refRID(*p)
		if !ok || rid == "" {
			continue
		}
		k := key{rid, w}
		if _, seen := got[k]; seen {
			continue
		}
		got[k] = ""
		if w {
			wm = append(wm, rid)
		} else {
			plain = append(plain, rid)
		}
	}
	if len(got) > 0 {
		c.signRefs(ctx, plain, "", func(rid, u string) { got[key{rid, false}] = u })
		c.signRefs(ctx, wm, c.s.Config.FreeWatermark.Process, func(rid, u string) { got[key{rid, true}] = u })
	}
	for _, p := range urls {
		if p == nil {
			continue
		}
		if rid, w, ok := refRID(*p); ok {
			*p = got[key{rid, w}] // 不合法 / 签失败 = 空串
		}
	}
}

// signRefs 逐个现签（失败的那个为空）。水印视图 process 为空或媒体服务未配置 → 整批为空。
func (c *core) signRefs(ctx context.Context, rids []string, process string, set func(rid, u string)) {
	if len(rids) == 0 {
		return
	}
	m := c.s.Media
	if m == nil {
		return
	}
	for _, rid := range rids {
		if ctx.Err() != nil {
			return
		}
		var (
			urls map[string]string
			err  error
		)
		if process != "" {
			if !c.s.Config.FreeWatermark.ValidProcess() {
				logx.WithContext(ctx).Infof("canvas: warning 免费水印处理串未配置或不合法，水印视图输出为空 (check=canvas_free_watermark.process)")
				return
			}
			urls, err = m.ResourceURLsWithProcess(ctx, []string{rid}, c.s.Config.FreeWatermark.SignedTTL(), process)
		} else {
			urls, err = m.ResourceURLs(ctx, []string{rid}, refURLTTL)
		}
		if err != nil {
			logx.WithContext(ctx).Infof("canvas: 引用现签失败，置空 rid=%s: %v", ridHint(rid), err)
			continue
		}
		set(rid, urls[rid])
	}
}

func ridHint(rid string) string {
	if len(rid) <= 12 {
		return rid
	}
	return rid[:6] + "…" + rid[len(rid)-4:]
}

// resolvedRuns 返回候选 / 视频引用已换成现签地址的浅拷贝（不改原 run，不回写库）。
func (c *core) resolvedRuns(ctx context.Context, runs []*model.NodeRun) []*model.NodeRun {
	out := make([]*model.NodeRun, len(runs))
	for i, r := range runs {
		out[i] = c.resolvedRun(ctx, r)
	}
	return out
}

// resolvedRun 候选与视频地址里的 mr:// / mrw:// 现签；LibraryState 在换掉引用之前按原值算好（异步转存未完成 = pending）。
func (c *core) resolvedRun(ctx context.Context, r *model.NodeRun) *model.NodeRun {
	if r == nil {
		return r
	}
	hasVideoRef := isMediaRef(r.VideoURL.String)
	if !hasVideoRef && (len(r.Candidates) == 0 || !strings.Contains(string(r.Candidates), "mr")) {
		return r
	}
	cp := *r
	cp.LibraryState = libraryStatus(r)
	list := r.CandidateList()
	var ptrs []*string
	for i := range list {
		ptrs = append(ptrs, &list[i].URL)
	}
	video := r.VideoURL.String
	if hasVideoRef {
		ptrs = append(ptrs, &video)
	}
	var thumbs []string
	if r.Kind != model.KindVideo { // 视频 run 的候选是封面帧，卡片展示的是视频本身，不额外签
		thumbs = c.candidateThumbs(ctx, list) // 按原引用算，必须在换成现签地址之前
	}
	c.resolveMediaRefs(ctx, ptrs)
	if thumbs != nil {
		for i := range list {
			if list[i].URL == "" {
				thumbs[i] = "" // 原图签不出 → 缩略图也不给（前端整格占位，不出现「缩略图能看、点开裂图」）
			}
		}
		cp.Thumbs = thumbs
	}
	if len(r.Candidates) > 0 {
		cp.Candidates = repo.MarshalCandidates(list)
	}
	if hasVideoRef {
		cp.VideoURL.String = video
	}
	return &cp
}

// thumbImageExt 能做 OSS 缩放的图片扩展名（引用没带扩展名按网关默认 PNG 处理）；GIF 缩放转 webp 会丢动画，不做。
var thumbImageExt = map[string]bool{"": true, "png": true, "jpg": true, "jpeg": true, "webp": true, "bmp": true}

// refExt mr:// / mrw:// 引用的扩展名（小写，没有为 ""）。
func refExt(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, refWMPrefix), refPrefix)
	if i := strings.LastIndexByte(v, '.'); i > 0 {
		return strings.ToLower(v[i+1:])
	}
	return ""
}

// watermarkThumbProcess 水印视图的缩略图处理串：把缩放接在水印处理链后面（先打水印再缩，缩略图上仍有水印）。
// 水印串不是 image/ 链（如命名样式 style/…）或已含缩放 / 格式转换时不敢叠，返回 "" = 不出缩略图（前端回退原图）。
func watermarkThumbProcess(wm, thumb string) string {
	if wm == "" || thumb == "" || !strings.HasPrefix(wm, "image/") || !strings.HasPrefix(thumb, "image/") {
		return ""
	}
	for _, op := range strings.Split(strings.TrimPrefix(wm, "image/"), "/") {
		if strings.HasPrefix(op, "resize,") || strings.HasPrefix(op, "format,") {
			return ""
		}
	}
	return wm + "/" + strings.TrimPrefix(thumb, "image/")
}

// candidateThumbs 候选缩略图（只增 2026-11-20，RunResult.candidates[].thumb_url / 画布回填 candidateThumbs）：
// 私有引用（mr:// / mrw://）的图片候选额外现签一个带缩放处理串的地址——处理串由媒体服务纳入签名，前端不能自己往签名地址上拼。
// 有效期与原图一致（mr:// 10 分钟，mrw:// 同水印视图）。素材库公开地址不在这里出（前端对公有未签名 OSS 地址自己拼缩放）。
// 返回与 list 同序的切片（"" = 没有）；一张都出不了返回 nil。缩略图只是加速，签失败一律置空，不影响原图。
func (c *core) candidateThumbs(ctx context.Context, list []model.Candidate) []string {
	thumb := c.s.Config.Thumb.ThumbProcess()
	if thumb == "" || c.s.Media == nil || len(list) == 0 || thumbsDisabled(ctx) {
		return nil
	}
	wmThumb := ""
	if c.s.Config.FreeWatermark.ValidProcess() {
		wmThumb = watermarkThumbProcess(c.s.Config.FreeWatermark.Process, thumb)
	}
	var plain, wm []string
	for _, x := range list {
		rid, w, ok := refRID(x.URL)
		if !ok || rid == "" || !thumbImageExt[refExt(x.URL)] {
			continue
		}
		if w {
			if wmThumb != "" {
				wm = append(wm, rid)
			}
		} else {
			plain = append(plain, rid)
		}
	}
	if len(plain) == 0 && len(wm) == 0 {
		return nil
	}
	plainURLs := c.signThumbs(ctx, plain, thumb, refURLTTL)
	wmURLs := c.signThumbs(ctx, wm, wmThumb, c.s.Config.FreeWatermark.SignedTTL())
	out := make([]string, len(list))
	got := false
	for i, x := range list {
		rid, w, ok := refRID(x.URL)
		if !ok || rid == "" || !thumbImageExt[refExt(x.URL)] {
			continue
		}
		if w {
			out[i] = wmURLs[rid]
		} else {
			out[i] = plainURLs[rid]
		}
		got = got || out[i] != ""
	}
	if !got {
		return nil
	}
	return out
}

// thumbSignConcurrency 缩略图逐个现签的并发上限（与媒体服务客户端整批签名的上限一致）。
const thumbSignConcurrency = 4

// signThumbs 一批 rid 带处理串逐个现签（有界并发），失败的那个为空。不走整批：媒体服务客户端整批里任一失败就整体报错、
// 不返回部分结果，再逐个重签会让永久签不出的图每次读都多付一轮调用；逐个签每个 rid 恰好一次调用。
func (c *core) signThumbs(ctx context.Context, rids []string, process string, ttl time.Duration) map[string]string {
	if len(rids) == 0 || process == "" {
		return nil
	}
	rids = uniqStrings(rids)
	m := c.s.Media
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, thumbSignConcurrency)
		out = make(map[string]string, len(rids))
	)
	for _, rid := range rids {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			urls, err := m.ResourceURLsWithProcess(ctx, []string{rid}, ttl, process)
			if err != nil {
				logx.WithContext(ctx).Infof("canvas: 缩略图现签失败，回退原图 rid=%s: %v", ridHint(rid), err)
				return
			}
			mu.Lock()
			out[rid] = urls[rid]
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// noThumbsKey 读出口不签缩略图的 context 标记（智能体工具出口不用 thumb_url / candidateThumbs，签了白费调用）。
type noThumbsKey struct{}

// withoutThumbs 返回不签缩略图的 ctx。
func withoutThumbs(ctx context.Context) context.Context {
	return context.WithValue(ctx, noThumbsKey{}, true)
}

func thumbsDisabled(ctx context.Context) bool { v, _ := ctx.Value(noThumbsKey{}).(bool); return v }

func uniqStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
