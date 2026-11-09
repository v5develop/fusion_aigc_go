package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/lease"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// mediaOutput 后端产物（待落素材库桶）。Stable=true 表示已是我方长期地址（studio 成品），不再转存。
type mediaOutput struct {
	RID, URL string
	W, H     int
	Stable   bool
}

// outcome 一次轮询的判定结果。
type outcome struct {
	done, ok bool
	outputs  []mediaOutput
	video    *mediaOutput
	cover    *mediaOutput
	text     string
	costCNY  float64
	actual   int    // 成功张数（count>1 部分成功时只结算这么多）
	code     string // 失败码
	msg      string // 失败原因（可展示）
	review   bool   // gateway UNKNOWN：不退款，待对账
}

// 失败码 → 文案。
var failText = map[string]string{
	"CONTENT_BLOCKED":  "内容未通过审核，请修改提示词或参考图",
	"INPUT_REJECTED":   "输入不被模型接受，请更换参考图或参数",
	"TIMEOUT":          "生成超时",
	"UPSTREAM_BUSY":    "模型繁忙，请稍后重试",
	"UPSTREAM_FAILED":  "生成失败，请重试",
	"UPSTREAM_UNKNOWN": "生成结果未知，点数待核对后处理",
	"CANCELED":         "已取消执行",
	"EMPTY_RESULT":     "没有生成结果，请重试",
}

func failureCode(f *aigateway.Failure) string {
	if f == nil {
		return "UPSTREAM_FAILED"
	}
	switch f.Class {
	case "CONTENT_POLICY":
		return "CONTENT_BLOCKED"
	case "INPUT_REJECTED":
		return "INPUT_REJECTED"
	case "DEADLINE_EXCEEDED":
		return "TIMEOUT"
	case "NO_CAPACITY", "NO_ROUTE":
		return "UPSTREAM_BUSY"
	}
	return "UPSTREAM_FAILED"
}

func fail(code string) outcome {
	return outcome{done: true, code: code, msg: failText[code]}
}

// ClassifyGateway 本 run 的全部 gateway 调用 → 判定。任一未终态 → 未完成；有成功产物 → 成功（actual=成功张数）；
// 全失败且含 UNKNOWN → 失败待对账（不退款）；否则失败（退款）。
func ClassifyGateway(kind model.RunKind, calls []*aigateway.Call) outcome {
	o := outcome{}
	var firstFail *aigateway.Failure
	unknown, cancelled := false, false
	for _, c := range calls {
		if c == nil || !c.IsTerminal() {
			return outcome{}
		}
		if c.Cost != nil {
			o.costCNY += costCNY(c.Cost)
		}
		switch c.Status {
		case aigateway.StatusSucceeded:
			switch kind {
			case model.KindVideo:
				if c.Result != nil && c.Result.Video != nil {
					v := c.Result.Video
					o.video = &mediaOutput{RID: v.Rid, URL: v.URL, W: v.Width, H: v.Height}
					if cv := c.Result.Cover; cv != nil {
						o.cover = &mediaOutput{RID: cv.Rid, URL: cv.URL, W: cv.Width, H: cv.Height}
					}
					o.actual++
				}
			case model.KindToolI2P:
				if c.Result != nil {
					var out struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					}
					_ = c.Result.Decode(&out)
					if t := strings.TrimSpace(out.Message.Content); t != "" {
						o.text = t
						o.actual++
					}
				}
			default:
				if c.Result != nil {
					for _, m := range c.Result.Media() {
						o.outputs = append(o.outputs, mediaOutput{RID: m.Rid, URL: m.URL, W: m.Width, H: m.Height})
						o.actual++
					}
				}
			}
		case aigateway.StatusUnknown:
			unknown = true
		case aigateway.StatusCancelled:
			cancelled = true
		default:
			if firstFail == nil {
				firstFail = c.Failure
			}
		}
	}
	o.done = true
	if o.actual > 0 {
		o.ok = true
		return o
	}
	switch {
	case unknown:
		r := fail("UPSTREAM_UNKNOWN")
		r.review, r.costCNY = true, o.costCNY
		return r
	case cancelled && firstFail == nil:
		return fail("CANCELED")
	case firstFail == nil:
		return fail("EMPTY_RESULT")
	}
	r := fail(failureCode(firstFail))
	r.costCNY = o.costCNY
	return r
}

