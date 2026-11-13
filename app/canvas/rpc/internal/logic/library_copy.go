package logic

// 产物转存素材库移出交付路径（Library.AsyncCopy，outbox = afv_canvas_copy_job，Go_V1.0.29）。
//
//	上游出图 ──▶ stageOutputs：gateway 产物已有 rid → mr://{rid}.{ext}；Java 结果只有外链 → 同步拉入（外链可能是会过期的签名地址，
//	             必须先落成我方 rid 才算持久）→ mr://{rid}.{ext}
//	         ──▶ 同一事务：CAS 成功（候选 / 视频写 mr:// 引用）+ 落 outbox（INSERT IGNORE，每 run 一行）──▶ 结算 ──▶ 用户立即可见
//	         ──▶ Kick：本进程后台立即转存；进程崩溃 / 名额满时由扫描到点接手
//	后台：认领（attempt CAS）→ 逐张 resources/copy（幂等键 canvas-copy:{task_id}:{i}，视频 i=99，与同步转存同一批键）
//	      → 把 run 的引用换成素材库公开地址（SwapRefs）→ 入素材库（同 source_task_id 只写一次）→ 完成
//	      失败按 Library.BackoffSeconds 退避，超过 Library.MaxAttempts 或 4xx → 永久失败：run 留着 mr://（照常可看可下载），
//	      error_code 打 LIBRARY_COPY_FAILED，接口 library_status=failed，ERROR 日志 check=canvas.library_copy_failed。
//
// 不变量：
//   - 落库的只有长期引用（mr://）或素材库公开地址，绝不落签名地址；读出口统一过 resolveMediaRefs 现签 10 分钟。
//   - 结算（确认 / 退款）只看 run 是否成功，与转存无关：转存失败不退款（产物已交付、用户能看能下载），转存成功也不再动账。
//   - outbox 与置成功同事务：有 outbox 必有成功 run，成功 run（异步路径）必有 outbox；落 outbox 失败（如 Go_V1.0.29 还没执行）
//     时整个事务回滚、本次退回同步转存，行为与改造前一致，不会出现「成功了却没人转存」。
//   - 写 run（换引用）与入素材库之前先续认领（Renew，attempt CAS）：认领已被接手的旧持有者不再写，素材不会入两组。
//   - 付费产物一定进素材库（产品口径：否则用户会对扣费有异议）：只要 run 是成功态（已交付、已结算确认），转存就做完——
//     用户在转存完成前删了画布（画布软删不动 run）或 run 被标了 deleted，照常复制、换 run 引用（SwapRefs 本来就不看 deleted）、
//     入素材库（素材行的用户 / 提示词 / 参数都取自 run 自己，不读画布）。作废只留给「run 不是成功态」（失败 / 取消 / 退款，
//     没有付费成品）。run 行被硬删除（只有人工改库会这样）时用 outbox 行自己的 src_snapshot（Go_V1.0.31，落行时存下的
//     kind / 模型组 / 请求 / mr:// 候选与视频等入素材库所需字段）兜底：照常复制、入素材库（同 source_task_id 只写一次），没有 run 行
//     可换引用；只有本列上线前落的旧行（src_snapshot 为 NULL）无从复制，仍作废并打 ERROR check=canvas.library_copy_orphaned。
//     run 的产物不会被改写（重跑是新的 run 行），没有「成品已换」。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/taskevent"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

const (
	// copyKickGrace 落 outbox 后多久对扫描可见：留给本进程的即时转存，进程崩溃时扫描最多晚这么久接手。
	copyKickGrace = 10
	// copyFailedCheck 转存永久失败的告警判据。
	copyFailedCheck = "canvas.library_copy_failed"
	// copyOrphanedCheck run 行已不存在、无从转存的告警判据（只有人工改库会走到）。
	copyOrphanedCheck = "canvas.library_copy_orphaned"
	// copyVideoIdx 视频成片的转存下标（与改造前同步转存一致）。
	copyVideoIdx = 99
)

// LibraryStatus 接口 library_status：产物是否已在素材库。
const (
	LibraryPending = "pending" // 已交付（mr:// 现签地址可看可下载），后台转存中
	LibraryFailed  = "failed"  // 转存永久失败：仍可看可下载，不在素材库
)

