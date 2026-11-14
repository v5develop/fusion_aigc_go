package asset

// 付费产物入库对账（BACKLOG「付费产物入库可靠性」对账扫描）：各服务一个轻量 job，挂在各自的后台推进器（Poller）里，
// 每 IntervalSeconds 扫一次近 WindowHours 小时「成功且已确认扣费」的任务（由服务的 List 给出，按 (结束时间, 主键) 键集分页，
// 走各表的 (update_time|updated_at, id) 索引），
// 用 ExistingSources 查哪些 source_task_id 在 afv_asset 还没有任何行（含用户已删的——删了的不补），交给服务的 Fix 幂等补入
// （Fix 内部走 InsertSetOnceTx / InsertSetOnce 等锁定读查重的写法，与收尾 / outbox 并发也只入一次）。
//
// 只看 GraceSeconds 之前结束的任务：刚置完成的留给收尾 / outbox 自己入库，不抢。
// 多实例：每个实例各扫各的，正确性靠 Fix 的锁定读幂等（并发补同一组只会写一组），重复扫描只是多几次只读查询。
//
// 日志判据（ERROR 才告警）：
//   - ASSET_RECONCILE_MISSING   INFO  发现缺行（随后补入）
//   - ASSET_RECONCILE_INSERTED  INFO  已补入
//   - ASSET_RECONCILE_FAILED    ERROR 补入失败（下次扫描重试；连续出现需人工看）
//   - ASSET_RECONCILE_NO_SOURCE ERROR 成功且已扣费却没有 source_task_id（无从幂等入库，需人工核对）
//   - ASSET_RECONCILE_PENDING   ERROR 收尾时入库失败、已退回只置完成（FinishWithAssets / 同步路径），等本扫描补入
//
// 指标：asset_reconcile_total{service, result=scanned|missing|inserted|failed|no_source|pending}。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// 对账日志判据。
const (
	CheckReconcileMissing  = "ASSET_RECONCILE_MISSING"
	CheckReconcileInserted = "ASSET_RECONCILE_INSERTED"
	CheckReconcileFailed   = "ASSET_RECONCILE_FAILED"
	CheckReconcileNoSource = "ASSET_RECONCILE_NO_SOURCE"
	CheckReconcilePending  = "ASSET_RECONCILE_PENDING"
)

var reconcileTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "asset",
	Subsystem: "reconcile",
	Name:      "total",
	Help:      "付费产物入库对账：scanned 扫过 / missing 缺行 / inserted 补入 / failed 补入失败 / no_source 无 source_task_id / pending 收尾入库失败待补",
	Labels:    []string{"service", "result"},
})

// ReconcileConf 对账扫描配置（各服务配置里嵌一份 AssetReconcile）。
type ReconcileConf struct {
	// Enabled 是否扫描（素材库写入开关 Asset.Enabled 关闭时服务侧不会构造对账器）。
	Enabled bool `json:",default=true"`
	// IntervalSeconds 两次扫描的间隔。
	IntervalSeconds int `json:",default=600"`
	// WindowHours 只扫最近这么多小时内结束的任务（默认 7 天）。
	WindowHours int `json:",default=168"`
	// GraceSeconds 结束不足这么久的不扫（留给收尾 / outbox 自己入库）。
	GraceSeconds int `json:",default=600"`
	// Batch 每页任务数。
	Batch int `json:",default=200"`
}

// ReconcileCursor 键集分页游标：(结束时间, 主键) 之后。
type ReconcileCursor struct {
	At time.Time
	ID int64
}

// ReconcileItem 一个待核对的任务。
type ReconcileItem struct {
	ID           int64     // 任务主键（分页游标）
	At           time.Time // 结束时间（= List 过滤所用的那一列，分页游标）
	UserID       int64     //
	SourceTaskID string    // 资产行的 source_task_id；空 = 无从幂等（告警 NO_SOURCE，不补）
	Ref          any       // 服务自己的任务行，Fix 用
	Skip         bool      // 不归对账管（如产物仍是私有引用、由转存 outbox 负责）：只计入 scanned
}

// Reconciler 一个服务的对账器。
type Reconciler struct {
	Service string
	Conf    ReconcileConf
	Writer  *Writer
	// List 结束时间落在 [from, to) 内、成功且已确认扣费的任务，(结束时间, 主键) 在 after 之后、升序，最多 limit 条。
	// after.At 为零值表示从 from 开始。
	List func(ctx context.Context, from, to time.Time, after ReconcileCursor, limit int) ([]ReconcileItem, error)
	// Fix 幂等补入一个缺行的任务，返回是否写了行（false = 已被别处写过 / 无产物）。
	Fix func(ctx context.Context, it ReconcileItem) (bool, error)
	// Now 测试注入；nil 用 time.Now。
	Now func() time.Time

	mu   sync.Mutex
	last time.Time
}