func costCNY(c *aigateway.Cost) float64 {
	s := c.AmountCNY
	if s == "" && strings.EqualFold(c.Currency, "CNY") {
		s = c.Amount
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// ClassifyJava Java 任务 → 判定（WAITING_MANUAL_PROCESS 不算终态）。
func ClassifyJava(t *javatool.Task) outcome {
	if t == nil || !t.Terminal() {
		return outcome{}
	}
	switch t.Status {
	case javatool.StatusSucceed:
		if t.ResultURL == "" {
			return fail("EMPTY_RESULT")
		}
		return outcome{done: true, ok: true, actual: 1, outputs: []mediaOutput{{URL: t.ResultURL}}}
	case javatool.StatusTimeout:
		return fail("TIMEOUT")
	case javatool.StatusCancelled:
		return fail("CANCELED")
	}
	o := fail("UPSTREAM_FAILED")
	if t.ErrorMessage != "" {
		o.msg = t.ErrorMessage
	}
	return o
}

// ClassifyStudio studio 批次 → 判定：4 全部成功 / 6 部分成功 → 成功（候选 = 各模特成品）；5 失败；7 取消。
func ClassifyStudio(b *studio.BatchDetail) outcome {
	if b == nil || b.Batch == nil || b.Batch.Status < 4 {
		return outcome{}
	}
	switch b.Batch.Status {
	case 4, 6:
		o := outcome{done: true}
		for _, t := range b.Tasks {
			if t.Status == 2 && t.ResultUrl != "" {
				// 调用 studio 时带 RawRefs：免费档成品是 mrw:// 引用（长期），原样落库、读时现签；
				// 公有桶地址本来就长期可读。绝不把 600 秒签名地址当候选落库。
				o.outputs = append(o.outputs, mediaOutput{URL: t.ResultUrl, W: int(t.ResultWidth), H: int(t.ResultHeight), Stable: true})
			}
		}
		if len(o.outputs) == 0 {
			return fail("EMPTY_RESULT")
		}
		o.ok, o.actual = true, len(o.outputs)
		return o
	case 7:
		return fail("CANCELED")
	}
	o := fail("UPSTREAM_FAILED")
	if b.Batch.ErrorReason != "" {
		o.msg = b.Batch.ErrorReason
	}
	return o
}

// held 本 run 是否持有 Go 侧预留单。
func held(run *model.NodeRun) bool {
	return run.Backend == model.BackendGateway && run.BillingStatus == model.BillHeld
}

// failRun CAS from → 失败；gateway 后端退款（refund 且持有预留单）。返回是否抢到。
func (c *core) failRun(ctx context.Context, run *model.NodeRun, from int8, code, msg string, refund bool) bool {
	return c.endRun(ctx, run, from, model.RunFailed, code, msg, refund, false)
}

func (c *core) endRun(ctx context.Context, run *model.NodeRun, from, to int8, code, msg string, refund, review bool) bool {
	if msg == "" {
		msg = failText[code]
	}
	set := map[string]any{"error_code": code, "error_reason": msg}
	bill := run.BillingStatus
	switch {
	case review && run.Backend == model.BackendGateway:
		bill = model.BillReview
	case refund && held(run):
		bill = model.BillRefunded
		set["points"] = 0
	}
	set["billing_status"] = bill
	bctx, cancel := detached(ctx)
	ok, err := c.s.Repo.Run.Transit(bctx, run.ID, from, to, set)
	cancel()
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 置终态失败: %v", run.ID, err)
		return false
	}
	if !ok {
		return false
	}
	if bill == model.BillRefunded && run.BillingRequestID.Valid {
		bctx, cancel := detached(ctx)
		c.s.Billing.Fail(bctx, run.UserID, run.BillingRequestID.String, msg)
		cancel()
	}
	run.Status, run.BillingStatus = to, bill
	return true
}

// succeed 成功收尾：产物转存素材库桶 → 结算（gateway）→ CAS 成功 → 入素材库。ready 非空表示产物已转存（裁剪）。
func (c *core) succeed(ctx context.Context, run *model.NodeRun, o outcome, ready []model.Candidate) {
	log := logx.WithContext(ctx)
	cands := ready
	var videoURL string
	if ready == nil {
		var outs []mediaOutput
		if run.Kind == model.KindVideo {
			if o.cover != nil {
				outs = append(outs, *o.cover)
			}
		} else {
			outs = o.outputs
		}
		for i, m := range outs {
			if m.Stable {
				cands = append(cands, model.Candidate{RID: m.RID, URL: m.URL, W: m.W, H: m.H})
				continue
			}
			cand, err := c.persistOutput(ctx, run, i, m)
			if err != nil {
				if isTemporary(err) {
					log.Errorf("canvas: run %d 产物转存暂时失败，下轮重试: %v", run.ID, err)
					return
				}
				c.failRun(ctx, run, model.RunRunning, "UPSTREAM_FAILED", "产物转存失败", true)
				return
			}
			cands = append(cands, cand)
		}
		if o.video != nil {
			v, err := c.persistOutput(ctx, run, 99, *o.video)
			if err != nil {
				if isTemporary(err) {
					log.Errorf("canvas: run %d 视频转存暂时失败，下轮重试: %v", run.ID, err)
					return
				}
				c.failRun(ctx, run, model.RunRunning, "UPSTREAM_FAILED", "视频转存失败", true)
				return
			}
			videoURL = v.URL
		}
	}
	actual := o.actual
	if actual <= 0 {
		actual = max(1, len(cands))
	}
	set := map[string]any{"progress": 100, "candidates": string(repo.MarshalCandidates(cands))}
	if len(cands) == 0 {
		set["candidates"] = nil
	}
	if o.text != "" {
		set["result_text"] = o.text
	}
	if videoURL != "" {
		set["video_url"] = videoURL
	}
	if o.costCNY > 0 {
		set["cost_cny"] = fmt.Sprintf("%.6f", o.costCNY)
	}
	if held(run) {
		// billing_status 先保持 held：结算成功才写 settled，失败写 unsettled（见 settle）
		if req := max(1, int(run.RequestedCount)); actual < req {
			set["points"] = run.Points * int64(actual) / int64(req)
		}
	}
	ok, err := c.s.Repo.Run.Transit(ctx, run.ID, model.RunRunning, model.RunSucceeded, set)
	if err != nil {
		log.Errorf("canvas: run %d 置成功失败: %v", run.ID, err)
		return
	}
	if !ok {
		return // 已被取消 / 强制失败（取消时已退款）
	}
	run.Status = model.RunSucceeded
	if held(run) {
		c.settle(ctx, run, settleQuantity(run.Kind, actual))
	}
	c.insertAssets(ctx, run, cands, videoURL)
}

// settleQuantity 结算时交给 billing.Settle 的实际产出量。
// 计费的计量单位因 kind 而异：image 按张（Quote.Quantity = 张数，部分成功按成功张数结算）；
// video 按秒（Quote.Quantity = 秒数），成功就是整段视频，传成功个数（1）会被当成「只交付了 1 秒」只扣 1/秒数——
// 所以非 image 一律传 0（= 按冻结量全额确认）。
func settleQuantity(kind model.RunKind, actual int) int {
	if kind == model.KindImage {
		return actual
	}
	return 0
}

// insertAssets 成品入素材库（只有 Go 生成的：gateway 图 / 视频、裁剪；Java 与 studio 的成品由对方入库）。
func (c *core) insertAssets(ctx context.Context, run *model.NodeRun, cands []model.Candidate, videoURL string) {
	if !c.s.Config.Asset.Enabled || !run.TaskID.Valid {
		return
	}
	var entry, typ string
	switch run.Kind {
	case model.KindImage:
		entry, typ = c.s.Config.EntryCodes.Image, "image"
	case model.KindVideo:
		entry, typ = c.s.Config.EntryCodes.Video, "video"
	case model.KindToolCrop:
		entry, typ = c.s.Config.EntryCodes.Crop, "image"
	default:
		return
	}
	req := parseStored(run.Request)
	var related []string
	for _, r := range req.References {
		if r.AssetID != "" {
			related = append(related, r.AssetID)
		}
	}
	label := firstNonEmpty(req.Model, map[model.RunKind]string{model.KindToolCrop: "裁剪"}[run.Kind], string(run.Kind))
	name := fmt.Sprintf("%s · 画布 %s", label, c.now().Format("01-02 15:04"))
	props, _ := json.Marshal(map[string]any{
		"source": "generated",
		"provenance": map[string]any{
			"runId": run.RunID, "workflowId": run.CanvasID, "nodeId": run.NodeID, "model": req.Model,
			"modelGroup": run.ModelGroup, "requestId": run.ID,
			"params": map[string]any{"ratio": req.Ratio, "size": req.Size, "quality": req.Quality, "count": req.Count, "duration": req.Duration},
		},
		"relatedIds": related,
	})
	var list []asset.Asset
	if run.Kind == model.KindVideo {
		if videoURL == "" {
			return
		}
		cover := ""
		if len(cands) > 0 {
			cover = cands[0].URL
		}
		list = append(list, asset.Asset{UserID: run.UserID, SourceTaskID: run.TaskID.String, Type: typ, EntryCode: entry, Name: name,
			CoverURL: cover, ResultURL: videoURL, AspectRatio: req.Ratio, AIPrompt: trunc(req.Prompt, 2000), Properties: string(props)})
	} else {
		for _, cd := range cands {
			list = append(list, asset.Asset{UserID: run.UserID, SourceTaskID: run.TaskID.String, Type: typ, EntryCode: entry, Name: name,
				CoverURL: cd.URL, ResultURL: cd.URL, AspectRatio: req.Ratio, AIPrompt: trunc(req.Prompt, 2000), Properties: string(props)})
		}
	}
	if err := c.s.Asset.InsertSet(ctx, list); err != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 入素材库失败（成品已在画布）: %v", run.ID, err)
	}
}

