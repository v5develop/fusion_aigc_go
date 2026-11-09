package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/lease"
	"aigc-local-life/pkg/semaphore"

	"github.com/zeromicro/go-zero/core/logx"
)

// Poller studio 的后台推进器。状态全部在库里，进程重启后从库里续跑（所有迁移都是 CAS）。
//
// 每轮（Poll.IntervalSeconds，默认 5s）：
//
//  1. advanceBatches：排队 / 切图中 / 反推中 的批次 → 后台编排（flow_batch.go），同时最多 Poll.BatchConcurrency 个；
//     商拍 下载规整 → crop-plan → 裁图 → crop-check → 组图 → analyze-batch；姿态 下载规整 → pose source → 每姿势 pose prompt；
//     图片生成 参考图入库 → （可选）chat.completion 润色。结束时每个子任务写好 prompt_text / input_images，批次进 生成中。
//  2. submitGenerating：生成中 的批次里排队的子任务按每批 Poll.Concurrency（默认 3）个名额 CAS 排队→生成中。
//  3. pollRunning：生成中 的子任务（同时最多 Poll.Workers 个）：没有 gateway_call_id 的提交（幂等键保证不重复扣费）；
//     有的 GET ?wait= 长轮询，未超时就 Billing.Renew；终态按 flow_task.go 处理（转存 → 结算 → CAS 成功 → 入素材库；失败退款）。
//     交付中 的子任务（转存后、结算前进程退出）续做交付。
//  4. 每个子任务到终态后 converge 收敛批次（全部成功 / 全部失败 / 部分成功 / 已取消），并兜底扫一遍 生成中 的批次。
//  5. maintain（每 maintainEvery 一次，flow_guard.go）：未终态批次下已冻结的子任务批量续期；提交中超过 5 分钟的批次退款置失败；
//     已终态却还有排队子任务的批次拉回；终态任务仍是「已冻结」的计费镜像按预留单真实状态回写。
//  6. TaskCenter.Sync。
//
// 多实例：批次（编排 / 派发）与子任务（提交 / 轮询 / 交付）各有一把行租约（pkg/lease，lease_owner / lease_until）。
// 扫描只列没有租约或租约已过期的行，逐个 Acquire，抢不到就跳过；持有期间后台心跳续租（编排 10 分钟、轮询 3 分钟，每 1/3 TTL 一次），
// 明确丢失租约时取消本次推进。进程崩溃不释放，租约到期后由其它实例接管：编排从头重放（媒体服务幂等键 + 批次缓存），
// 子任务先按 gateway_call_id 查上游、没有才按幂等键提交，都不会重复扣费。进程内的 inflight 只做本地去重，不再是正确性依赖。
type Poller struct {
	f      *flow
	svcCtx *svc.ServiceContext
	cancel context.CancelFunc
	done   chan struct{}
}

func NewPoller(svcCtx *svc.ServiceContext) *Poller {
	logFreeWatermark(svcCtx.Config.FreeWatermark)
	return &Poller{f: newFlow(svcCtx), svcCtx: svcCtx, done: make(chan struct{})}
}

