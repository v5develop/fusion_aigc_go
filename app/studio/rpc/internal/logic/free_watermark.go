package logic

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 免费档水印视图（与 Java FreeImageWatermark / MediaRefs.WATERMARK_PREFIX 同一套约定）：
//
//   - 交付：免费档用户（pkg/usertier.WatermarkedImage）且 FreeWatermark.Active() 时，成品不 CopyToLibrary，
//     原件就是网关产物（media_purpose=intermediate → 私有 work 桶），result_rid = 该 rid，
//     result_url 与素材库行写 mrw://{rid}.{ext}；
//   - 读：所有对外出口（批次详情 / 历史 / 任务 / 重做 / 取消 / 校色 / 后台）把 mrw:// 签成带 x-oss-process 水印参数的地址
//     （媒体服务把参数纳入签名，去掉即 403）；签不出或没配处理串 → 空串，绝不下发干净签名或原始引用；
//   - 付费后：promoteFreeOriginals 把原件复制到公有素材库桶，改写子任务与素材库行（Java FreeOriginalSweeper.promote 的 studio 版）。

// watermarkRef rid + mime → mrw://{rid}.{ext}。
func watermarkRef(rid, mime string) string {
	return repo.WatermarkRefPrefix + rid + "." + extOfMime(mime)
}

// isWatermarkRef 是否是水印视图引用。
func isWatermarkRef(v string) bool {
	return strings.HasPrefix(strings.TrimSpace(v), repo.WatermarkRefPrefix)
}

// watermarkRidOf mrw://{rid}[.ext] → rid（去掉最后一个点之后的扩展名）；不是水印视图或 rid 不合法返回 ""。
func watermarkRidOf(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, repo.WatermarkRefPrefix) {
		return ""
	}
	rid := strings.TrimPrefix(v, repo.WatermarkRefPrefix)
	if i := strings.LastIndexByte(rid, '.'); i > 0 {
		rid = rid[:i]
	}
	if !mediasvc.ValidRID(rid) {
		return ""
	}
	return rid
}

func extOfMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	}
	return "png" // 网关图片产物默认 PNG
}

// processSigner 带处理参数的现签（*mediasvc.Client 实现）。
type processSigner interface {
	ResourceURLsWithProcess(ctx context.Context, rids []string, ttl time.Duration, process string) (map[string]string, error)
}

// signTTL 水印地址签名有效期（最少 60 秒，默认 600 秒，与 Java signed-ttl-seconds 一致）。
func signTTL(c config.FreeWatermarkConf) time.Duration {
	s := c.SignedTTLSeconds
	if s <= 0 {
		s = 600
	}
	return time.Duration(max(60, s)) * time.Second
}

// signedCache 已签水印地址的进程内缓存（明显短于签名有效期：轮询不必每次都打媒体服务）。键 = 处理串|rid。
var signedCache = struct {
	sync.Mutex
	m map[string]signedEntry
}{m: map[string]signedEntry{}}

type signedEntry struct {
	url string
	at  time.Time
}

const signedCacheMax = 20000

// signedIndex 已签水印地址 → rid 的反查表（键 = host + path，不含签名查询串），让「用户把自己成品的签名地址当输入」
// 能映射回 mrw:// 引用并校验归属（见 deps.ownsSigned）。进程内、带过期（签名有效期 + 余量）；多实例时只认本实例签出的地址，
// 别的实例签的会被当成外部地址拒收（前端重取一次结果即可），宁可拒收也不放行未校验归属的私有桶地址。
var signedIndex = struct {
	sync.Mutex
	m map[string]indexEntry
}{m: map[string]indexEntry{}}

type indexEntry struct {
	rid string
	exp time.Time
}

func urlIndexKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host) + u.EscapedPath()
}