// ---------------- 轮询器 ----------------

// Poller 后台推进器：每轮
//  1. 排队的 run：超时置失败退款；否则提交后端（RunNode 内联提交遇暂时性错误时留下的）。
//  2. 运行中的 run 按后端轮询：gateway BatchGet / Java GET /api/v1/tasks/{taskUid}（用缓存的用户 Bearer）/ studio GetBatch；
//     成功 → 转存 → 结算 → CAS 成功 → 入素材库；失败 / 取消 → 退款（gateway）；UNKNOWN 不退款标待对账；
//     超时 → gateway 主动 Cancel 后置失败退款；studio 取消批次后置失败；Java 调取消接口（由 Java 退款，无有效登录态时退化为停止跟踪）后置失败。
//  3. TaskCenter.Sync（image / video 两类进任务中心）。
//  4. 每分钟给排队 / 运行中、已冻结（held）的 gateway run 续期预留单（billing.Renew），排队久的不会过期被回收。
//
// 多实例：每个 run 一把行租约（pkg/lease，lease_owner / lease_until，TTL 3 分钟，后台每分钟批量续租）。
// 扫描只列没有租约或租约已过期的 run，逐个 Acquire，抢不到就跳过；RunNode 内联提交前也先抢，抢不到交给持有者。
// 进程崩溃不释放，租约到期后其它实例接管：有 backend_ref 的先查上游，没有的按幂等键重提，不会重复提交 / 扣费。
type Poller struct {
	c      *core
	cancel context.CancelFunc
	done   chan struct{}
}

