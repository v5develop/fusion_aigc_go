package logic

// 付费成品转存素材库移出交付路径（Library.AsyncCopy，outbox = afv_studio_copy_job，Go_V1.0.30；与画布 library_copy.go 同一套协议）。
//
//	网关出图 ──▶ 付费档：成品已有 rid → mr://{rid}.{ext}
//	         ──▶ 同一事务：CAS 生成中→交付中（result_url 写 mr:// 引用）+ 落 outbox（INSERT IGNORE，每子任务每张产物一行）
//	         ──▶ 结算 ──▶ CAS 交付中→成功 ──▶ 用户立即可见（读时现签）
//	         ──▶ Kick：本进程后台立即转存；进程崩溃 / 名额满时由扫描到点接手
//	后台：子任务还在交付中 → 推迟（不占尝试次数）；已不是成功态 / 成品已换 → 作废；用户删除（软删批次 / 子任务）不作废，照常入素材库
//	      认领（attempt CAS）→ resources/copy（幂等键 studio-copy:{src_rid}）→ 入素材库（只写公有地址，按 source_task_id 查重）
//	      → 按旧值 CAS 把子任务成品换成素材库公开地址（SwapLibraryRef）→ 完成 → 补服装蒙版（用公开地址，与之前同一缓存键）
//	      失败按 Library.BackoffSeconds 退避，超过 Library.MaxAttempts 或 4xx → 永久失败：子任务留着 mr://（照常可看可下载），
//	      接口 library_status=failed，ERROR 日志 check=studio.library_copy_failed。
//
// 不变量：
//   - afv_asset 只写素材库公开地址（或免费档 mrw://），绝不写 mr://：Java 侧有多个 afv_asset 读口不解析 mr://
//     （AssetController list-with-items、AdminUserAssetService、CompatUserAssetService、AgentWorkspaceAssembler、
//     VideoCoverBackfillService），所以「先交付」只动 studio 自己的子任务行，资产行等转存完成再写。
//   - 子任务 result_url 落库的只有长期引用（mr:// / mrw://）或素材库公开地址，绝不落签名地址；读出口统一过 signViews 现签。
//   - 结算（确认 / 退款）只看子任务交付，与转存无关：转存失败不退款（成品已交付、可看可下载），转存成功也不再动账。
//   - outbox 与进交付中同事务：有 outbox 必有交付中 / 之后的子任务；落 outbox 失败（如 Go_V1.0.30 还没执行）时整个事务回滚、
//     本次退回同步转存，行为与改造前一致。交付失败（预留失效等）会清掉 result_url，outbox 随之作废，不会把未交付的成品复制到公有桶。
//   - 入素材库与换地址之前先续认领（Renew，attempt CAS）：认领已被接手的旧持有者不再写，素材不会入两条。
//   - 付费成品一定进素材库（产品口径：否则用户会对扣费有异议）：用户在转存完成前删了批次（软删批次 + 子任务），转存照常进行、
//     照常入素材库、照常换子任务成品地址（已软删的行，用户看不到，换了无害，也省掉一套分支），只跳过服装蒙版（删了没人看，
//     补蒙版要付网关费用）。作废只留给「成品没付费 / 不再是它」：交付失败（预留失效）、退款、取消、成品已换。
//     子任务行被硬删除（只有人工改库会这样）时用 outbox 行自己的字段兜底：src_rid 照常复制，素材行的用户 / source_task_id
//     取自本行，kind / 入口码按任务号前缀反推（见 orphanTask），不再作废。
//   - 成品已换（重新生成 / 运营重做）：旧成品同样付过费，但用户侧重新生成在转存期间被拒（regenerate_task_logic.go，
//     「这张图正在存入素材库」），运营重做只收失败 / 已取消，所以正常走不到；万一走到（竞态 / 人工改库）仍作废——
//     此时再入旧成品会让新成品的 regen_asset_floor 查重误判「已入库」而漏写新成品——并打 ERROR check=studio.library_copy_superseded
//     交人工核对。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/pkg/mediasvc"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