func (p *Poller) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	defer close(p.done)
	p.f.catalogMaxRefs(ctx, "") // 启动时预拉模型组目录（失败不影响启动，用兜底上限）

	interval := time.Duration(p.svcCtx.Config.Poll.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	logx.Info("studio poller started")
	for {
		p.f.tick(ctx)
		select {
		case <-ctx.Done():
			p.f.wg.Wait()
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

// ---------------------------------------------------------------- 依赖

// flowBiller Poller 用到的计费调用；*billing.Service 满足。
type flowBiller interface {
	biller
	Renew(ctx context.Context, userID int64, requestID string) error
	Settle(ctx context.Context, userID int64, requestID string, actual int) error
	Status(ctx context.Context, userID int64, requestID string) (string, error)
	// Enough 只读余额预检（报价 vs 余额，不冻结；非 on 模式恒通过）：用户重试占轮次前先拦明显不够的
	Enough(ctx context.Context, c billing.Charge) error
	RefundConfirmed(ctx context.Context, userID int64, requestID, reason string) (int64, error)
}

// flow 编排与轮询共用的依赖与在途标记。Poller 与后台接口（重做 / 强制失败）各自 newFlow，共用库里的状态。
type flow struct {
	cfg   config.Config
	repo  *repo.Repo
	media pipeline.Media   // 未配置媒体服务时为 nil
	rev   pipeline.Reverse // sidecar
	gw    pipeline.Gateway
	gwOn  bool
	bill  flowBiller
	asset func(ctx context.Context, list []asset.Asset) error
	sync  func(ctx context.Context)
	now   func() time.Time

	// catalog 拉 gateway 模型组（feature_limits），缓存 catalogTTL；nil 表示不可用
	catalog  func(ctx context.Context) ([]aigateway.ModelGroup, error)
	catMu    sync.Mutex
	catAt    time.Time
	catLimit map[string]int // 启用的模型组 → max_reference_images

	// mask 区域蒙版服务（成品服装蒙版的异步后置步骤）；nil = 不补蒙版
	mask *masker

	// promptPct 商拍提示词灰度比例（afv_system_config 覆盖 yaml）；nil 时直接用 cfg.TryonPrompt.NewPercent
	promptPct *intKnob

	sem      *semaphore.Group
	owner    string   // 行租约持有者（pkg/lease.Owner，进程内共用）
	inflight sync.Map // 本地去重："b:<id>" 编排中、"t:<id>" 轮询中、"s:<id>" 派发中（跨实例互斥靠行租约）
	wg       sync.WaitGroup

	maintAt time.Time // 上次 maintain 的时间（零值 = 下一轮立刻做）

	previewCleanAt time.Time // 上次清理配对预览缓存的时间（零值 = 下一轮立刻做）
	renewAfter     int64     // 续期游标：上一轮续到的子任务主键（超过 renewLimit 时分轮轮转，0 = 从头）

	// tier 免费档判定（免费水印视图交付用）；nil = 不区分（全部按付费交付）
	tier freeTier
	// promoteAt / promoteAfter 免费原图付费转存：上次扫描时间、扫描游标（主键）
	promoteAt    time.Time
	promoteAfter int64
	// cleanAt / cleanAfter 到期清理：上次扫描时间、扫描游标；dropAssets 逻辑删除该任务的素材库行
	cleanAt    time.Time
	cleanAfter int64
	dropAssets func(ctx context.Context, userID int64, sourceTaskID string) error
}

// semaphore.Group 的 key：批次编排、子任务轮询各一个全局名额池。
const (
	semBatch int64 = -1
	semPoll  int64 = -2
)

// 行租约时长。持有期间每 1/3 TTL 心跳续租一次；进程崩溃后最多这么久由其它实例接管。
const (
	// leaseOrchestrate 批次编排（切图 + 反推可能 5 分钟以上，靠心跳续，不必覆盖整段）。
	leaseOrchestrate = 10 * time.Minute
	// leasePoll 子任务提交 / 长轮询 / 交付，以及 生成中 批次的派发。
	leasePoll = 3 * time.Minute
)

func newFlow(s *svc.ServiceContext) *flow {
	f := &flow{
		cfg: s.Config, repo: s.Repo, rev: s.Reverse, bill: s.Billing,
		sem: semaphore.NewGroup(), now: time.Now, owner: lease.Owner(),
		asset: func(ctx context.Context, list []asset.Asset) error { return s.Asset.InsertSet(ctx, list) },
		dropAssets: func(ctx context.Context, userID int64, sourceTaskID string) error {
			return s.Asset.SoftDeleteByTask(ctx, userID, sourceTaskID)
		},
		sync: func(ctx context.Context) {
			if s.TaskCenter != nil {
				_ = s.TaskCenter.Sync(ctx) // 失败已在 taskcenter 里记日志
			}
		},
	}
	if s.MediaSvc != nil { // 别把 nil 指针装进接口
		f.media = s.MediaSvc
	}
	if s.Repo != nil {
		f.mask = newMasker(s)
	}
	if s.Tier != nil { // 别把 nil 指针装进接口
		f.tier = s.Tier
	}
	if s.Conn != nil {
		f.promptPct = newIntKnob(KnobTryonNewPromptPercent, s.Config.TryonPrompt.NewPercent, 0, 100, systemConfigLoader(s.Conn))
	}
	if s.Gateway != nil {
		f.gw, f.gwOn = s.Gateway, s.Gateway.Enabled()
		if f.gwOn {
			f.catalog = s.Gateway.ModelGroups
		}
	}
	return f
}

// newAdminFlow 后台接口用：不同步任务中心（全量同步要 1–2s，会顶到 admin-api 调 rpc 的 2s 超时），交给 Poller 下一轮。
func newAdminFlow(s *svc.ServiceContext) *flow {
	f := newFlow(s)
	f.sync = func(context.Context) {}
	return f
}

func (f *flow) claim(key string) bool {
	_, loaded := f.inflight.LoadOrStore(key, struct{}{})
	return !loaded
}

func (f *flow) release(key string) { f.inflight.Delete(key) }

// goClaimed 抢到 key 才起协程；协程结束释放。
func (f *flow) goClaimed(key string, fn func()) {
	if !f.claim(key) {
		return
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer f.release(key)
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("studio: %s panic: %v", key, r)
			}
		}()
		fn()
	}()
}

func (f *flow) batchSize() int {
	if n := f.cfg.Poll.BatchSize; n > 0 {
		return n
	}
	return 100
}

func positive(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func secs(v int, def time.Duration) time.Duration {
	if v > 0 {
		return time.Duration(v) * time.Second
	}
	return def
}

func (f *flow) cropTimeout() time.Duration { return secs(f.cfg.Timeouts.Crop, pipeline.CropTimeout) }

// tryonNewPromptPercent 商拍反推走 prompt_version=new 的灰度比例（0–100，只对 Seedream 生效）。
func (f *flow) tryonNewPromptPercent(ctx context.Context) int {
	if f.promptPct != nil {
		return f.promptPct.Get(ctx)
	}
	return f.cfg.TryonPrompt.NewPercent
}

func (f *flow) analyzeTimeout() time.Duration {
	return secs(f.cfg.Timeouts.Analyze, pipeline.AnalyzeTimeout)
}
func (f *flow) generateTimeout() time.Duration {
	return secs(f.cfg.Timeouts.Generate, pipeline.GenerateTimeout)
}
func (f *flow) deliverGrace() time.Duration {
	return secs(f.cfg.Timeouts.DeliverGrace, defaultDeliverGrace)
}
func (f *flow) longPollWait() time.Duration {
	return min(secs(f.cfg.AiGateway.LongPollWait, 20*time.Second), 25*time.Second)
}

// catalogTTL 模型组目录缓存时长。
const catalogTTL = 10 * time.Minute

// catalogMaxRefs 模型组的参考图上限：取 catalog feature_limits.max_reference_images（过滤 enabled=false），
// 取不到返回 0（调用方用 pipeline 的硬编码兜底）。目录缓存 10 分钟；拉取失败时 1 分钟后再试。
func (f *flow) catalogMaxRefs(ctx context.Context, group string) int {
	if f.catalog == nil {
		return 0
	}
	f.catMu.Lock()
	defer f.catMu.Unlock()
	if f.catLimit == nil || f.now().Sub(f.catAt) > catalogTTL {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		groups, err := f.catalog(cctx)
		cancel()
		if err != nil {
			logx.WithContext(ctx).Errorf("studio: 拉 gateway 模型组目录失败，参考图上限用兜底值: %v", err)
			f.catAt = f.now().Add(time.Minute - catalogTTL)
			if f.catLimit == nil {
				f.catLimit = map[string]int{}
			}
		} else {
			lim := map[string]int{}
			for _, g := range groups {
				if !g.Enabled {
					continue
				}
				lim[g.Code] = 0
				if n := g.FeatureLimits.MaxReferenceImages; n != nil && *n > 0 {
					lim[g.Code] = *n
				}
			}
			f.catLimit, f.catAt = lim, f.now()
		}
	}
	return f.catLimit[group]
}

// ---------------------------------------------------------------- 一轮

func (f *flow) tick(ctx context.Context) {
	f.maintain(ctx)
	f.advanceBatches(ctx)
	f.submitGenerating(ctx)
	f.pollRunning(ctx)
	f.sync(ctx)
}

// advanceBatches 排队 / 切图中 / 反推中 的批次交给后台编排（重启 / 接管后从头重放，各步有缓存与幂等键）。
// 拿到本地名额后再抢批次租约，抢不到（别的实例在编排）就跳过。
func (f *flow) advanceBatches(ctx context.Context) {
	for _, st := range []int8{model.BatchQueued, model.BatchCropping, model.BatchPrompting} {
		list, err := f.repo.Batch.ListByStatus(ctx, st, f.batchSize())
		if err != nil {
			logx.WithContext(ctx).Errorf("studio: 列批次(%d)失败: %v", st, err)
			continue
		}
		for _, b := range list {
			id := b.ID
			f.goClaimed(fmt.Sprintf("b:%d", id), func() {
				release, err := f.sem.Acquire(ctx, semBatch, positive(f.cfg.Poll.BatchConcurrency, 2))
				if err != nil {
					return
				}
				defer release()
				lctx, stop, ok, err := f.repo.Batch.KeepLease(ctx, id, f.owner, leaseOrchestrate)
				if err != nil {
					logx.WithContext(ctx).Errorf("studio: 批次 %d 抢租约失败: %v", id, err)
					return
				}
				if !ok {
					return
				}
				f.orchestrate(lctx, id)
				started := f.submitBatch(lctx, id)
				stop()
				f.spawnPolls(ctx, started)
			})
		}
	}
}

// submitGenerating 生成中 的批次：派发排队的子任务；没有排队 / 生成中的就收敛终态（兜底）。
func (f *flow) submitGenerating(ctx context.Context) {
	list, err := f.repo.Batch.ListByStatus(ctx, model.BatchGenerating, f.batchSize())
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 列生图中批次失败: %v", err)
		return
	}
	for _, b := range list {
		key := fmt.Sprintf("s:%d", b.ID)
		if !f.claim(key) {
			continue
		}
		ok, err := f.repo.Batch.AcquireLease(ctx, b.ID, f.owner, leasePoll)
		if err != nil {
			logx.WithContext(ctx).Errorf("studio: 批次 %d 抢租约失败: %v", b.ID, err)
		}
		if ok { // 抢不到：别的实例在编排 / 派发这个批次
			started := f.submitBatch(ctx, b.ID)
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := f.repo.Batch.ReleaseLease(rctx, b.ID, f.owner); err != nil {
				logx.WithContext(ctx).Errorf("studio: 批次 %d 释放租约失败（到期自动失效）: %v", b.ID, err)
			}
			cancel()
			f.spawnPolls(ctx, started)
		}
		f.release(key)
	}
}

// spawnPolls 刚派发的子任务各起轮询协程（用 Poller 的 ctx，不随批次租约结束而取消）。
func (f *flow) spawnPolls(ctx context.Context, ids []int64) {
	for _, id := range ids {
		f.spawnPoll(ctx, id)
	}
}

// submitBatch 批次进 生成中 后：排队的子任务按每批并发名额 CAS 排队→生成中，返回刚派发的子任务（调用方起轮询协程，
// 提交在 pollTask 里做）。调用方须持有批次租约：多实例时每批的并发名额才不会被各自算一遍。
func (f *flow) submitBatch(ctx context.Context, batchID int64) (started []int64) {
	log := logx.WithContext(ctx)
	b, err := f.repo.Batch.GetByID(ctx, batchID, 0)
	if err != nil || b == nil || b.Status != model.BatchGenerating {
		return nil
	}
	if !f.gwOn {
		log.Errorf("studio: 批次 %d 待生图，但 ai-gateway 未配置", b.ID)
		return nil
	}
	tasks, err := f.repo.Task.ListByBatch(ctx, b.ID)
	if err != nil {
		log.Errorf("studio: 批次 %d 列子任务失败: %v", b.ID, err)
		return nil
	}
	running, queued := 0, 0 // running 含交付中：已出图待结算的也占本批名额，避免交付卡住时又派发一批
	for _, t := range tasks {
		switch t.Status {
		case model.TaskRunning, model.TaskDelivering:
			running++
		case model.TaskQueued:
			queued++
		}
	}
	if running == 0 && queued == 0 {
		f.converge(ctx, b.ID)
		return nil
	}
	limit := positive(f.cfg.Poll.Concurrency, 3)
	if queued == 0 || running >= limit {
		return nil
	}
	// 全局上限：库里生成中的总数（多实例共用）达到 Poll.MaxRunning 就不再派发，等下一轮
	global, err := f.repo.Task.CountByStatus(ctx, model.TaskRunning)
	if err != nil {
		log.Errorf("studio: 统计生成中子任务失败，本轮不派发: %v", err)
		return nil
	}
	slots := int64(positive(f.cfg.Poll.MaxRunning, 64)) - global
	for _, t := range tasks {
		if running >= limit || slots <= 0 {
			break
		}
		if t.Status != model.TaskQueued || t.AdminLocked != 0 {
			continue
		}
		// started_at 先置空：真正提交到 gateway 时（SetGateway 写调用号）才记，等提交名额的时间不算进生图时限
		ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskQueued, model.TaskRunning, map[string]any{"started_at": nil})
		if err != nil {
			log.Errorf("studio: 子任务 %d 排队→生成中失败: %v", t.ID, err)
			continue
		}
		if !ok { // 用户刚取消 / 别处已派发
			continue
		}
		running++
		slots--
		started = append(started, t.ID)
	}
	return started
}