func NewPoller(s *svc.ServiceContext) *Poller {
	return &Poller{c: newCore(s), done: make(chan struct{})}
}

func (p *Poller) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	defer close(p.done)
	interval := time.Duration(p.c.s.Config.Poll.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	logx.Info("canvas poller started")
	for {
		p.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Poller) Stop() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
}

// Tick 推进一轮（e2e / 单测可直接调用）。
func (p *Poller) Tick(ctx context.Context) {
	c := p.c
	c.refreshGroups(ctx)
	c.maintain(ctx)
	c.advanceSubmitting(ctx)
	c.advanceQueued(ctx)
	c.pollRunning(ctx)
	if err := c.s.TaskCenter.Sync(ctx); err != nil {
		logx.WithContext(ctx).Errorf("canvas: 任务中心同步失败: %v", err)
	}
}

func (c *core) expired(run *model.NodeRun) bool {
	_, timeout := c.budget(run.Kind)
	return c.now().Sub(run.CreatedAt) > timeout
}

// runLeaseTTL run 行租约时长：持有期间后台每 1/3 TTL 批量续租；进程崩溃后最多这么久由其它实例接管。
const runLeaseTTL = 3 * time.Minute

// leases 本轮用的租约集合（用完 Close，释放仍持有的）。
func (c *core) leases() *lease.Set { return c.s.Repo.Run.LeaseSet(lease.Owner(), runLeaseTTL) }