const (
	// copyKickGrace 落 outbox 后多久对扫描可见：留给本进程置成功后的即时转存，进程崩溃时扫描最多晚这么久接手。
	copyKickGrace = 10
	// copyDeliverWait 子任务还在交付中（结算中）时推迟多久再看（不占尝试次数）。
	copyDeliverWait = 15
	// copyFailedCheck 转存永久失败的告警判据。
	copyFailedCheck = "studio.library_copy_failed"
	// copySupersededCheck 成品已换时作废的告警判据（正常走不到，见文件头不变量）。
	copySupersededCheck = "studio.library_copy_superseded"
	// mrPrefix 私有资源引用前缀（与网关 / Java MediaRefs 同一写法）。注意 mrw:// 不以它开头。
	mrPrefix = "mr://"
)

// 接口 library_status：成品是否已在素材库。
const (
	LibraryPending = "pending" // 已交付（mr:// 现签地址可看可下载），后台转存素材库中，asset_id 可能仍为 0
	LibraryFailed  = "failed"  // 转存永久失败：仍可看可下载，不在素材库
)

var defaultCopyBackoff = []int{5, 15, 30, 60, 120, 300, 600, 1800}

var (
	copyJobs = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "studio",
		Subsystem: "library_copy",
		Name:      "jobs_total",
		Help:      "studio 付费成品异步转存素材库的结局：done 完成 / retry 暂时失败待重试 / failed 永久失败 / void 作废 / fallback 落 outbox 失败退回同步转存",
		Labels:    []string{"result"},
	})
	copyLag = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "studio",
		Subsystem: "library_copy",
		Name:      "lag_ms",
		Help:      "子任务进交付中到成品进素材库的延迟（毫秒）",
		Labels:    []string{"kind"},
		Buckets:   []float64{500, 1000, 2000, 5000, 10000, 30000, 60000, 300000, 1800000},
	})
)

// ---------------- 引用 ----------------

// mrRef rid + mime → mr://{rid}.{ext}（扩展名只给读侧判断媒体类型，不影响寻址）。
func mrRef(rid, mime string) string { return mrPrefix + rid + "." + extOfMime(mime) }

// isPendingRef 是否是待转存的私有引用 mr://（水印视图 mrw:// 不算）。
func isPendingRef(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), mrPrefix) }

// mrRidOf mr://{rid}[.ext] → rid；不是私有引用或 rid 不合法返回 ""。
func mrRidOf(v string) string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, mrPrefix) {
		return ""
	}
	rid := strings.TrimPrefix(v, mrPrefix)
	if i := strings.LastIndexByte(rid, '.'); i > 0 {
		rid = rid[:i]
	}
	if !mediasvc.ValidRID(rid) {
		return ""
	}
	return rid
}

// ---------------- 交付侧 ----------------

// asyncCopyOn 本子任务的转存能否走 outbox（开关开、媒体服务已配、有子任务号作素材库 source_task_id）。
func (f *flow) asyncCopyOn(t *model.Task) bool {
	return f.cfg.Library.AsyncCopy && f.media != nil && f.repo != nil && t.TaskID.Valid && t.TaskID.String != ""
}

// errCopyEnqueue 落 outbox 失败（如 Go_V1.0.30 还没执行）：事务已回滚（子任务仍是生成中），调用方退回同步转存。
var errCopyEnqueue = errors.New("落转存 outbox 失败")