// pollRunning 生成中 的子任务逐个长轮询；交付中 的（结算前进程退出）续做交付。
func (f *flow) pollRunning(ctx context.Context) {
	if !f.gwOn {
		return
	}
	for _, st := range []int8{model.TaskRunning, model.TaskDelivering} {
		list, err := f.repo.Task.ListByStatus(ctx, st, f.batchSize())
		if err != nil {
			logx.WithContext(ctx).Errorf("studio: 列子任务(%d)失败: %v", st, err)
			continue
		}
		for _, t := range list {
			f.spawnPoll(ctx, t.ID)
		}
	}
}

// spawnPoll 起一个子任务的轮询协程：本地去重 → 本地名额 → 抢子任务租约（抢不到 = 别的实例在轮询，跳过）。
func (f *flow) spawnPoll(ctx context.Context, taskID int64) {
	f.goClaimed(fmt.Sprintf("t:%d", taskID), func() {
		release, err := f.sem.Acquire(ctx, semPoll, positive(f.cfg.Poll.Workers, 16))
		if err != nil {
			return
		}
		defer release()
		lctx, stop, ok, err := f.repo.Task.KeepLease(ctx, taskID, f.owner, leasePoll)
		if err != nil {
			logx.WithContext(ctx).Errorf("studio: 子任务 %d 抢租约失败: %v", taskID, err)
			return
		}
		if !ok {
			return
		}
		defer stop()
		f.pollTask(lctx, taskID)
	})
}