func indexSigned(rid, signed string, ttl time.Duration, now time.Time) {
	k := urlIndexKey(signed)
	if k == "" {
		return
	}
	signedIndex.Lock()
	defer signedIndex.Unlock()
	if len(signedIndex.m) > signedCacheMax {
		for kk, e := range signedIndex.m {
			if now.After(e.exp) {
				delete(signedIndex.m, kk)
			}
		}
		if len(signedIndex.m) > signedCacheMax {
			signedIndex.m = map[string]indexEntry{}
		}
	}
	signedIndex.m[k] = indexEntry{rid: rid, exp: now.Add(ttl + time.Minute)}
}

// ridOfSignedURL 本实例签出的水印地址 → rid；没签过 / 已过期返回 ""。
func ridOfSignedURL(raw string, now time.Time) string {
	k := urlIndexKey(raw)
	if k == "" {
		return ""
	}
	signedIndex.Lock()
	defer signedIndex.Unlock()
	if e, ok := signedIndex.m[k]; ok && now.Before(e.exp) {
		return e.rid
	}
	return ""
}

func cacheTTL(ttl time.Duration) time.Duration { return min(240*time.Second, ttl/2) }

// plainSigner 不带处理参数的现签（*mediasvc.Client 实现）：付费先交付的 mr:// 成品用。
type plainSigner interface {
	ResourceURLs(ctx context.Context, rids []string, ttl time.Duration) (map[string]string, error)
}

// resolveWatermarkViews 把 ptrs 里的 mrw:// 就地换成带水印参数的签名地址、mr://（付费先交付、待转存素材库的成品）换成普通签名地址；
// 签不出的置空（绝不下发原始引用）。其它值不动。process 为空 / 不合法或 signer 为 nil 时，水印视图一律置空。
// mr:// 只会出现在付费交付（watermarkDelivery=false）的成品上，免费档成品永远是 mrw://，所以这里签干净地址不会让免费用户看到原图。
func resolveWatermarkViews(ctx context.Context, signer processSigner, c config.FreeWatermarkConf, ptrs []*string) {
	resolvePlainRefs(ctx, signer, c, ptrs)
	var views []*string
	var rids []string
	seen := map[string]bool{}
	for _, p := range ptrs {
		if p == nil || !isWatermarkRef(*p) {
			continue
		}
		views = append(views, p)
		if rid := watermarkRidOf(*p); rid != "" && !seen[rid] {
			seen[rid] = true
			rids = append(rids, rid)
		}
	}
	if len(views) == 0 {
		return
	}
	var urls map[string]string
	if len(rids) > 0 {
		urls = signWatermarkViews(ctx, signer, c, rids)
	}
	for _, p := range views {
		*p = urls[watermarkRidOf(*p)] // 签不出 / 引用不合法 = 空串（失败关闭）
	}
}

// resolvePlainRefs 把 ptrs 里的 mr:// 就地换成普通签名地址（有效期同水印视图），签不出的置空。
func resolvePlainRefs(ctx context.Context, signer processSigner, c config.FreeWatermarkConf, ptrs []*string) {
	var refs []*string
	var rids []string
	seen := map[string]bool{}
	for _, p := range ptrs {
		if p == nil || !isPendingRef(*p) {
			continue
		}
		refs = append(refs, p)
		if rid := mrRidOf(*p); rid != "" && !seen[rid] {
			seen[rid] = true
			rids = append(rids, rid)
		}
	}
	if len(refs) == 0 {
		return
	}
	var urls map[string]string
	if len(rids) > 0 {
		ps, _ := signer.(plainSigner)
		urls = signPlainRefs(ctx, ps, c, rids)
	}
	for _, p := range refs {
		*p = urls[mrRidOf(*p)] // 签不出 / 引用不合法 = 空串（失败关闭）
	}
}

// plainCacheKey 普通签名在 signedCache 里的键（处理串不会是这个值：ValidProcess 不允许空格）。
const plainCacheKey = "plain "

