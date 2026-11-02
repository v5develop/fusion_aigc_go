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

// refURLTTL 现签地址有效期：与 mediasvc.ResourceURLs 的默认值（defaultURLTTL = 1h）一致，
// 列表 / 详情每次读都重新签，1 小时足够前端渲染与短时缓存。
const refURLTTL = time.Hour

// resourceSigner ResourceURLs 的最小接口（*mediasvc.Client 实现；测试用假实现）。
type resourceSigner interface {
	ResourceURLs(ctx context.Context, rids []string, ttl time.Duration) (map[string]string, error)
}

// signerOf 避免 nil *mediasvc.Client 装进接口后变成非 nil。
func signerOf(s *svc.ServiceContext) resourceSigner {
	if s == nil || s.Media == nil {
		return nil
	}
	return s.Media
}

// ridOf mr://{rid}[.ext] → rid（与 Java ridOf 同口径）；不是资源引用返回 ""。
func ridOf(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, mrPrefix) {
		return ""
	}
	rid := strings.TrimPrefix(v, mrPrefix)
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
func resolveRefs(ctx context.Context, signer resourceSigner, rows []*repo.AssetRow) {
	fieldsOf := func(r *repo.AssetRow) []*sql.NullString {
		return []*sql.NullString{&r.CoverURL, &r.ResultURL, &r.CurrentImage}
	}
	var rids []string
	seen := map[string]bool{}
	for _, r := range rows {
		if r == nil {
			continue
		}
		for _, f := range fieldsOf(r) {
			if rid := ridOf(f.String); rid != "" && mediasvc.ValidRID(rid) && !seen[rid] {
				seen[rid] = true
				rids = append(rids, rid)
			}
		}
	}
	urls := signAll(ctx, signer, rids)
	for _, r := range rows {
		if r == nil {
			continue
		}
		for _, f := range fieldsOf(r) {
			if !strings.HasPrefix(strings.TrimSpace(f.String), mrPrefix) {
				continue
			}
			rid := ridOf(f.String)
			if u := urls[rid]; u != "" {
				*f = sql.NullString{String: u, Valid: true}
				continue
			}
			logx.WithContext(ctx).Infof("[warn] assetlib: 资源引用无法现签，已置空 asset=%d rid=%s", r.ID, ridHint(rid))
			*f = sql.NullString{}
		}
	}
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