// ---------------------------------------------------------------- 收敛与日志

// converge 子任务全部到终态时按结果定批次终态，计数按库里实数重写（自愈重复 / 漏记）。任何非终态都可直接收敛。
func (f *flow) converge(ctx context.Context, batchID int64) {
	log := logx.WithContext(ctx)
	b, err := f.repo.Batch.GetByID(ctx, batchID, 0)
	if err != nil || b == nil || model.BatchFinal(b.Status) || b.Status == model.BatchSubmitting {
		return // 提交中的由提交链路 / 兜底收尾
	}
	counts, err := f.repo.Task.CountByBatch(ctx, b.ID)
	if err != nil {
		log.Errorf("studio: 批次 %d 统计子任务失败: %v", b.ID, err)
		return
	}
	to, ok := batchFinalStatus(counts)
	if !ok {
		return
	}
	extra := map[string]any{
		"task_done":   counts[model.TaskSucceeded],
		"task_failed": counts[model.TaskFailed] + counts[model.TaskCanceled],
	}
	ok, err = f.repo.Batch.UpdateStatus(ctx, b.ID, b.Status, to, extra)
	if err != nil {
		log.Errorf("studio: 批次 %d 收敛失败: %v", b.ID, err)
		return
	}
	if !ok {
		return
	}
	f.stepLog(ctx, b.ID, model.StepLog{Step: stepFinished, TaskTotal: int(b.TaskTotal), Detail: batchStatusName(to),
		Ms: f.now().Sub(b.CreatedAt).Milliseconds()})
	f.sync(ctx)
}