func signPlainRefs(ctx context.Context, signer plainSigner, c config.FreeWatermarkConf, rids []string) map[string]string {
	log := logx.WithContext(ctx)
	if signer == nil {
		log.Infow("studio: warning private refs blanked: no media service (check=studio_library_copy.sign)", logx.Field("count", len(rids)))
		return nil
	}
	ttl := signTTL(c)
	now := time.Now()
	out := make(map[string]string, len(rids))
	var miss []string
	signedCache.Lock()
	for _, rid := range rids {
		if e, ok := signedCache.m[plainCacheKey+"|"+rid]; ok && now.Sub(e.at) < cacheTTL(ttl) {
			out[rid] = e.url
		} else {
			miss = append(miss, rid)
		}
	}
	signedCache.Unlock()
	if len(miss) == 0 {
		return out
	}
	got, err := signer.ResourceURLs(ctx, miss, ttl)
	if err != nil { // 整批失败时逐个重签，只让坏的那几个为空
		got = map[string]string{}
		for _, rid := range miss {
			if ctx.Err() != nil {
				break
			}
			one, e := signer.ResourceURLs(ctx, []string{rid}, ttl)
			if e != nil {
				log.Infow("studio: warning private ref sign failed, blanked (check=studio_library_copy.sign)",
					logx.Field("rid_hint", ridHint(rid)), logx.Field("err", e.Error()))
				continue
			}
			got[rid] = one[rid]
		}
	}
	signedCache.Lock()
	if len(signedCache.m) > signedCacheMax {
		signedCache.m = map[string]signedEntry{}
	}
	for _, rid := range miss {
		if u := got[rid]; u != "" {
			out[rid] = u
			signedCache.m[plainCacheKey+"|"+rid] = signedEntry{url: u, at: now}
			// 用户把自己成品的签名地址当输入时能反查回 rid 并校验归属（ownsSigned，result_rid 就是该 rid）
			indexSigned(rid, u, ttl, now)
		}
	}
	signedCache.Unlock()
	return out
}

func signWatermarkViews(ctx context.Context, signer processSigner, c config.FreeWatermarkConf, rids []string) map[string]string {
	log := logx.WithContext(ctx)
	if signer == nil || !c.ValidProcess() {
		log.Infow("studio: warning watermark views blanked: no media service or FreeWatermark.Process (check=studio_free_watermark.process)",
			logx.Field("count", len(rids)))
		return nil
	}
	ttl := signTTL(c)
	now := time.Now()
	out := make(map[string]string, len(rids))
	var miss []string
	signedCache.Lock()
	for _, rid := range rids {
		if e, ok := signedCache.m[c.Process+"|"+rid]; ok && now.Sub(e.at) < cacheTTL(ttl) {
			out[rid] = e.url
		} else {
			miss = append(miss, rid)
		}
	}
	signedCache.Unlock()
	if len(miss) == 0 {
		return out
	}
	got, err := signer.ResourceURLsWithProcess(ctx, miss, ttl, c.Process)
	if err != nil { // 整批失败（个别 rid 坏）时逐个重签，只让坏的那几个为空
		got = map[string]string{}
		for _, rid := range miss {
			if ctx.Err() != nil {
				break
			}
			one, e := signer.ResourceURLsWithProcess(ctx, []string{rid}, ttl, c.Process)
			if e != nil {
				log.Infow("studio: warning watermark view sign failed, blanked (check=studio_free_watermark.sign)",
					logx.Field("rid_hint", ridHint(rid)), logx.Field("err", e.Error()))
				continue
			}
			got[rid] = one[rid]
		}
	}
	signedCache.Lock()
	if len(signedCache.m) > signedCacheMax {
		signedCache.m = map[string]signedEntry{}
	}
	for _, rid := range miss {
		if u := got[rid]; u != "" {
			out[rid] = u
			signedCache.m[c.Process+"|"+rid] = signedEntry{url: u, at: now}
			indexSigned(rid, u, ttl, now)
		}
	}
	signedCache.Unlock()
	return out
}