// acquire 抢 run 的租约；抢不到（别的实例在处理）或出错都返回 false，本轮跳过。
func acquire(ctx context.Context, ls *lease.Set, run *model.NodeRun) bool {
	ok, err := ls.Acquire(ctx, run.ID)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 抢租约失败: %v", run.ID, err)
	}
	return ok
}

func (c *core) advanceQueued(ctx context.Context) {
	list, err := c.s.Repo.Run.ListByStatus(ctx, model.RunQueued, c.s.Config.Poll.BatchSize)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 查排队 run 失败: %v", err)
		return
	}
	if len(list) == 0 {
		return
	}
	ls := c.leases()
	defer ls.Close()
	gone := c.goneCanvases(ctx, list)
	var mine []*model.NodeRun
	for _, run := range list {
		if !acquire(ctx, ls, run) {
			continue
		}
		mine = append(mine, run)
		c.advanceQueuedRun(ctx, run, gone)
		ls.Release(ctx, run.ID)
	}
	c.touch(ctx, mine, model.RunQueued)
}

// advanceQueuedRun 推进一个排队的 run（调用方已持有租约）。
func (c *core) advanceQueuedRun(ctx context.Context, run *model.NodeRun, gone map[string]bool) {
	if javaLive(run) {
		// Java 发过请求但 backend_ref 没写上：待取消 / 画布已删 / 排队超时都按幂等键重放找回任务号再取消（见 java_request.go）
		bearer := c.s.Bearers.Get(run.UserID)
		if c.retryPendingJavaStop(ctx, run, bearer) { // 待取消：按退避到点才重试
			return
		}
		if gone[run.CanvasID] {
			c.stopJava(ctx, run, stopGone, "画布已删除，执行已取消", bearer, 0)
			return
		}
		if c.expired(run) {
			c.stopJava(ctx, run, stopTimeout, "排队超时", bearer, 0)
			return
		}
	}
	if gone[run.CanvasID] {
		// 画布已删：不再提交，取消并退款（之前提交过但没写上 backend_ref 的调用一并回收）
		if c.endRun(ctx, run, model.RunQueued, model.RunCanceled, "CANCELED", "画布已删除，执行已取消", true, false) {
			c.reclaimQueuedCalls(ctx, run, "画布已删除")
		}
		return
	}
	if c.expired(run) {
		if c.failRun(ctx, run, model.RunQueued, "TIMEOUT", "排队超时，点数已退回", true) {
			c.reclaimQueuedCalls(ctx, run, "排队超时")
		}
		return
	}
	sp, err := c.specFromRun(ctx, run)
	if err != nil {
		c.failRun(ctx, run, model.RunQueued, "PARAM_INVALID", errMsg(err), true)
		return
	}
	bearer := c.s.Bearers.Get(run.UserID)
	if run.Backend == model.BackendJava && bearer == "" {
		return // 等前端下一次请求把 Bearer 带进来；不占队头（touch 排到队尾），超时由上面兜底
	}
	_ = c.dispatch(ctx, run, sp, bearer)
}