// ReconcileStats 一次扫描的计数。
type ReconcileStats struct {
	Scanned, Missing, Inserted, Failed, NoSource int
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Tick 推进器每轮调用：到间隔才真正扫描（同一进程内不并发）。
func (r *Reconciler) Tick(ctx context.Context) {
	if r == nil || !r.Conf.Enabled || r.Writer == nil || r.List == nil || r.Fix == nil {
		return
	}
	interval := time.Duration(max(r.Conf.IntervalSeconds, 10)) * time.Second
	r.mu.Lock()
	if !r.last.IsZero() && r.now().Sub(r.last) < interval {
		r.mu.Unlock()
		return
	}
	r.last = r.now()
	r.mu.Unlock()
	st := r.RunOnce(ctx)
	if st.Missing > 0 || st.Failed > 0 || st.NoSource > 0 {
		logx.WithContext(ctx).Infof("asset reconcile %s: scanned=%d missing=%d inserted=%d failed=%d no_source=%d",
			r.Service, st.Scanned, st.Missing, st.Inserted, st.Failed, st.NoSource)
	}
}

// RunOnce 扫一遍窗口（不看间隔）。
func (r *Reconciler) RunOnce(ctx context.Context) ReconcileStats {
	var st ReconcileStats
	log := logx.WithContext(ctx)
	now := r.now()
	from := now.Add(-time.Duration(max(r.Conf.WindowHours, 1)) * time.Hour)
	to := now.Add(-time.Duration(max(r.Conf.GraceSeconds, 0)) * time.Second)
	batch := r.Conf.Batch
	if batch <= 0 {
		batch = 200
	}
	var after ReconcileCursor
	for ctx.Err() == nil {
		items, err := r.List(ctx, from, to, after, batch)
		if err != nil {
			log.Errorf("asset reconcile %s: 列任务失败 (check=%s): %v", r.Service, CheckReconcileFailed, err)
			return st
		}
		if len(items) == 0 {
			return st
		}
		ids := make([]string, 0, len(items))
		for _, it := range items {
			if !it.Skip {
				ids = append(ids, it.SourceTaskID)
			}
			after = ReconcileCursor{At: it.At, ID: it.ID}
		}
		st.Scanned += len(items)
		reconcileTotal.Add(float64(len(items)), r.Service, "scanned")
		have, err := r.Writer.ExistingSources(ctx, ids)
		if err != nil {
			log.Errorf("asset reconcile %s: 查资产行失败 (check=%s): %v", r.Service, CheckReconcileFailed, err)
			return st
		}
		for _, it := range items {
			if it.Skip {
				continue
			}
			if it.SourceTaskID == "" {
				st.NoSource++
				reconcileTotal.Inc(r.Service, "no_source")
				log.Errorw(fmt.Sprintf("asset reconcile %s: 任务 %d 成功且已扣费却没有 source_task_id，无从幂等入库，需人工核对 (check=%s)",
					r.Service, it.ID, CheckReconcileNoSource), logx.Field("user_id", it.UserID))
				continue
			}
			if have[it.SourceTaskID] {
				continue
			}
			st.Missing++
			reconcileTotal.Inc(r.Service, "missing")
			log.Infow(fmt.Sprintf("asset reconcile %s: 任务 %d 成功且已扣费但素材库没有对应行，补入 (check=%s)", r.Service, it.ID, CheckReconcileMissing),
				logx.Field("source_task_id", it.SourceTaskID), logx.Field("user_id", it.UserID))
			wrote, err := r.Fix(ctx, it)
			switch {
			case err != nil:
				st.Failed++
				reconcileTotal.Inc(r.Service, "failed")
				log.Errorw(fmt.Sprintf("asset reconcile %s: 任务 %d 补入素材库失败，下次扫描重试 (check=%s)", r.Service, it.ID, CheckReconcileFailed),
					logx.Field("source_task_id", it.SourceTaskID), logx.Field("user_id", it.UserID), logx.Field("err", err.Error()))
			case wrote:
				st.Inserted++
				reconcileTotal.Inc(r.Service, "inserted")
				log.Infow(fmt.Sprintf("asset reconcile %s: 任务 %d 已补入素材库 (check=%s)", r.Service, it.ID, CheckReconcileInserted),
					logx.Field("source_task_id", it.SourceTaskID), logx.Field("user_id", it.UserID))
			}
		}
		if len(items) < batch {
			return st
		}
	}
	return st
}

// LogPending 收尾时入库失败、已退回只置完成（等对账补入）：ERROR check=ASSET_RECONCILE_PENDING + 计数。
func LogPending(ctx context.Context, service string, id int64, sourceTaskID string, userID int64, err error) {
	reconcileTotal.Inc(service, "pending")
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	logx.WithContext(ctx).Errorw(fmt.Sprintf("%s: 任务 %d 入素材库失败，已照常交付，等对账扫描补入 (check=%s)", service, id, CheckReconcilePending),
		logx.Field("source_task_id", sourceTaskID), logx.Field("user_id", userID), logx.Field("err", msg))
}

// KeysetCond 对账 List 用的键集分页条件：timeCol 在 [from, to) 且 (timeCol, idCol) 在 after 之后。
// 列名是调用方给的字面量（如 "`update_time`" / "r.updated_at"）；配合 ORDER BY timeCol, idCol 走 (时间, id) 索引。
func KeysetCond(timeCol, idCol string, from, to time.Time, after ReconcileCursor) (string, []any) {
	if after.At.IsZero() {
		return timeCol + " >= ? AND " + timeCol + " < ?", []any{from, to}
	}
	return timeCol + " < ? AND (" + timeCol + " > ? OR (" + timeCol + " = ? AND " + idCol + " > ?))", []any{to, after.At, after.At, after.ID}
}