// ridHint 日志里只打 rid 的前后几位。
func ridHint(rid string) string {
	if len(rid) <= 12 {
		return rid
	}
	return rid[:6] + "…" + rid[len(rid)-4:]
}

// signerOf 别把 nil *mediasvc.Client 装进接口。
func signerOf(s *svc.ServiceContext) processSigner {
	if s == nil || s.MediaSvc == nil {
		return nil
	}
	return s.MediaSvc
}

// signViews 对外出口统一调用：任务的 result_url、批次的 cover_url 里的水印视图签成水印地址。
func signViews(ctx context.Context, s *svc.ServiceContext, batches []*studio.BatchInfo, tasks []*studio.TaskInfo) {
	var c config.FreeWatermarkConf
	if s != nil {
		c = s.Config.FreeWatermark
	}
	resolveWatermarkViews(ctx, signerOf(s), c, viewPtrs(batches, tasks))
}

// viewPtrs 对外结构里可能是水印视图的字段。
func viewPtrs(batches []*studio.BatchInfo, tasks []*studio.TaskInfo) []*string {
	var ptrs []*string
	for _, b := range batches {
		if b != nil {
			ptrs = append(ptrs, &b.CoverUrl)
		}
	}
	for _, t := range tasks {
		if t != nil {
			ptrs = append(ptrs, &t.ResultUrl)
		}
	}
	return ptrs
}

// detailViews 批次详情（含子任务）里的批次与任务。
func detailViews(details []*studio.BatchDetail) ([]*studio.BatchInfo, []*studio.TaskInfo) {
	var bs []*studio.BatchInfo
	var ts []*studio.TaskInfo
	for _, d := range details {
		if d != nil {
			bs = append(bs, d.Batch)
			ts = append(ts, d.Tasks...)
		}
	}
	return bs, ts
}

// signDetails 批次详情（含子任务）。
func signDetails(ctx context.Context, s *svc.ServiceContext, details ...*studio.BatchDetail) {
	bs, ts := detailViews(details)
	signViews(ctx, s, bs, ts)
}

// ---------------------------------------------------------------- 交付

// freeTier 交付时判免费档（*usertier.Resolver 满足）。
type freeTier interface {
	WatermarkedImage(ctx context.Context, userID int64) bool
}

// watermarkDelivery 该用户的新成品是否走水印视图：开关开、处理串可用、且是免费档（展示水印开 + 分层开 + 没付过钱）。
func (f *flow) watermarkDelivery(ctx context.Context, userID int64) bool {
	if f.tier == nil || !f.cfg.FreeWatermark.Active() {
		return false
	}
	return f.tier.WatermarkedImage(ctx, userID)
}

// firstMediaMime 成功调用的第一张产物的 mime（拼 mrw:// 扩展名用，与 pipeline.ParseOutputs 同一顺序）。
func firstMediaMime(call *aigateway.Call) string {
	if call == nil || call.Result == nil {
		return ""
	}
	if m := call.Result.Media(); len(m) > 0 {
		return m[0].Mime
	}
	return ""
}

// maskSourceURL 自动补蒙版用的源地址：水印视图 / 私有引用 mr:// 签一条干净的短期地址只给网关分割用（不落库、不下发给用户）；
// 其它原样。签不出返回 ""（跳过蒙版）。
func (f *flow) maskSourceURL(ctx context.Context, resultURL string) string {
	if !isWatermarkRef(resultURL) && !isPendingRef(resultURL) {
		return resultURL
	}
	rid := watermarkRidOf(resultURL)
	if rid == "" {
		rid = mrRidOf(resultURL) // 付费成品转存素材库永久失败时仍是 mr://
	}
	if rid == "" || f.media == nil {
		return ""
	}
	urls, err := f.media.ResourceURLs(ctx, []string{rid}, 10*time.Minute)
	if err != nil {
		logx.WithContext(ctx).Infow("studio mask: warning sign original for segment failed (check=task_mask_source)",
			logx.Field("rid_hint", ridHint(rid)), logx.Field("err", err.Error()))
		return ""
	}
	return urls[rid]
}