// transitWithCopyJob 同一事务里 CAS 生成中 → 交付中（result_url 是 mr:// 引用）并落 outbox：二者同生同灭。
// ok=false 表示 CAS 没抢到（已被强制失败 / 超时失败 / 别处处理），什么都没写。
func (f *flow) transitWithCopyJob(ctx context.Context, t *model.Task, b *model.Batch, set map[string]any, srcRid, ref string) (bool, error) {
	tctx, cancel := detached(ctx)
	defer cancel()
	ok := false
	err := f.repo.RunInTx(tctx, func(ctx context.Context, tx *repo.Repo) error {
		got, err := tx.Task.TransitStatus(ctx, t.ID, model.TaskRunning, model.TaskDelivering, set)
		if err != nil || !got {
			return err
		}
		j := &model.CopyJob{TaskPK: t.ID, TaskID: t.TaskID.String, UserID: b.UserID, SrcRid: srcRid, Ref: ref}
		if err := tx.CopyJob.Enqueue(ctx, j, copyKickGrace); err != nil {
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

// kickCopy 交给本进程的转存池立即执行（没有池时等扫描接手）。
func (f *flow) kickCopy(taskPK int64) {
	if f.cp != nil {
		f.cp.Kick(taskPK)
	}
}

// ---------------- 后台转存 ----------------

func (f *flow) copyBackoff(attempt int32) int {
	list := f.cfg.Library.BackoffSeconds
	if len(list) == 0 {
		list = defaultCopyBackoff
	}
	i := min(max(int(attempt)-1, 0), len(list)-1)
	return max(1, list[i])
}

func (f *flow) copyMaxAttempts() int32 {
	if n := f.cfg.Library.MaxAttempts; n > 0 {
		return int32(n)
	}
	return 10
}

func (f *flow) copyClaimSeconds() int {
	if n := f.cfg.Library.ClaimSeconds; n > 0 {
		return n
	}
	return 300
}

// copyIdem 复制的幂等键：同一产物重试 / 多实例并发得到同一个素材库 rid。
func copyIdem(j *model.CopyJob) string { return "studio-copy:" + j.SrcRid }

// jobLive 子任务是否仍以本行的引用作为成功成品（可以转存 / 换地址）。不看 deleted：用户删除批次不影响付费成品入素材库。
func jobLive(t *model.Task, j *model.CopyJob) bool {
	return t != nil && t.Status == model.TaskSucceeded && t.ResultURL.String == j.Ref
}

// orphanTask 子任务行已不存在（硬删除，只有人工改库会这样）时，用 outbox 行自己的字段拼一个只够入素材库的子任务：
// 主键 / 任务号 / 成品取自本行，kind 按任务号前缀反推（任务号 = <kind 前缀>_<坐标>_…，见 submit_common.newTaskID），
// 反推不出时 kind 为空（素材名 / 入口码为空，仍入库）。已删除标记置 1：不补蒙版。
func (f *flow) orphanTask(j *model.CopyJob) *model.Task {
	t := &model.Task{ID: j.TaskPK, Status: model.TaskSucceeded, Deleted: 1}
	t.TaskID.String, t.TaskID.Valid = j.TaskID, true
	t.ResultURL.String, t.ResultURL.Valid = j.Ref, true
	for _, k := range []model.Kind{model.KindTryon, model.KindPose, model.KindImageGen} {
		if p := svc.Prefix(f.cfg, k); p != "" && strings.HasPrefix(j.TaskID, p+"_") {
			t.Kind = k
			break
		}
	}
	return t
}

// voidCopyJob 作废一行（成品没付费 / 不再是它）。成品已换但旧成品付过费的（正常走不到）额外打 ERROR 交人工核对。
func (f *flow) voidCopyJob(ctx context.Context, j *model.CopyJob, attempt int32, t *model.Task) bool {
	ok, err := f.repo.CopyJob.Finish(ctx, j.ID, attempt, model.CopyVoid, "子任务已不是成功态 / 成品已换")
	if err != nil || !ok {
		if err != nil {
			logx.WithContext(ctx).Errorf("studio: 转存任务 %d（子任务 %d）写作废失败: %v", j.ID, j.TaskPK, err)
		}
		return false
	}
	copyJobs.Inc("void")
	if t != nil && t.Status != model.TaskFailed && t.Status != model.TaskCanceled {
		logx.WithContext(ctx).Errorw(fmt.Sprintf("studio: 子任务 %d 付费成品还没入素材库就被换掉（状态 %d），旧成品未入素材库，需人工核对 (check=%s)",
			j.TaskPK, t.Status, copySupersededCheck), logx.Field("task_id", j.TaskID), logx.Field("user_id", j.UserID), logx.Field("src_rid", j.SrcRid))
	}
	return true
}

// alreadySwapped 子任务成品已是素材库地址：上一轮换地址（SwapLibraryRef）已写成、但写终态前崩溃 / 写终态失败。
// 有待转存行期间，成功子任务的 result_url 只有本协议会改（重新生成在转存期间被拒，运营重做只收失败 / 已取消），
// 所以「成功、成品不再是引用而是素材库 rid」只能是本行换的，按完成收尾（并补上次没来得及补的蒙版）。软删的行同样可能是本行换的
// （换地址不看 deleted）。
func alreadySwapped(t *model.Task) bool {
	if t == nil || t.Status != model.TaskSucceeded {
		return false
	}
	u := t.ResultURL.String
	if u == "" || isPendingRef(u) || isWatermarkRef(u) {
		return false
	}
	parts := strings.Split(t.ResultRid.String, "-")
	return mediasvc.ValidRID(t.ResultRid.String) && len(parts) == 3 && parts[1] == "asset"
}

// runCopyJob 执行一行 outbox（先看子任务，再认领；没抢到说明别的实例 / 协程在做）。
func (f *flow) runCopyJob(ctx context.Context, j *model.CopyJob) {
	log := logx.WithContext(ctx)
	if f.media == nil {
		// 本实例没配媒体服务：不认领（认领后无法完成会白占尝试次数），留给配了的实例
		log.Errorf("studio: 转存任务 %d（子任务 %d）本实例未配置媒体服务，跳过 (check=%s)", j.ID, j.TaskPK, copyFailedCheck)
		return
	}
	// 结果写回不随 ctx 取消（进程停止时把本轮结局记下来，免得干等认领超时）
	wctx, cancel := detached(ctx)
	defer cancel()
	t, err := f.repo.Task.GetByID(ctx, j.TaskPK)
	if err != nil {
		log.Errorf("studio: 转存任务 %d（子任务 %d）读子任务失败，等下一轮: %v", j.ID, j.TaskPK, err)
		return
	}
	if t != nil && t.Status == model.TaskDelivering && t.ResultURL.String == j.Ref {
		// 还在结算：成品是否交付未定，不能先复制到公有桶。推迟，不认领、不占尝试次数
		if err := f.repo.CopyJob.Retry(wctx, j.ID, j.Attempt, copyDeliverWait, "子任务仍在交付中，等结算完成"); err != nil {
			log.Errorf("studio: 转存任务 %d（子任务 %d）推迟失败: %v", j.ID, j.TaskPK, err)
		}
		return
	}
	if t == nil {
		t = f.orphanTask(j) // 子任务行硬删除：用本行字段兜底入库（见文件头不变量）
	}
	if !jobLive(t, j) {
		if alreadySwapped(t) {
			if ok, err := f.repo.CopyJob.Finish(wctx, j.ID, j.Attempt, model.CopyDone, ""); err == nil && ok {
				copyJobs.Inc("done")
				log.Infof("studio: 转存任务 %d（子任务 %d）成品已换成素材库地址（上一轮写终态前中断），按完成收尾", j.ID, j.TaskPK)
				f.maskAfterCopy(wctx, t, t.ResultURL.String)
			}
			return
		}
		f.voidCopyJob(wctx, j, j.Attempt, t)
		return
	}
	ok, err := f.repo.CopyJob.Claim(ctx, j.ID, j.Attempt, f.copyClaimSeconds())
	if err != nil {
		log.Errorf("studio: 认领转存任务 %d（子任务 %d）失败: %v", j.ID, j.TaskPK, err)
		return
	}
	if !ok {
		return
	}
	attempt := j.Attempt + 1
	finish := func(status int8, why, result string) bool {
		ok, err := f.repo.CopyJob.Finish(wctx, j.ID, attempt, status, why)
		if err != nil {
			log.Errorf("studio: 转存任务 %d（子任务 %d）写终态失败（认领到期后重做，幂等）: %v", j.ID, j.TaskPK, err)
			return false
		}
		if ok {
			copyJobs.Inc(result)
		}
		return ok
	}
	giveUp := func(err error) {
		if finish(model.CopyFailed, copyErrText(err), "failed") {
			log.Errorw(fmt.Sprintf("studio: 子任务 %d 成品转存素材库永久失败（第 %d 次），成品仍以私有引用可看可下载，未进素材库 (check=%s)",
				j.TaskPK, attempt, copyFailedCheck), logx.Field("task_id", j.TaskID), logx.Field("user_id", j.UserID), logx.Field("err", copyErrText(err)))
			// 蒙版是成功后的可选步骤：转存不成也照补（maskSourceURL 给网关签一条干净短期地址）
			f.maskAfterCopy(wctx, t, j.Ref)
		}
	}
	retry := func(err error) {
		if ctx.Err() != nil {
			// 进程停止打断了本轮（复制 / 读库被取消）：不算真正失败，不据此记永久失败，尽快由扫描重做
			if werr := f.repo.CopyJob.Retry(wctx, j.ID, attempt, 1, "进程停止，本轮中断"); werr != nil {
				log.Errorf("studio: 转存任务 %d（子任务 %d）写重试失败: %v", j.ID, j.TaskPK, werr)
			}
			return
		}
		if attempt >= f.copyMaxAttempts() {
			giveUp(err)
			return
		}
		delay := f.copyBackoff(attempt)
		if werr := f.repo.CopyJob.Retry(wctx, j.ID, attempt, delay, copyErrText(err)); werr != nil {
			log.Errorf("studio: 转存任务 %d（子任务 %d）写重试失败: %v", j.ID, j.TaskPK, werr)
		}
		copyJobs.Inc("retry")
		log.Infof("studio: 子任务 %d 转存素材库暂时失败（第 %d 次），%d 秒后重试: %v", j.TaskPK, attempt, delay, err)
	}
	// 写素材库 / 换地址之前确认本轮认领还在（并续期）：复制耗时超过认领时长、已被别的实例接手时，旧持有者不再写
	stillMine := func() bool {
		held, err := f.repo.CopyJob.Renew(wctx, j.ID, attempt, f.copyClaimSeconds())
		if err != nil {
			log.Errorf("studio: 转存任务 %d（子任务 %d）续认领失败（认领到期后重做，幂等）: %v", j.ID, j.TaskPK, err)
			return false
		}
		if !held {
			log.Infof("studio: 转存任务 %d（子任务 %d）认领已被接手，本轮不再写", j.ID, j.TaskPK)
		}
		return held
	}

	lib, err := f.media.CopyToLibrary(ctx, j.SrcRid, mediasvc.PurposeAssetLibrary, copyIdem(j))
	if err == nil && lib.URL == "" {
		err = &mediasvc.Error{Status: 422, Code: "no_public_url", Message: "素材库桶没有公开地址"}
	}
	if err != nil {
		var me *mediasvc.Error
		if errors.As(err, &me) && me.Permanent() && ctx.Err() == nil {
			giveUp(fmt.Errorf("成品转存失败: %w", err))
			return
		}
		retry(err)
		return
	}
	if !stillMine() {
		return
	}
	// 复制可能耗时几秒：入库前再看一眼子任务（期间被重新生成的不入库；被删除的照常入库）
	if t, err = f.repo.Task.GetByID(wctx, j.TaskPK); err != nil {
		retry(err)
		return
	}
	orphan := t == nil
	if orphan {
		t = f.orphanTask(j)
	}
	if !jobLive(t, j) {
		f.voidCopyJob(wctx, j, attempt, t)
		return
	}
	var b *model.Batch
	if !orphan {
		if b, err = f.repo.Batch.GetByID(wctx, t.BatchID, 0); err != nil {
			retry(fmt.Errorf("读批次失败: %w", err))
			return
		}
	}
	if b == nil { // 批次 / 子任务行硬删除：用户取本行（宽高比等批次参数缺省）
		b = &model.Batch{ID: t.BatchID, UserID: j.UserID}
	}
	if err := f.saveAssetErr(wctx, t, b, lib.URL); err != nil {
		retry(fmt.Errorf("入素材库失败: %w", err))
		return
	}
	if !stillMine() {
		return
	}
	if orphan { // 没有子任务行可换地址：入了素材库即完成
		finish(model.CopyDone, "", "done")
		return
	}
	swapped, err := f.repo.Task.SwapLibraryRef(wctx, t.ID, j.Ref, lib.RID, lib.URL)
	if err != nil {
		retry(err)
		return
	}
	if !swapped {
		f.voidCopyJob(wctx, j, attempt, t)
		return
	}
	if finish(model.CopyDone, "", "done") {
		copyLag.Observe(time.Since(j.CreatedAt).Milliseconds(), string(t.Kind))
	}
	if f.syncSoon != nil { // 任务中心尽快换成公开地址（mr:// 期间 Java 侧照样现签）
		f.syncSoon()
	}
	if t.Deleted != 0 { // 已删除：蒙版没人看，补它要付网关费用
		return
	}
	t.ResultURL.String = lib.URL
	f.attachMaskAsync(t, b.UserID, lib.URL, tryonMaskParts(t.Kind, b.Payload))
}

// maskAfterCopy 补服装蒙版：转存永久失败时用 mr:// 引用（maskSourceURL 现签干净短期地址只给网关）；
// 换地址后中断再收尾时用素材库地址（与正常完成同一缓存键，不重复付费）。已删除的子任务不补。
func (f *flow) maskAfterCopy(ctx context.Context, t *model.Task, ref string) {
	if t == nil || t.Deleted != 0 {
		return
	}
	b, err := f.repo.Batch.GetByID(ctx, t.BatchID, 0)
	if err != nil || b == nil {
		return
	}
	f.attachMaskAsync(t, b.UserID, ref, tryonMaskParts(t.Kind, b.Payload))
}

// copyErrText 失败原因（写 last_error / 日志用，nil 为空串）。
func copyErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------- 转存池 ----------------

// copier 本进程的转存执行池：Kick（刚置成功的子任务）与扫描接手（到点的 outbox 行）共用 Library.Workers 个名额。
type copier struct {
	f        *flow
	ctx      context.Context
	slots    chan struct{}
	inflight sync.Map // 子任务主键 → 本进程已在做（本地去重；跨实例互斥靠 attempt CAS）
	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
}

func newCopier(ctx context.Context, f *flow) *copier {
	n := f.cfg.Library.Workers
	if n <= 0 {
		n = 8
	}
	return &copier{f: f, ctx: ctx, slots: make(chan struct{}, n)}
}

// spawn 占名额起协程执行 fn；已在做 / 名额满 / 已关闭返回 false。
func (p *copier) spawn(taskPK int64, fn func(ctx context.Context)) bool {
	if p == nil || taskPK <= 0 {
		return false
	}
	if _, loaded := p.inflight.LoadOrStore(taskPK, struct{}{}); loaded {
		return false
	}
	select {
	case p.slots <- struct{}{}:
	default:
		p.inflight.Delete(taskPK)
		return false
	}
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		<-p.slots
		p.inflight.Delete(taskPK)
		return false
	}
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		defer p.inflight.Delete(taskPK)
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("studio: 子任务 %d 转存素材库 panic: %v", taskPK, r)
			}
		}()
		fn(p.ctx)
	}()
	return true
}