var defaultCopyBackoff = []int{5, 15, 30, 60, 120, 300, 600, 1800}

var (
	copyJobs = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "canvas",
		Subsystem: "library_copy",
		Name:      "jobs_total",
		Help:      "画布产物异步转存素材库的结局：done 完成 / retry 暂时失败待重试 / failed 永久失败 / void 作废 / fallback 落 outbox 失败退回同步转存",
		Labels:    []string{"result"},
	})
	copyLag = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "canvas",
		Subsystem: "library_copy",
		Name:      "lag_ms",
		Help:      "run 置成功到产物进素材库的延迟（毫秒）",
		Labels:    []string{"kind"},
		Buckets:   []float64{500, 1000, 2000, 5000, 10000, 30000, 60000, 300000, 1800000},
	})
)

// ---------------- 引用 ----------------

// mimeExt mime → mr:// 引用的扩展名（只用于读侧判断媒体类型，不影响寻址）；不认识时按视频 mp4 / 图片 png。
func mimeExt(mime string, video bool) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "video/mp4":
		return "mp4"
	case "video/quicktime":
		return "mov"
	case "video/webm":
		return "webm"
	}
	if video {
		return "mp4"
	}
	return "png"
}

// mrRef rid + 扩展名 → mr://{rid}.{ext}（与 Java MediaRefs / 网关同一写法）。
func mrRef(rid, ext string) string {
	if ext == "" {
		return refPrefix + rid
	}
	return refPrefix + rid + "." + ext
}

// refExtOf 引用里的扩展名（没有返回空）。
func refExtOf(v string) string {
	body := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(v), refWMPrefix), refPrefix)
	if i := strings.LastIndexByte(body, '.'); i > 0 {
		return body[i+1:]
	}
	return ""
}

