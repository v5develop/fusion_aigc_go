package logic

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aigc-local-life/pkg/aigateway"

	"github.com/zeromicro/go-zero/core/logx"
)

// 参考图预上传：画布每次落库（保存 / 新建 / 智能体改图）后，把「gateway 图片节点」上接入的参考图异步交给网关
// POST /v1/media:prewarm。网关按与提交完全相同的解析与缓存键，提前把图传到供应商（Atlas uploadMedia 实测约 1.4 秒），
// 用户点「生成」时提交直接命中上传缓存。
//
//   - 只为提速：fire-and-forget，不阻塞保存（落库后另起协程、短超时、失败只记日志）；网关没处理也只是提交时照常上传。
//   - 与派发同形：参考图同样经 mediaRef 拉入媒体服务成 mr://{rid}+sha256（派发时再拉入按内容幂等），网关看到的输入与
//     提交时一致，缓存键（按实发字节 sha256）才对得上。
//   - 只看「图片素材节点（type=image，http(s) 地址）→ 图片模型节点的图片口」：模型产物作参考时快照里已剥离、要按 run 现签，
//     这里不处理（派发时照常上传）。
//   - 去重：进程内按（模型组, 地址）与（模型组, sha256）在 Prewarm.DedupeSeconds 内只发一次；失败的条目放回，下次保存再试。
//   - 网关旧版（404）：本进程关闭此功能，打一次 INFO（check=canvas.prewarm_unsupported）。

// prewarmer 进程级状态（core 按请求新建，状态不能放在 core 上）。
type prewarmer struct {
	mu      sync.Mutex
	seen    map[string]time.Time // key → 过期时刻
	off     atomic.Bool          // 网关不支持（404）后关闭
	running atomic.Int32
	wg      sync.WaitGroup // 测试等后台任务结束用
}

var refPrewarm = &prewarmer{seen: map[string]time.Time{}}

// claim 窗口内第一次见到 key 返回 true 并占住；顺手清理过期项。
func (p *prewarmer) claim(key string, now time.Time, ttl time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if exp, ok := p.seen[key]; ok && now.Before(exp) {
		return false
	}
	if len(p.seen) > 4096 {
		for k, exp := range p.seen {
			if !now.Before(exp) {
				delete(p.seen, k)
			}
		}
	}
	p.seen[key] = now.Add(ttl)
	return true
}

func (p *prewarmer) forget(keys ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range keys {
		delete(p.seen, k)
	}
}

// prewarmJob 一个模型组下要预上传的参考图地址（去重、按快照顺序）。
type prewarmJob struct {
	group string
	urls  []string
}

// prewarmJobs 快照里「图片素材节点 → gateway 图片模型节点图片口」的参考图，按模型组归并。
func (c *core) prewarmJobs(d *Doc) []prewarmJob {
	nodes := d.NodeByID()
	var order []string
	byGroup := map[string][]string{}
	seen := map[string]bool{}
	for _, e := range d.Edges {
		if NormalizeHandle(e.TargetHandle) != HandleImage {
			continue
		}
		src, dst := nodes[e.Source], nodes[e.Target]
		if src == nil || dst == nil || src.Type != "image" || dst.Type != "model" || isVideoModel(dst) {
			continue
		}
		u := strings.TrimSpace(strData(src.Data, "image"))
		if !isHTTP(u) {
			continue
		}
		m, ok := c.s.Config.FindModel(strData(dst.Data, "model"), strData(dst.Data, "modelId"), "image")
		if !ok || m.ModelGroup == "" {
			continue
		}
		if seen[m.ModelGroup+"|"+u] {
			continue
		}
		seen[m.ModelGroup+"|"+u] = true
		if _, ok := byGroup[m.ModelGroup]; !ok {
			order = append(order, m.ModelGroup)
		}
		byGroup[m.ModelGroup] = append(byGroup[m.ModelGroup], u)
	}
	out := make([]prewarmJob, 0, len(order))
	for _, g := range order {
		out = append(out, prewarmJob{group: g, urls: byGroup[g]})
	}
	return out
}

