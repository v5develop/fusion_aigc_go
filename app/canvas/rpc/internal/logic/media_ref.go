package logic

import (
	"context"
	"strings"
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
// 绝不把 600 秒的签名地址落库；读出口（RunResult / 画布回填 / 上游候选引用）统一过 resolveMediaRefs。

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

// resolvedRuns 返回候选引用已换成现签地址的浅拷贝（不改原 run，不回写库）。
func (c *core) resolvedRuns(ctx context.Context, runs []*model.NodeRun) []*model.NodeRun {
	out := make([]*model.NodeRun, len(runs))
	for i, r := range runs {
		out[i] = c.resolvedRun(ctx, r)
	}
	return out
}

func (c *core) resolvedRun(ctx context.Context, r *model.NodeRun) *model.NodeRun {
	if r == nil || len(r.Candidates) == 0 || !strings.Contains(string(r.Candidates), "mr") {
		return r
	}
	list := r.CandidateList()
	var ptrs []*string
	for i := range list {
		ptrs = append(ptrs, &list[i].URL)
	}
	c.resolveMediaRefs(ctx, ptrs)
	cp := *r
	cp.Candidates = repo.MarshalCandidates(list)
	return &cp
}