// ridClass rid 的类别段（<实例>-<类别>-<编码>）。
func ridClass(rid string) string {
	parts := strings.Split(rid, "-")
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// pendingRef 待转存的私有引用：mr://（不是免费档水印视图 mrw://，那是 studio 成品，不归这里转存）且不在素材库类别。
func pendingRef(v string) (string, bool) {
	rid, wm, ok := refRID(v)
	if !ok || wm || rid == "" || ridClass(rid) == "asset" {
		return "", false
	}
	return rid, true
}

func hasPendingRefs(cands []model.Candidate, videoURL string) bool {
	for _, c := range cands {
		if _, ok := pendingRef(c.URL); ok {
			return true
		}
	}
	_, ok := pendingRef(videoURL)
	return ok
}

// libraryStatus 成功 run 的产物是否已在素材库：""（已在 / 不适用）、pending、failed。
func libraryStatus(r *model.NodeRun) string {
	if r == nil || r.Status != model.RunSucceeded || r.Backend == model.BackendStudio || !hasPendingRefs(r.CandidateList(), r.VideoURL.String) {
		return ""
	}
	if r.ErrorCode.Valid && r.ErrorCode.String == model.CopyFailedCode {
		return LibraryFailed
	}
	return LibraryPending
}

// ---------------- 收尾：产物落成长期引用 ----------------

type outputJob struct {
	idx   int
	m     mediaOutput
	video bool
}

func outputJobs(run *model.NodeRun, o outcome) []outputJob {
	var outs []mediaOutput
	if run.Kind == model.KindVideo {
		if o.cover != nil {
			outs = append(outs, *o.cover)
		}
	} else {
		outs = o.outputs
	}
	jobs := make([]outputJob, 0, len(outs)+1)
	for i, m := range outs {
		jobs = append(jobs, outputJob{idx: i, m: m})
	}
	if o.video != nil {
		jobs = append(jobs, outputJob{idx: copyVideoIdx, m: *o.video, video: true})
	}
	return jobs
}

// stageOutputs 上游产物 → 候选 / 视频地址：studio 成品（Stable）原样；有 rid 的写 mr://{rid}.{ext}；只有外链的（Java 结果）
// 先同步拉入媒体服务再写 mr://。媒体服务未配置时（仅开发）原样用上游地址。ok=false 表示本轮不收尾（暂时失败下轮重试，
// 或永久失败已置失败退款）。
func (c *core) stageOutputs(ctx context.Context, run *model.NodeRun, o outcome) (cands []model.Candidate, videoURL string, ok bool) {
	jobs := outputJobs(run, o)
	res := make([]model.Candidate, len(jobs))
	errs := fanout(len(jobs), c.fanoutLimit(), func(j int) error {
		jb := jobs[j]
		m := jb.m
		switch {
		case m.Stable:
			res[j] = model.Candidate{RID: m.RID, URL: m.URL, W: m.W, H: m.H}
			return nil
		case c.s.Media == nil:
			logx.WithContext(ctx).Infof("canvas: 媒体服务未配置，run %d 的产物直接使用上游地址（仅开发环境）", run.ID)
			res[j] = model.Candidate{RID: m.RID, URL: m.URL, W: m.W, H: m.H}
			return nil
		case m.RID == "":
			got, err := c.s.Media.IngestURL(ctx, m.URL, mediasvc.PurposeIntermediate, "")
			if err != nil {
				return mediaErr("拉取产物失败", err)
			}
			m.RID, m.Mime = got.RID, firstNonEmpty(m.Mime, got.Mime)
			if m.W == 0 {
				m.W, m.H = got.Width, got.Height
			}
		}
		ext := mimeExt(m.Mime, jb.video)
		if !mediasvc.ValidRID(m.RID) {
			// 拼不成合法引用（读侧会签成空）：这一张当场转存，与改造前一致
			cand, err := c.copyToLibrary(ctx, m.RID, fmt.Sprintf("canvas-copy:%s:%d", run.TaskID.String, jb.idx), ext)
			if err != nil {
				return err
			}
			if cand.W == 0 {
				cand.W, cand.H = m.W, m.H
			}
			res[j] = cand
			return nil
		}
		res[j] = model.Candidate{RID: m.RID, URL: mrRef(m.RID, ext), W: m.W, H: m.H}
		return nil
	})
	if !c.outputErrs(ctx, run, jobs, errs) {
		return nil, "", false
	}
	for j, jb := range jobs {
		if jb.video {
			videoURL = res[j].URL
		} else {
			cands = append(cands, res[j])
		}
	}
	return cands, videoURL, true
}

// outputErrs 按下标顺序取第一个错误：暂时性 → 本轮不收尾（下轮重试）；永久 → 失败退款。返回 true 表示没有错误。
func (c *core) outputErrs(ctx context.Context, run *model.NodeRun, jobs []outputJob, errs []error) bool {
	for j, err := range errs {
		if err == nil {
			continue
		}
		what := "产物"
		if jobs[j].video {
			what = "视频"
		}
		if isTemporary(err) {
			logx.WithContext(ctx).Errorf("canvas: run %d %s转存暂时失败，下轮重试: %v", run.ID, what, err)
			return false
		}
		c.failRun(ctx, run, model.RunRunning, "UPSTREAM_FAILED", what+"转存失败", true)
		return false
	}
	return true
}

// copyRefs 把候选 / 视频里待转存的 mr:// 逐个复制到素材库桶（并发 Poll.FanoutConcurrency，幂等键 canvas-copy:{task_id}:{i}），
// 返回换好地址的新切片。errs 与 jobs 一一对应（只含待转存的那些），调用方按顺序判定。
func (c *core) copyRefs(ctx context.Context, run *model.NodeRun, cands []model.Candidate, videoURL string) ([]model.Candidate, string, []outputJob, []error) {
	type item struct {
		job  outputJob
		pos  int // 候选下标；视频为 -1
		rid  string
		ext  string
		w, h int
	}
	var items []item
	for i, cd := range cands {
		if rid, ok := pendingRef(cd.URL); ok {
			items = append(items, item{job: outputJob{idx: i}, pos: i, rid: rid, ext: refExtOf(cd.URL), w: cd.W, h: cd.H})
		}
	}
	if rid, ok := pendingRef(videoURL); ok {
		items = append(items, item{job: outputJob{idx: copyVideoIdx, video: true}, pos: -1, rid: rid, ext: refExtOf(videoURL)})
	}
	out := append([]model.Candidate(nil), cands...)
	got := make([]model.Candidate, len(items))
	errs := fanout(len(items), c.fanoutLimit(), func(j int) error {
		it := items[j]
		cand, err := c.copyToLibrary(ctx, it.rid, fmt.Sprintf("canvas-copy:%s:%d", run.TaskID.String, it.job.idx), it.ext)
		if err != nil {
			return err
		}
		if cand.W == 0 {
			cand.W, cand.H = it.w, it.h
		}
		got[j] = cand
		return nil
	})
	jobs := make([]outputJob, len(items))
	for j, it := range items {
		jobs[j] = it.job
		if errs[j] != nil {
			continue
		}
		if it.pos < 0 {
			videoURL = got[j].URL
		} else {
			out[it.pos] = got[j]
		}
	}
	return out, videoURL, jobs, errs
}

// asyncCopyOn 本 run 的转存能否走 outbox（开关开、媒体服务已配、有 task_id 可做幂等键）。
func (c *core) asyncCopyOn(run *model.NodeRun) bool {
	return c.s.Config.Library.AsyncCopy && c.s.Media != nil && run.TaskID.Valid && run.TaskID.String != ""
}

// errCopyEnqueue 落 outbox 失败（如 Go_V1.0.29 还没执行）：事务已回滚（run 仍是运行中），调用方退回同步转存。
var errCopyEnqueue = errors.New("落转存 outbox 失败")

// transitWithCopyJob 同一事务里 CAS 运行中 → 成功（候选 / 视频是 mr:// 引用）并落 outbox：二者同生同灭，
// 不会出现「成功了却没有 outbox」或「outbox 指着一个还在运行中的 run」。ok=false 表示 CAS 没抢到（已取消 / 强制失败），什么都没写。
func (c *core) transitWithCopyJob(ctx context.Context, run *model.NodeRun, set map[string]any) (bool, error) {
	bctx, cancel := detached(ctx)
	defer cancel()
	ok := false
	err := c.s.Repo.RunInTx(bctx, func(ctx context.Context, tx *repo.Repo) error {
		got, err := tx.Run.Transit(ctx, run.ID, model.RunRunning, model.RunSucceeded, set)
		if err != nil || !got {
			return err
		}
		var cands json.RawMessage
		if v, ok := set["candidates"].(string); ok {
			cands = json.RawMessage(v)
		}
		video, _ := set["video_url"].(string)
		if err := tx.CopyJob.Enqueue(ctx, run.ID, run.TaskID.String, run.UserID, copyKickGrace, copySnapshotOf(run, cands, video)); err != nil {
			return fmt.Errorf("%w: %w", errCopyEnqueue, err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

// copySnapshot afv_canvas_copy_job.src_snapshot（Go_V1.0.31）：入素材库所需的 run 字段（assetList / copyRefs 读的那些），
// run 行被硬删除时据此兜底转存、入素材库。candidates / video_url 是置成功时写进 run 的 mr:// 引用。
type copySnapshot struct {
	Kind       model.RunKind   `json:"kind"`
	ModelGroup string          `json:"model_group,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
	CanvasID   string          `json:"canvas_id,omitempty"`
	NodeID     string          `json:"node_id,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	Candidates json.RawMessage `json:"candidates,omitempty"`
	VideoURL   string          `json:"video_url,omitempty"`
}

// copySnapshotOf 落 outbox 时存下的快照（序列化失败返回 nil：本行退回旧行为，不挡置成功）。
func copySnapshotOf(run *model.NodeRun, cands json.RawMessage, videoURL string) []byte {
	snap := copySnapshot{Kind: run.Kind, ModelGroup: run.ModelGroup, RunID: run.RunID, CanvasID: run.CanvasID, NodeID: run.NodeID,
		Request: validJSON(run.Request), Candidates: validJSON(cands), VideoURL: videoURL}
	b, err := json.Marshal(snap)
	if err != nil {
		return nil
	}
	return b
}

func validJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return raw
}

// orphanRun run 行已不存在时按快照拼一个只够转存 + 入素材库的 run（成功态；主键 / task_id / 用户取自 outbox 行）。
// 快照为空 / 解析失败 / 没有待转存也没有可入库的产物时返回 nil（旧行为：作废）。
func orphanRun(j *model.CopyJob, raw []byte) *model.NodeRun {
	if len(raw) == 0 {
		return nil
	}
	var snap copySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil || !snap.Kind.Valid() {
		return nil
	}
	r := &model.NodeRun{ID: j.RunPK, UserID: j.UserID, Status: model.RunSucceeded, Kind: snap.Kind, Backend: model.BackendOf(snap.Kind),
		ModelGroup: snap.ModelGroup, RunID: snap.RunID, CanvasID: snap.CanvasID, NodeID: snap.NodeID,
		Request: snap.Request, Candidates: snap.Candidates, Deleted: 1}
	r.TaskID.String, r.TaskID.Valid = j.TaskID, j.TaskID != ""
	if snap.VideoURL != "" {
		r.VideoURL.String, r.VideoURL.Valid = snap.VideoURL, true
	}
	if len(r.CandidateList()) == 0 && snap.VideoURL == "" {
		return nil
	}
	return r
}

// kickCopy 交给本进程的转存池立即执行（没有池时等扫描接手）。
func (c *core) kickCopy(run *model.NodeRun) {
	if c.s.KickCopy != nil {
		c.s.KickCopy(run.ID)
	}
}

// ---------------- 后台转存 ----------------

func (c *core) copyBackoff(attempt int32) int {
	list := c.s.Config.Library.BackoffSeconds
	if len(list) == 0 {
		list = defaultCopyBackoff
	}
	i := int(attempt) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(list) {
		i = len(list) - 1
	}
	return max(1, list[i])
}

func (c *core) copyMaxAttempts() int32 {
	if n := c.s.Config.Library.MaxAttempts; n > 0 {
		return min(int32(n), repo.CopyAttemptEpoch-1) // 一代内的次数不能跨过代际步长
	}
	return 10
}

func (c *core) copyClaimSeconds() int {
	if n := c.s.Config.Library.ClaimSeconds; n > 0 {
		return n
	}
	return 300
}

// runCopyJob 执行一行 outbox（先认领；没抢到说明别的实例 / 协程在做）。
func (c *core) runCopyJob(ctx context.Context, j *model.CopyJob) {
	log := logx.WithContext(ctx)
	ok, err := c.s.Repo.CopyJob.Claim(ctx, j.ID, j.Attempt, c.copyClaimSeconds())
	if err != nil {
		log.Errorf("canvas: 认领转存任务 %d（run %d）失败: %v", j.ID, j.RunPK, err)
		return
	}
	if !ok {
		return
	}
	attempt := j.Attempt + 1
	// 结果写回不随 ctx 取消（进程停止时把本轮结局记下来，免得干等认领超时）
	wctx, cancel := detached(ctx)
	defer cancel()
	finish := func(status int8, why, result string) {
		if _, err := c.s.Repo.CopyJob.Finish(wctx, j.ID, attempt, status, why); err != nil {
			log.Errorf("canvas: 转存任务 %d（run %d）写终态失败（认领到期后重做，幂等）: %v", j.ID, j.RunPK, err)
			return
		}
		copyJobs.Inc(result)
	}
	round := repo.CopyRound(attempt) // 本代第几次（用户重试后 attempt 跳到下一个整千，见 repo.CopyAttemptEpoch）
	retry := func(err error) {
		if round >= c.copyMaxAttempts() {
			c.copyGiveUp(wctx, j, attempt, err)
			return
		}
		delay := c.copyBackoff(round)
		if werr := c.s.Repo.CopyJob.Retry(wctx, j.ID, attempt, delay, errText(err)); werr != nil {
			log.Errorf("canvas: 转存任务 %d（run %d）写重试失败: %v", j.ID, j.RunPK, werr)
		}
		copyJobs.Inc("retry")
		log.Infof("canvas: run %d 转存素材库暂时失败（第 %d 次），%d 秒后重试: %v", j.RunPK, round, delay, err)
	}
	run, err := c.s.Repo.Run.Get(ctx, j.RunPK)
	if err != nil {
		retry(err)
		return
	}
	if run != nil && !model.RunFinal(run.Status) {
		// outbox 与置成功同事务落库，正常不会看到未结束的 run；万一看到（人工改库等）不作废，退避后再看——
		// 作废后 run 再成功时 INSERT IGNORE 撞 uk_run 是空操作，就永远没人转存了
		retry(fmt.Errorf("run 状态 %d 还未结束", run.Status))
		return
	}
	orphan := run == nil
	if orphan {
		// run 行被硬删除（只有人工改库会走到）：按 outbox 行自己的快照兜底转存、入素材库
		raw, serr := c.s.Repo.CopyJob.SourceSnapshot(ctx, j.ID)
		if serr != nil {
			retry(fmt.Errorf("读转存快照失败: %w", serr))
			return
		}
		if run = orphanRun(j, raw); run == nil {
			// 旧行（Go_V1.0.31 之前落的）没有快照，无从复制
			finish(model.CopyVoid, "run 行已不存在，无产物可转存", "void")
			log.Errorw(fmt.Sprintf("canvas: run %d 行已不存在且转存行没有产物快照，付费产物无从转存素材库，需人工核对 (check=%s)", j.RunPK, copyOrphanedCheck),
				logx.Field("task_id", j.TaskID), logx.Field("user_id", j.UserID))
			return
		}
		log.Infow(fmt.Sprintf("canvas: run %d 行已不存在，按转存行快照转存并入素材库 (check=%s)", j.RunPK, copyOrphanedCheck),
			logx.Field("task_id", j.TaskID), logx.Field("user_id", j.UserID))
	}
	// 不看 run.Deleted / 画布是否已删：用户删除不影响付费产物入素材库
	if run.Status != model.RunSucceeded {
		finish(model.CopyVoid, "run 已不是成功态", "void")
		return
	}
	// 写 run / 入素材库之前确认本轮认领还在（并续期）：复制耗时超过认领时长、已被别的实例接手时，旧持有者不再写，
	// 免得两边各入一组素材（InsertSetOnce 的查重不加锁，互斥靠认领）
	stillMine := func() bool {
		held, err := c.s.Repo.CopyJob.Renew(wctx, j.ID, attempt, c.copyClaimSeconds())
		if err != nil {
			log.Errorf("canvas: 转存任务 %d（run %d）续认领失败（认领到期后重做，幂等）: %v", j.ID, j.RunPK, err)
			return false
		}
		if !held {
			log.Infof("canvas: 转存任务 %d（run %d）认领已被接手，本轮不再写", j.ID, j.RunPK)
		}
		return held
	}
	cands, video := run.CandidateList(), run.VideoURL.String
	if hasPendingRefs(cands, video) {
		tid, rn := c.traceIDOf(j.RunPK), int(round)
		c.s.Events.Start(wctx, tid, stRehost, rn, taskevent.Attrs{"async": true, "outputs": len(jobsOf(cands, video)), "kind": string(run.Kind)})
		nc, nv, jobs, errs := c.copyRefs(ctx, run, cands, video)
		for i, e := range errs {
			if e == nil {
				continue
			}
			class := "PERMANENT"
			if isTemporary(e) {
				class = "TEMPORARY"
			}
			c.s.Events.Finish(wctx, tid, stRehost, rn, taskevent.StatusFailed,
				taskevent.Fail{Class: class, Code: "REHOST_FAILED", Check: "canvas.rehost", Message: traceErrMsg(e)}, taskevent.Attrs{"async": true, "round": rn})
			if !isTemporary(e) {
				what := "产物"
				if jobs[i].video {
					what = "视频"
				}
				c.copyGiveUp(wctx, j, attempt, fmt.Errorf("%s转存失败: %w", what, e))
				return
			}
			retry(e)
			return
		}
		c.s.Events.Finish(wctx, tid, stRehost, rn, taskevent.StatusOK, taskevent.Fail{}, taskevent.Attrs{"async": true, "round": rn})
		if !stillMine() {
			return
		}
		if !orphan { // 没有 run 行可换引用：入了素材库即完成
			ok, err := c.s.Repo.Run.SwapRefs(wctx, run.ID, repo.MarshalCandidates(nc), nv)
			if err != nil {
				retry(err)
				return
			}
			if !ok {
				finish(model.CopyVoid, "run 已不是成功态", "void")
				return
			}
		}
		cands, video = nc, nv
	}
	if !stillMine() {
		return
	}
	if err := c.insertAssetsOnce(wctx, run, cands, video); err != nil {
		retry(fmt.Errorf("入素材库失败: %w", err))
		return
	}
	finish(model.CopyDone, "", "done")
	copyLag.Observe(time.Since(j.CreatedAt).Milliseconds(), string(run.Kind))
}

// copyGiveUp 永久失败：outbox 行进终态与成功 run 打标记（library_status=failed）同一事务、以本轮认领 CAS（认领已被接手的
// 旧持有者什么都不写，见 repo.FailWithRunMark）；产物仍是 mr://，照常可看可下载。
func (c *core) copyGiveUp(ctx context.Context, j *model.CopyJob, attempt int32, err error) {
	log := logx.WithContext(ctx)
	won, ferr := c.s.Repo.FailWithRunMark(ctx, j.ID, j.RunPK, attempt, errText(err))
	if ferr != nil {
		log.Errorf("canvas: 转存任务 %d（run %d）写永久失败出错（认领到期后重做，幂等）: %v", j.ID, j.RunPK, ferr)
		return
	}
	if !won {
		log.Infof("canvas: 转存任务 %d（run %d）认领已被接手，本轮不写永久失败", j.ID, j.RunPK)
		return
	}
	copyJobs.Inc("failed")
	log.Errorw(fmt.Sprintf("canvas: run %d 产物转存素材库永久失败（第 %d 次），产物仍以私有引用可看可下载，未进素材库 (check=%s)",
		j.RunPK, repo.CopyRound(attempt), copyFailedCheck), logx.Field("task_id", j.TaskID), logx.Field("user_id", j.UserID), logx.Field("err", errText(err)))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------- 转存池 ----------------

// copier 本进程的转存执行池：Kick（刚置成功的 run）与扫描接手（到点的 outbox 行）共用 Library.Workers 个名额。
type copier struct {
	c        *core
	ctx      context.Context
	slots    chan struct{}
	inflight sync.Map // run 主键 → 本进程已在做（本地去重；跨实例互斥靠 attempt CAS）
	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
}

func newCopier(ctx context.Context, c *core) *copier {
	n := c.s.Config.Library.Workers
	if n <= 0 {
		n = 8
	}
	return &copier{c: c, ctx: ctx, slots: make(chan struct{}, n)}
}

// spawn 占名额起协程执行 fn；已在做 / 名额满 / 已关闭返回 false。
func (p *copier) spawn(runPK int64, fn func(ctx context.Context)) bool {
	if p == nil || runPK <= 0 {
		return false
	}
	if _, loaded := p.inflight.LoadOrStore(runPK, struct{}{}); loaded {
		return false
	}
	select {
	case p.slots <- struct{}{}:
	default:
		p.inflight.Delete(runPK)
		return false
	}
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		<-p.slots
		p.inflight.Delete(runPK)
		return false
	}
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		defer p.inflight.Delete(runPK)
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("canvas: run %d 转存素材库 panic: %v", runPK, r)
			}
		}()
		fn(p.ctx)
	}()
	return true
}

// Kick 刚置成功的 run 立即转存（按 run 主键取 outbox 行；行已不是待转存就什么都不做）。
func (p *copier) Kick(runPK int64) bool {
	return p.spawn(runPK, func(ctx context.Context) {
		j, err := p.c.s.Repo.CopyJob.GetByRun(ctx, runPK)
		if err != nil || j == nil || j.Status != model.CopyPending {
			if err != nil {
				logx.WithContext(ctx).Infof("canvas: run %d 即时转存取 outbox 失败（等扫描接手）: %v", runPK, err)
			}
			return
		}
		p.c.runCopyJob(ctx, j)
	})
}

// sweep 到点的 outbox 行交给池（不阻塞扫描；名额满就留到下一轮）。
func (p *copier) sweep(ctx context.Context) {
	if p == nil {
		return
	}
	n := p.c.s.Config.Library.SweepBatch
	if n <= 0 {
		n = 50
	}
	list, err := p.c.s.Repo.CopyJob.ListDue(ctx, n)
	if err != nil {
		if !isMissingTable(err) {
			logx.WithContext(ctx).Errorf("canvas: 查待转存 outbox 失败: %v", err)
		}
		return
	}
	for _, j := range list {
		j := j
		if !p.spawn(j.RunPK, func(ctx context.Context) { p.c.runCopyJob(ctx, j) }) && len(p.slots) == cap(p.slots) {
			return
		}
	}
}

// close 不再接新的，等在做的全部返回（调用方先取消根 ctx）。
func (p *copier) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.wg.Wait()
}

// isMissingTable outbox 表还没建（Go_V1.0.29 未执行）：扫描静默跳过，落 outbox 时退回同步转存。
func isMissingTable(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1146
}