func errMsg(err error) string {
	if _, msg, ok := xerr.Parse(err); ok {
		return msg
	}
	return "参数不合法"
}

func (c *core) pollRunning(ctx context.Context) {
	list, err := c.s.Repo.Run.ListByStatus(ctx, model.RunRunning, c.s.Config.Poll.BatchSize)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 查运行中 run 失败: %v", err)
		return
	}
	if len(list) == 0 {
		return
	}
	ls := c.leases()
	defer ls.Close() // gateway 的 run 一起 batchGet，处理完由 Close 统一释放
	var gw, mine []*model.NodeRun
	// Java 工具所在画布已删：取消 Java 任务（不再交付到已删画布）
	var javaRuns []*model.NodeRun
	for _, run := range list {
		if run.Backend == model.BackendJava {
			javaRuns = append(javaRuns, run)
		}
	}
	var javaGone map[string]bool
	if len(javaRuns) > 0 {
		javaGone = c.goneCanvases(ctx, javaRuns)
	}
	for _, run := range list {
		if !acquire(ctx, ls, run) {
			continue
		}
		mine = append(mine, run)
		switch run.Backend {
		case model.BackendGateway:
			gw = append(gw, run)
			continue
		case model.BackendJava:
			c.pollJavaRun(ctx, run, javaGone[run.CanvasID])
		case model.BackendStudio:
			c.pollStudio(ctx, run)
		case model.BackendMedia:
			// 裁剪在 RunNode 内同步完成；停在运行中说明收尾时进程退出，超时置失败
			if c.expired(run) {
				c.failRun(ctx, run, model.RunRunning, "TIMEOUT", "", false)
			}
		}
		ls.Release(ctx, run.ID)
	}
	c.pollGateway(ctx, gw)
	c.touch(ctx, mine, model.RunRunning)
}

func callIDs(run *model.NodeRun) []string {
	var out []string
	for _, s := range strings.Split(run.BackendRef.String, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c *core) pollGateway(ctx context.Context, runs []*model.NodeRun) {
	if len(runs) == 0 || !c.s.Gateway.Enabled() {
		return
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, callIDs(r)...)
	}
	calls := map[string]*aigateway.Call{}
	for i := 0; i < len(ids); i += aigateway.MaxBatchGet {
		chunk := ids[i:min(len(ids), i+aigateway.MaxBatchGet)]
		got, err := c.s.Gateway.BatchGet(ctx, chunk)
		if err != nil {
			// 不整轮跳过：查不到的调用按未完成处理，下面照样做超时判断与取消（gateway 长时间不可用时冻结不会悬空到被回收）
			logx.WithContext(ctx).Errorf("canvas: gateway batchGet 失败（本批按未完成处理，仍做超时判断）: %v", err)
			continue
		}
		for j := range got {
			calls[got[j].ID] = &got[j]
		}
	}
	for _, run := range runs {
		var cs []*aigateway.Call
		for _, id := range callIDs(run) {
			cs = append(cs, calls[id])
		}
		o := ClassifyGateway(run.Kind, cs)
		if !o.done && c.expired(run) {
			if c.failRun(ctx, run, model.RunRunning, "TIMEOUT", "生成超时，点数已退回", true) {
				c.cancelCalls(ctx, run.ID, callIDs(run), "生成超时")
			}
			continue
		}
		c.apply(ctx, run, o)
	}
}