// ---------------------------------------------------------------- 付费转存

// promoteBatch 每轮最多转存多少张。
const promoteBatch = 50

// promoteFreeOriginals 已付费用户的水印视图成品 → 公有素材库原图（每 FreeWatermark.PromoteMinutes 一次，各实例各跑，
// 复制按幂等键、改写按旧值 CAS，并发安全）。与开关无关：开关关掉后已落库的 mrw:// 仍要能在付费后换成原图。
func (f *flow) promoteFreeOriginals(ctx context.Context) int {
	every := f.cfg.FreeWatermark.PromoteMinutes
	if every <= 0 || f.media == nil || f.repo == nil {
		return 0
	}
	now := f.now()
	if !f.promoteAt.IsZero() && now.Sub(f.promoteAt) < time.Duration(every)*time.Minute {
		return 0
	}
	f.promoteAt = now
	log := logx.WithContext(ctx)
	list, err := f.repo.Task.ListPromotableFreeOriginals(ctx, f.promoteAfter, promoteBatch)
	if err != nil {
		log.Errorw("studio: free original promote scan failed (check=studio_free_original.promote_scan)", logx.Field("err", err.Error()))
		return 0
	}
	if len(list) < promoteBatch {
		f.promoteAfter = 0 // 扫到尾，下一轮从头
	} else {
		f.promoteAfter = list[len(list)-1].ID
	}
	done := 0
	for _, t := range list {
		if ctx.Err() != nil {
			break
		}
		if err := f.promoteOne(ctx, t); err != nil {
			log.Errorw("studio: free original promote failed, retry next round (check=studio_free_original.promote)",
				logx.Field("task_id", t.TaskID.String), logx.Field("err", err.Error()))
			continue
		}
		done++
	}
	return done
}

func (f *flow) promoteOne(ctx context.Context, t repo.FreeOriginal) error {
	rid := watermarkRidOf(t.ResultURL)
	if rid == "" {
		return &mediasvc.Error{Status: 422, Code: "bad_ref", Message: "水印视图引用格式不正确"}
	}
	lib, err := f.media.CopyToLibrary(ctx, rid, mediasvc.PurposeAssetLibrary, "studio-free-promote:"+rid)
	if err == nil && lib.URL == "" {
		err = &mediasvc.Error{Status: 422, Code: "no_public_url", Message: "素材库桶没有公开地址"}
	}
	if err != nil {
		return err
	}
	// 先改素材库，再改子任务：子任务的 CAS 是「转存完成」的标记，素材库没改成就留着 mrw:// 下一轮重来（按旧值匹配，幂等）
	if t.TaskID.Valid && t.TaskID.String != "" {
		if _, err := f.repo.Task.PromoteAssetRefs(ctx, t.UserID, t.TaskID.String, t.ResultURL, lib.URL); err != nil {
			return err
		}
	}
	ok, err := f.repo.Task.PromoteFreeOriginal(ctx, t.ID, t.ResultURL, lib.RID, lib.URL)
	if err != nil {
		return err
	}
	if ok {
		logx.WithContext(ctx).Infow("studio: free original promoted (check=studio_free_original.promoted)",
			logx.Field("task_id", t.TaskID.String), logx.Field("user_id", t.UserID))
	}
	return nil
}

// ---------------------------------------------------------------- 到期清理

// mediaDeleter 媒体服务的删除能力（*mediasvc.Client 实现；测试替身可不实现）。
type mediaDeleter interface {
	Delete(ctx context.Context, rid, idem string) error
}