// stepLog 追加 step_log；失败只记日志。
func (f *flow) stepLog(ctx context.Context, batchID int64, e model.StepLog) {
	if e.At == 0 {
		e.At = f.now().UnixMilli()
	}
	e.Detail = repo.Trunc(e.Detail, 200)
	if err := f.repo.Batch.AppendStepLog(ctx, batchID, e); err != nil {
		logx.WithContext(ctx).Errorf("studio: 批次 %d 写 step_log 失败: %v", batchID, err)
	}
}

// logStep 一行 JSON 日志（验收 I：job_id/step/ms/images/crops/degraded/cost；不含 base64 / Key / 完整 prompt）+ 写 step_log。
func (f *flow) logStep(ctx context.Context, batchID int64, lg pipeline.StepLog) {
	fields := []logx.LogField{
		logx.Field("job_id", lg.JobID), logx.Field("step", lg.Step), logx.Field("ms", lg.Ms),
		logx.Field("images", lg.Images), logx.Field("crops", lg.Crops), logx.Field("degraded", lg.Degraded),
		logx.Field("cost", lg.CostCNY),
	}
	if len(lg.Warnings) > 0 {
		fields = append(fields, logx.Field("warnings", lg.Warnings))
	}
	if lg.Error != "" {
		fields = append(fields, logx.Field("error", repo.Trunc(lg.Error, 300)))
		logx.WithContext(ctx).Errorw("studio step", fields...)
	} else {
		logx.WithContext(ctx).Infow("studio step", fields...)
	}
	detail := lg.Error
	if detail == "" && len(lg.Warnings) > 0 {
		detail = strings.Join(lg.Warnings, "；")
	}
	f.stepLog(ctx, batchID, model.StepLog{
		Step: lg.Step, Ms: lg.Ms, Images: lg.Images, Crops: lg.Crops, Degraded: lg.Degraded, CostCNY: lg.CostCNY, Detail: detail,
	})
}

// parseStepLogs 解析批次 step_log（坏数据当空）。
func parseStepLogs(raw json.RawMessage) []model.StepLog {
	var out []model.StepLog
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

var errAborted = errors.New("studio: 批次状态已变化，停止编排")