// Kick 刚置成功的子任务立即转存（按子任务主键取待转存行；没有就什么都不做）。
func (p *copier) Kick(taskPK int64) bool {
	return p.spawn(taskPK, func(ctx context.Context) {
		j, err := p.f.repo.CopyJob.GetPendingByTask(ctx, taskPK)
		if err != nil || j == nil {
			if err != nil {
				logx.WithContext(ctx).Infof("studio: 子任务 %d 即时转存取 outbox 失败（等扫描接手）: %v", taskPK, err)
			}
			return
		}
		p.f.runCopyJob(ctx, j)
	})
}

// sweep 到点的 outbox 行交给池（不阻塞扫描；名额满就留到下一轮）。
func (p *copier) sweep(ctx context.Context) {
	if p == nil || p.f.media == nil { // 没配媒体服务的实例不接手（runCopyJob 同样跳过），留给配了的实例
		return
	}
	n := p.f.cfg.Library.SweepBatch
	if n <= 0 {
		n = 50
	}
	list, err := p.f.repo.CopyJob.ListDue(ctx, n)
	if err != nil {
		if !isMissingTable(err) {
			logx.WithContext(ctx).Errorf("studio: 查待转存 outbox 失败: %v", err)
		}
		return
	}
	for _, j := range list {
		j := j
		if !p.spawn(j.TaskPK, func(ctx context.Context) { p.f.runCopyJob(ctx, j) }) && len(p.slots) == cap(p.slots) {
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

// isMissingTable outbox 表还没建（Go_V1.0.30 未执行）：扫描静默跳过，落 outbox 时退回同步转存。
func isMissingTable(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1146
}

// ---------------- 读侧 ----------------

// libraryStatusOf 子任务的 library_status：成功且成品仍是 mr:// 时，按 outbox 行判 pending / failed；其它（已在素材库 /
// 免费档水印视图 / 非成功）为空。st 是该成品引用对应的 outbox 状态（没有行 = has=false）。
func libraryStatusOf(status int32, rawURL string, st int8, has bool) string {
	if status != int32(model.TaskSucceeded) || !isPendingRef(rawURL) {
		return ""
	}
	if has && (st == model.CopyFailed || st == model.CopyVoid) {
		return LibraryFailed // 作废但成品仍是 mr://（如运营重做失败后还原了成品）：同样没人再转存，按失败展示
	}
	return LibraryPending
}