// apply 按判定收尾。
func (c *core) apply(ctx context.Context, run *model.NodeRun, o outcome) {
	if !o.done {
		return
	}
	if o.ok {
		c.succeed(ctx, run, o, nil)
		return
	}
	if o.costCNY > 0 {
		_ = c.s.Repo.Run.Update(ctx, run.ID, map[string]any{"cost_cny": fmt.Sprintf("%.6f", o.costCNY)})
	}
	if o.code == "CANCELED" {
		c.endRun(ctx, run, model.RunRunning, model.RunCanceled, o.code, o.msg, true, false)
		return
	}
	c.endRun(ctx, run, model.RunRunning, model.RunFailed, o.code, o.msg, !o.review, o.review)
}

func (c *core) pollJava(ctx context.Context, run *model.NodeRun) { c.pollJavaRun(ctx, run, false) }

// pollJavaRun 推进一个运行中的 Java run（用缓存的用户 Bearer）：待取消的先重试取消；画布已删 / 超时 → 调 Java 取消接口
// （拿不到有效登录态时退化为停止跟踪，见 java_cancel.go）；否则查任务、终态收尾。
func (c *core) pollJavaRun(ctx context.Context, run *model.NodeRun, gone bool) {
	bearer := c.s.Bearers.Get(run.UserID)
	if c.retryPendingJavaStop(ctx, run, bearer) { // 待取消：按退避到点才重试
		return
	}
	if gone {
		c.stopJava(ctx, run, stopGone, "画布已删除，执行已取消", bearer, 0)
		return
	}
	if bearer != "" && run.BackendRef.Valid {
		t, err := c.s.Java.GetTask(ctx, bearer, run.BackendRef.String)
		var je *javatool.Error
		switch {
		case err == nil:
			o := ClassifyJava(t)
			if o.done {
				c.apply(ctx, run, o)
				return
			}
		case errors.As(err, &je) && je.Unauthorized():
			// 令牌已失效：丢掉缓存，等前端下一次请求带新的进来（不再拿旧令牌反复打 Java）。
			// 只删本次用的那个：期间用户请求可能已补进新令牌，不能误删
			if c.s.Bearers.Get(run.UserID) == bearer {
				c.s.Bearers.Delete(run.UserID)
			}
			bearer = ""
			logx.WithContext(ctx).Infof("canvas: 查 Java 任务 %s（run %d）401，已清除缓存的登录令牌", run.BackendRef.String, run.ID)
		default:
			logx.WithContext(ctx).Infof("canvas: 查 Java 任务 %s（run %d）失败: %v", run.BackendRef.String, run.ID, err)
		}
	}
	if c.expired(run) {
		c.stopJava(ctx, run, stopTimeout, "工具执行超时", bearer, 0)
	}
}

func (c *core) pollStudio(ctx context.Context, run *model.NodeRun) {
	if c.s.Studio == nil {
		return
	}
	bid, _ := strconv.ParseInt(run.BackendRef.String, 10, 64)
	b, err := c.s.Studio.GetBatch(ctx, &studio.GetBatchReq{BatchId: bid, UserId: run.UserID, RawRefs: true})
	if err == nil {
		if o := ClassifyStudio(b); o.done {
			c.apply(ctx, run, o)
			return
		}
	} else {
		logx.WithContext(ctx).Infof("canvas: 查 studio 批次 %d（run %d）失败: %v", bid, run.ID, err)
	}
	if c.expired(run) {
		if c.failRun(ctx, run, model.RunRunning, "TIMEOUT", "试穿超时，已取消", false) {
			c.cancelStudio(ctx, run, run.BackendRef.String, "试穿超时")
		}
	}
}

// billingStatusOf 冻结后的计费镜像。
func billingStatusOf(mode string, r *billing.Reservation) string {
	switch {
	case mode == billing.ModeShadow:
		return model.BillShadow
	case mode == billing.ModeOn && r != nil:
		return model.BillHeld
	}
	return model.BillNone
}