// schedulePrewarm 落库后调用：立即返回，预上传在后台协程里做（并发满了直接丢弃）。
func (c *core) schedulePrewarm(canvasID string, snapshot []byte) {
	cfg := c.s.Config.Prewarm
	if !cfg.Enabled || refPrewarm.off.Load() || c.s.Gateway == nil || !c.s.Gateway.Enabled() || len(snapshot) == 0 {
		return
	}
	limit := int32(max(1, cfg.MaxConcurrent))
	if refPrewarm.running.Add(1) > limit {
		refPrewarm.running.Add(-1)
		return
	}
	timeout := time.Duration(max(1, cfg.TimeoutSeconds)) * time.Second
	refPrewarm.wg.Add(1)
	go func() {
		defer refPrewarm.wg.Done()
		defer refPrewarm.running.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c.prewarmSnapshot(ctx, canvasID, snapshot)
	}()
}

// prewarmSnapshot 解析快照、去重、拉入媒体服务、调网关。任何失败只记日志。
func (c *core) prewarmSnapshot(ctx context.Context, canvasID string, snapshot []byte) {
	defer func() {
		if r := recover(); r != nil {
			logx.WithContext(ctx).Errorf("canvas: 参考图预上传异常 canvas=%s（check=canvas.prewarm_panic）: %v", canvasID, r)
		}
	}()
	d, err := ParseSnapshot(snapshot, 0)
	if err != nil {
		return
	}
	ttl := time.Duration(max(1, c.s.Config.Prewarm.DedupeSeconds)) * time.Second
	for _, job := range c.prewarmJobs(d) {
		if refPrewarm.off.Load() || ctx.Err() != nil {
			return
		}
		c.prewarmGroup(ctx, canvasID, job, ttl)
	}
}

func (c *core) prewarmGroup(ctx context.Context, canvasID string, job prewarmJob, ttl time.Duration) {
	log := logx.WithContext(ctx)
	var refs []aigateway.MediaRef
	var keys [][]string // 与 refs 一一对应：这张图占住的去重键（失败时只放回没送达的那些）
	for _, u := range job.urls {
		k := job.group + "|u|" + u
		if !refPrewarm.claim(k, c.now(), ttl) {
			continue
		}
		ref, err := c.mediaRef(ctx, u)
		if err != nil {
			refPrewarm.forget(k)
			log.Infof("canvas: 参考图预上传跳过（拉入媒体服务失败）canvas=%s group=%s（check=canvas.prewarm_ingest）: %v", canvasID, job.group, err)
			continue
		}
		if ref.Sha256 != "" && !refPrewarm.claim(job.group+"|s|"+ref.Sha256, c.now(), ttl) {
			continue // 同一份字节换了个地址：已预上传过
		}
		held := []string{k}
		if ref.Sha256 != "" {
			held = append(held, job.group+"|s|"+ref.Sha256)
		}
		keys = append(keys, held)
		refs = append(refs, ref)
	}
	for start := 0; start < len(refs); start += aigateway.MaxPrewarmInputs {
		chunk := refs[start:min(len(refs), start+aigateway.MaxPrewarmInputs)]
		items, err := c.s.Gateway.Prewarm(ctx, aigateway.PrewarmRequest{ModelGroup: job.group, Operation: opImageEdit, Inputs: chunk})
		if err != nil {
			// 只放回这一批及其后还没发的批次的键；前面已送达网关的批次保持去重，下次保存不重复预上传
			var unsent []string
			for _, held := range keys[start:] {
				unsent = append(unsent, held...)
			}
			refPrewarm.forget(unsent...)
			if errors.Is(err, aigateway.ErrNotFound) {
				if refPrewarm.off.CompareAndSwap(false, true) {
					log.Infof("canvas: 网关没有参考图预上传接口（旧版），本进程关闭预上传（check=canvas.prewarm_unsupported）")
				}
				return
			}
			log.Infof("canvas: 参考图预上传失败（不影响生成）canvas=%s group=%s n=%d（check=canvas.prewarm_failed）: %v", canvasID, job.group, len(chunk), err)
			return
		}
		queued, cached, skipped := 0, 0, 0
		for _, it := range items {
			switch it.Status {
			case aigateway.PrewarmQueued:
				queued++
			case aigateway.PrewarmCached:
				cached++
			default:
				skipped++
			}
		}
		log.Infof("canvas: 参考图预上传已交网关 canvas=%s group=%s queued=%d cached=%d skipped=%d（check=canvas.prewarm_sent）",
			canvasID, job.group, queued, cached, skipped)
	}
}
