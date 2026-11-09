package logic

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/app/assetlib/rpc/internal/svc"
	"aigc-local-life/pkg/mediasvc"

	"github.com/zeromicro/go-zero/core/logx"
)

// mrPrefix 资源引用前缀：Java 把封面等存成 mr://{rid}（CreationTaskViewAssembler 同口径），读时现签。
const mrPrefix = "mr://"

// mrwPrefix 免费档水印视图引用（Java MediaRefs.WATERMARK_PREFIX）：与 mr://{rid} 指向同一个私有原件，
// 但只能签成带 x-oss-process 水印参数的地址（参数在签名内，去掉即 403）；签不出就置空，绝不签干净地址。
const mrwPrefix = "mrw://"

// refURLTTL 现签地址有效期：与 mediasvc.ResourceURLs 的默认值（defaultURLTTL = 1h）一致，
// 列表 / 详情每次读都重新签，1 小时足够前端渲染与短时缓存。
const refURLTTL = time.Hour

// resourceSigner ResourceURLs 的最小接口（*mediasvc.Client 实现；测试用假实现）。
type resourceSigner interface {
	ResourceURLs(ctx context.Context, rids []string, ttl time.Duration) (map[string]string, error)
}

// processSigner 带处理参数的现签（*mediasvc.Client 实现）；签名器不支持时水印视图一律置空。
type processSigner interface {
	ResourceURLsWithProcess(ctx context.Context, rids []string, ttl time.Duration, process string) (map[string]string, error)
}

// signerOf 避免 nil *mediasvc.Client 装进接口后变成非 nil。
func signerOf(s *svc.ServiceContext) resourceSigner {
	if s == nil || s.Media == nil {
		return nil
	}
	return s.Media
}

// ridOf mr://{rid}[.ext] → rid（与 Java ridOf 同口径）；不是资源引用返回 ""。水印视图 mrw:// 也返回 ""，
// 由 watermarkRidOf 单独处理，免得被当成普通引用签出干净地址。
func ridOf(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, mrPrefix) {
		return ""
	}
	return ridOfBody(strings.TrimPrefix(v, mrPrefix))
}

// watermarkRidOf mrw://{rid}[.ext] → rid；不是水印视图返回 ""。
func watermarkRidOf(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, mrwPrefix) {
		return ""
	}
	return ridOfBody(strings.TrimPrefix(v, mrwPrefix))
}

func ridOfBody(rid string) string {
	if i := strings.LastIndexByte(rid, '.'); i > 0 {
		rid = rid[:i]
	}
	return rid
}

// ridHint 日志里只出 rid 前缀，不出完整引用 / 地址。
func ridHint(rid string) string {
	if len(rid) > 12 {
		return rid[:12] + "…"
	}
	return rid
}

// resolveRefs 把一页资产行里的 mr://{rid}（cover_url / result_url / 当前版本图）就地替换成现签地址。
// 整页只调一次 ResourceURLs；解析失败的字段置空，绝不把原始 mr:// 交给前端。
// 不带水印处理串：水印视图 mrw:// 一律置空（见 resolveRefsWith）。
func resolveRefs(ctx context.Context, signer resourceSigner, rows []*repo.AssetRow) {
	resolveRefsWith(ctx, signer, "", rows)
}