// cleanupExpiredFreeOriginals 免费档水印视图成品超过保留期（FreeWatermark.RetentionDays，从子任务结束时间算）仍未付费：
// 删私有原件、逻辑删除素材库行与子任务（Java FreeOriginalSweeper.cleanupExpired 的 studio 版，同样只处理终态成功任务）。
// 默认关（CleanupEnabled），各实例各跑：删除按幂等键、子任务软删带 CAS，并发安全；任一步失败留到下一轮重试（先删对象再删库，
// 对象删除幂等，库行没删成下轮重来）。
func (f *flow) cleanupExpiredFreeOriginals(ctx context.Context) int {
	c := f.cfg.FreeWatermark
	del, ok := f.media.(mediaDeleter)
	if !c.CleanupEnabled || c.CleanupMinutes <= 0 || c.RetentionDays <= 0 || !ok || f.repo == nil || f.dropAssets == nil {
		return 0
	}
	now := f.now()
	if !f.cleanAt.IsZero() && now.Sub(f.cleanAt) < time.Duration(c.CleanupMinutes)*time.Minute {
		return 0
	}
	f.cleanAt = now
	log := logx.WithContext(ctx)
	before := now.AddDate(0, 0, -c.RetentionDays)
	list, err := f.repo.Task.ListExpiredFreeOriginals(ctx, before, f.cleanAfter, promoteBatch)
	if err != nil {
		log.Errorw("studio: free original expire scan failed (check=studio_free_original.expire_scan)", logx.Field("err", err.Error()))
		return 0
	}
	if len(list) < promoteBatch {
		f.cleanAfter = 0
	} else {
		f.cleanAfter = list[len(list)-1].ID
	}
	done := 0
	for _, t := range list {
		if ctx.Err() != nil {
			break
		}
		if err := f.expireOne(ctx, del, t); err != nil {
			log.Errorw("studio: free original expire failed, retry next round (check=studio_free_original.expire)",
				logx.Field("task_id", t.TaskID.String), logx.Field("err", err.Error()))
			continue
		}
		done++
	}
	return done
}

func (f *flow) expireOne(ctx context.Context, del mediaDeleter, t repo.FreeOriginal) error {
	rid := watermarkRidOf(t.ResultURL)
	if rid == "" {
		return &mediasvc.Error{Status: 422, Code: "bad_ref", Message: "水印视图引用格式不正确"}
	}
	if err := del.Delete(ctx, rid, "studio-free-expire:"+rid); err != nil {
		return err
	}
	if t.TaskID.Valid && t.TaskID.String != "" {
		if err := f.dropAssets(ctx, t.UserID, t.TaskID.String); err != nil {
			return err
		}
	}
	if _, err := f.repo.Task.SoftDeleteFreeOriginal(ctx, t.ID, t.ResultURL); err != nil {
		return err
	}
	logx.WithContext(ctx).Infow("studio: free original expired (check=studio_free_original.expired)",
		logx.Field("task_id", t.TaskID.String), logx.Field("user_id", t.UserID))
	return nil
}

// logFreeWatermark 启动时打印免费水印的生效状态（与 Java check=free_watermark.process 对照：两边处理串必须相同）。
func logFreeWatermark(c config.FreeWatermarkConf) {
	switch {
	case c.Enabled && !c.ValidProcess():
		logx.Errorf("[FreeWatermark] studio 免费水印已开启但 Process 为空或含不允许的字符，免费用户成品照旧转存公有桶 (check=studio_free_watermark.process)")
	case c.Enabled:
		logx.Infof("[FreeWatermark] studio 免费水印已开启 (check=studio_free_watermark.process): %s", c.Process)
	case c.Process != "" && !c.ValidProcess():
		logx.Errorf("[FreeWatermark] studio FreeWatermark.Process 含不允许的字符，已落库的水印视图将输出为空 (check=studio_free_watermark.process_charset)")
	}
}