// resolveRefsWith 同 resolveRefs，另把免费档水印视图 mrw://{rid} 签成带 process 水印参数的地址
// （与观看者无关：水印视图只出现在免费档交付的资产行上，付费转存后 Java 会把它换成公有原图地址）。
// process 为空或签名器不支持处理参数时，水印视图置空 —— 宁可不出图，也不签干净原件。
func resolveRefsWith(ctx context.Context, signer resourceSigner, process string, rows []*repo.AssetRow) {
	fieldsOf := func(r *repo.AssetRow) []*sql.NullString {
		return []*sql.NullString{&r.CoverURL, &r.ResultURL, &r.CurrentImage}
	}
	var rids, wmRids []string
	seen, wmSeen := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		if r == nil {
			continue
		}
		for _, f := range fieldsOf(r) {
			if rid := ridOf(f.String); rid != "" && mediasvc.ValidRID(rid) && !seen[rid] {
				seen[rid] = true
				rids = append(rids, rid)
			}
			if rid := watermarkRidOf(f.String); rid != "" && mediasvc.ValidRID(rid) && !wmSeen[rid] {
				wmSeen[rid] = true
				wmRids = append(wmRids, rid)
			}
		}
	}
	urls := signAll(ctx, signer, rids)
	wmURLs := signWatermarked(ctx, signer, process, wmRids)
	for _, r := range rows {
		if r == nil {
			continue
		}
		for _, f := range fieldsOf(r) {
			v := strings.TrimSpace(f.String)
			var rid, u string
			switch {
			case strings.HasPrefix(v, mrPrefix):
				rid = ridOf(v)
				u = urls[rid]
			case strings.HasPrefix(v, mrwPrefix):
				rid = watermarkRidOf(v)
				u = wmURLs[rid]
			default:
				continue
			}
			if u != "" {
				*f = sql.NullString{String: u, Valid: true}
				continue
			}
			logx.WithContext(ctx).Infof("[warn] assetlib: 资源引用无法现签，已置空 asset=%d rid=%s", r.ID, ridHint(rid))
			*f = sql.NullString{}
		}
	}
}

// signWatermarked 水印视图逐个带处理参数现签（失败的那个为空）；没有处理串或签名器不支持时整批为空。
func signWatermarked(ctx context.Context, signer resourceSigner, process string, rids []string) map[string]string {
	if len(rids) == 0 {
		return nil
	}
	ps, ok := signer.(processSigner)
	if process == "" || signer == nil || !ok {
		logx.WithContext(ctx).Infof("[warn] assetlib: 未配置免费水印处理串（FreeWatermark.Process）或媒体服务，%d 个水印视图输出为空", len(rids))
		return nil
	}
	out := make(map[string]string, len(rids))
	for _, rid := range rids {
		if ctx.Err() != nil {
			break
		}
		one, err := ps.ResourceURLsWithProcess(ctx, []string{rid}, refURLTTL, process)
		if err != nil {
			logx.WithContext(ctx).Infof("[warn] assetlib: 水印视图现签失败 rid=%s: %v", ridHint(rid), err)
			continue
		}
		out[rid] = one[rid]
	}
	return out
}

// signAll 一次签整批；整批因个别 rid 的永久错误（4xx，如对象不存在）失败时逐个重签，
// 只让坏的那几个为空。暂时性错误（5xx / 网络）不逐个重试，整批置空。
func signAll(ctx context.Context, signer resourceSigner, rids []string) map[string]string {
	if len(rids) == 0 {
		return nil
	}
	if signer == nil {
		logx.WithContext(ctx).Infof("[warn] assetlib: 未配置媒体服务，%d 个资源引用输出为空", len(rids))
		return nil
	}
	urls, err := signer.ResourceURLs(ctx, rids, refURLTTL)
	if err == nil {
		return urls
	}
	var me *mediasvc.Error
	if len(rids) == 1 || !errors.As(err, &me) || !me.Permanent() {
		logx.WithContext(ctx).Infof("[warn] assetlib: 资源引用现签失败（%d 个，首个 rid=%s）: %v", len(rids), ridHint(rids[0]), err)
		return nil
	}
	out := make(map[string]string, len(rids))
	for _, rid := range rids {
		if ctx.Err() != nil {
			break
		}
		one, err := signer.ResourceURLs(ctx, []string{rid}, refURLTTL)
		if err != nil {
			logx.WithContext(ctx).Infof("[warn] assetlib: 资源引用现签失败 rid=%s: %v", ridHint(rid), err)
			continue
		}
		out[rid] = one[rid]
	}
	return out
}
