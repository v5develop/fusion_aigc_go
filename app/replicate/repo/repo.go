// Package repo 视频做同款业务表（afv_replicate_*）的 bun 数据访问，实现 core.Store。
//
// 约定（与 app/studio/rpc/internal/repo 一致）：
//   - 查单条找不到返回 (nil, nil)；状态迁移一律 CAS（WHERE id=? AND status=?），RowsAffected=0 表示没抢到。
//   - 时间一律交给库：set 里的 time.Time 改写成 FROM_UNIXTIME(ms/1000)（bun 方言按 UTC 内联 time.Time，与 DSN 会话时区不一致）。
//   - 只写 afv_replicate_*；计费 / 任务中心 / 素材库走 pkg/billing、pkg/taskcenter、pkg/asset。
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/lease"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// Repo 实现 core.Store。
type Repo struct{ db *bun.DB }

var _ core.Store = (*Repo)(nil)

// New 根 Repo。
func New(db *bun.DB) *Repo { return &Repo{db: db} }

func isDup(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func noRows[T any](v *T, err error) (*T, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

func msExpr(t time.Time) bun.Safe {
	return bun.Safe(fmt.Sprintf("FROM_UNIXTIME(%d / 1000)", t.UnixMilli()))
}

// conv 规范 set 的值：时间交给库换算，JSON 按文本写。
func conv(set map[string]any) map[string]any {
	out := make(map[string]any, len(set))
	for k, v := range set {
		switch x := v.(type) {
		case time.Time:
			out[k] = msExpr(x)
		case *time.Time:
			if x == nil {
				out[k] = nil
			} else {
				out[k] = msExpr(*x)
			}
		case json.RawMessage:
			out[k] = string(x)
		case []byte:
			out[k] = string(x)
		default:
			out[k] = v
		}
	}
	return out
}

func setAll(q *bun.UpdateQuery, set map[string]any) *bun.UpdateQuery {
	cols := make([]string, 0, len(set))
	for c := range set {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, c := range cols {
		q = q.Set("? = ?", bun.Ident(c), set[c])
	}
	return q
}

func one(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---------------------------------------------------------------- 分析

func (r *Repo) InsertAnalysis(ctx context.Context, a *model.Analysis) (*model.Analysis, bool, error) {
	_, err := r.db.NewInsert().Model(a).Exec(ctx)
	if err == nil {
		return a, true, nil
	}
	if !isDup(err) {
		return nil, false, err
	}
	var got model.Analysis
	err = r.db.NewSelect().Model(&got).Where("a.user_id = ? AND a.source_key = ?", a.UserID, a.SourceKey).Limit(1).Scan(ctx)
	if err != nil {
		return nil, false, err
	}
	return &got, false, nil
}

func (r *Repo) GetAnalysis(ctx context.Context, id int64) (*model.Analysis, error) {
	var a model.Analysis
	return noRows(&a, r.db.NewSelect().Model(&a).Where("a.id = ?", id).Limit(1).Scan(ctx))
}

func (r *Repo) GetAnalysisByNo(ctx context.Context, userID int64, no string) (*model.Analysis, error) {
	var a model.Analysis
	return noRows(&a, r.db.NewSelect().Model(&a).Where("a.analysis_no = ? AND a.user_id = ?", no, userID).Limit(1).Scan(ctx))
}

func (r *Repo) UpdateAnalysis(ctx context.Context, id int64, stage string, attempt int, set map[string]any) (bool, error) {
	return bundb.UpdateIf(ctx, r.db, (*model.Analysis)(nil), conv(set), func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ? AND stage = ? AND attempt = ? AND status IN (?)", id, stage, attempt,
			bun.In([]int8{model.AnalysisQueued, model.AnalysisRunning}))
	})
}

func (r *Repo) RestartAnalysis(ctx context.Context, id int64, attempt int) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableAnalysis).
		Set("status = ?", model.AnalysisQueued).Set("stage = ?", model.StageIngest).Set("attempt = attempt + 1").
		Set("state = NULL").Set("error_code = NULL").Set("error_msg = NULL").Set("finished_at = NULL").Set("next_run_at = NULL").
		Where("id = ? AND status = ? AND attempt = ?", id, model.AnalysisFailed, attempt).Exec(ctx))
}

// dueCond next_run_at 已到（NULL = 立即）。
const dueCond = "(next_run_at IS NULL OR next_run_at <= NOW(3))"

// dueOrder 到期先后：从没排过的（NULL）最先，其余按 next_run_at 升序——每轮推进后写回，推过的排到后面，不会饿死。
const dueOrder = "next_run_at IS NOT NULL, next_run_at, id"

// DueAnalyses 排队 / 进行中、租约空闲、next_run_at 已到的分析，按 next_run_at 升序。
func (r *Repo) DueAnalyses(ctx context.Context, limit int) ([]int64, error) {
	var ids []int64
	err := r.db.NewSelect().Table(model.TableAnalysis).Column("id").
		Where("status IN (?)", bun.In([]int8{model.AnalysisQueued, model.AnalysisRunning})).Where(lease.FreeCond("")).
		Where(dueCond).OrderExpr(dueOrder).Limit(int64(limit)).Scan(ctx, &ids)
	return ids, err
}

// reschedule next_run_at = 库时钟 + delay；显式 updated_at = updated_at，不触发自动更新（updated_at 是提交 / 继续的心跳）。
func (r *Repo) reschedule(ctx context.Context, table string, id int64, delay time.Duration) error {
	_, err := r.db.ExecContext(ctx, "UPDATE `"+table+"` SET `next_run_at` = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), "+
		"`updated_at` = `updated_at` WHERE `id` = ?", delay.Microseconds(), id)
	return err
}

func (r *Repo) RescheduleAnalysis(ctx context.Context, id int64, delay time.Duration) error {
	return r.reschedule(ctx, model.TableAnalysis, id, delay)
}

// ---------------------------------------------------------------- 方案

func (r *Repo) InsertPlan(ctx context.Context, p *model.Plan) (*model.Plan, bool, error) {
	_, err := r.db.NewInsert().Model(p).Value("expires_at", string(msExpr(p.ExpiresAt))).Exec(ctx)
	if err == nil {
		return p, true, nil
	}
	if !isDup(err) {
		return nil, false, err
	}
	var got model.Plan
	if err := r.db.NewSelect().Model(&got).Where("p.user_id = ? AND p.plan_hash = ?", p.UserID, p.PlanHash).Limit(1).Scan(ctx); err != nil {
		return nil, false, err
	}
	return &got, false, nil
}

func (r *Repo) GetPlanByNo(ctx context.Context, userID int64, no string) (*model.Plan, error) {
	var p model.Plan
	return noRows(&p, r.db.NewSelect().Model(&p).Where("p.plan_no = ? AND p.user_id = ?", no, userID).Limit(1).Scan(ctx))
}

func (r *Repo) GetPlan(ctx context.Context, id int64) (*model.Plan, error) {
	var p model.Plan
	return noRows(&p, r.db.NewSelect().Model(&p).Where("p.id = ?", id).Limit(1).Scan(ctx))
}

func (r *Repo) ConfirmPlan(ctx context.Context, id, jobID int64) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TablePlan).Set("status = ?", model.PlanConfirmed).Set("job_id = ?", jobID).
		Where("id = ? AND status = ?", id, model.PlanReady).Exec(ctx))
}

func (r *Repo) RefreshPlan(ctx context.Context, id int64, expiresAt time.Time, quote []byte) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TablePlan).Set("expires_at = ?", msExpr(expiresAt)).Set("quote = ?", string(quote)).
		Where("id = ? AND status = ?", id, model.PlanReady).Exec(ctx))
}

// ReleasePlan 确认没走完时放回方案（同一事务）。占用期间（CONFIRMED）若已有改标注出的子方案（parent_plan_id = 本方案；
// 现在占用期间的改标注已一律 409，这里兜底旧数据 / 竞态），放回后原方案直接是 STALE（superseded_by = 最新子方案），否则新旧两张都能确认；
// 没有子方案才回 READY。同时把该方案下 READY 的子方案收敛为只剩最新一张（其余 STALE，superseded_by 指向它）。
// 子方案查询包一层派生表（MySQL 不允许 UPDATE 目标表出现在同表子查询里，带聚合的派生表会先物化）。
func (r *Repo) ReleasePlan(ctx context.Context, id, jobID int64) (bool, error) {
	ok := false
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		ok, err = one(tx.ExecContext(ctx, "UPDATE `"+model.TablePlan+"` AS p "+
			"LEFT JOIN (SELECT MAX(c.id) AS cid FROM `"+model.TablePlan+"` AS c WHERE c.parent_plan_id = ?) AS k ON 1 = 1 "+
			"SET p.status = IF(k.cid IS NULL, ?, ?), p.superseded_by = k.cid, p.job_id = NULL "+
			"WHERE p.id = ? AND p.status = ? AND p.job_id = ?",
			id, model.PlanReady, model.PlanStale, id, model.PlanConfirmed, jobID))
		if err != nil || !ok {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE `"+model.TablePlan+"` AS p "+
			"JOIN (SELECT MAX(c.id) AS cid FROM `"+model.TablePlan+"` AS c WHERE c.parent_plan_id = ? AND c.status = ?) AS k ON k.cid IS NOT NULL "+
			"SET p.status = ?, p.superseded_by = k.cid "+
			"WHERE p.parent_plan_id = ? AND p.status = ? AND p.id < k.cid",
			id, model.PlanReady, model.PlanStale, id, model.PlanReady)
		return err
	})
	return ok, err
}

// ---------------------------------------------------------------- 出片任务

func (r *Repo) CreateJob(ctx context.Context, j *model.Job, build core.JobBuilder) (*model.Job, bool, error) {
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(j).Exec(ctx); err != nil {
			return err
		}
		task, steps, charges := build(j.ID)
		j.TaskID = &task
		if _, err := tx.NewUpdate().Table(model.TableJob).Set("task_id = ?", task).Set("tc_updated_at = NOW(3)").Where("id = ?", j.ID).Exec(ctx); err != nil {
			return err
		}
		if len(steps) > 0 {
			if _, err := tx.NewInsert().Model(&steps).Exec(ctx); err != nil {
				return err
			}
		}
		if len(charges) > 0 {
			if _, err := tx.NewInsert().Model(&charges).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return j, true, nil
	}
	if !isDup(err) {
		return nil, false, err
	}
	got, gerr := r.GetJobByRequest(ctx, j.UserID, j.RequestID)
	if gerr != nil || got == nil {
		return nil, false, errors.Join(err, gerr)
	}
	return got, false, nil
}

func (r *Repo) GetJob(ctx context.Context, id int64) (*model.Job, error) {
	var j model.Job
	return noRows(&j, r.db.NewSelect().Model(&j).Where("j.id = ?", id).Limit(1).Scan(ctx))
}

func (r *Repo) GetJobByRequest(ctx context.Context, userID int64, requestID string) (*model.Job, error) {
	var j model.Job
	return noRows(&j, r.db.NewSelect().Model(&j).Where("j.user_id = ? AND j.request_id = ?", userID, requestID).Limit(1).Scan(ctx))
}

func (r *Repo) TransitJob(ctx context.Context, id int64, from []int8, to int8, set map[string]any) (bool, error) {
	s := conv(set)
	s["status"] = to
	s["tc_updated_at"] = tcNow // 业务状态写：刷新任务中心同步水位（Go_V1.0.39，列上没有 ON UPDATE）
	return bundb.UpdateIf(ctx, r.db, (*model.Job)(nil), s, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ? AND status IN (?)", id, bun.In(from))
	})
}

// SwapResultDoc 整份结果文档 CAS（JSON 列按值比较：CAST(? AS JSON) 与列值都规范化后比，键序 / 空白不影响）。
func (r *Repo) SwapResultDoc(ctx context.Context, id int64, old, new json.RawMessage) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableJob).Set("result_document = CAST(? AS JSON)", string(new)).Set("tc_updated_at = NOW(3)").
		Where("id = ? AND result_document = CAST(? AS JSON)", id, string(old)).Exec(ctx))
}

// tcNow 任务中心同步水位的取值（库时钟）。只在业务状态写路径设置；租约 / next_run_at / 提交心跳不碰。
var tcNow = bun.Safe("NOW(3)")

// FinishJob 同 TransitJob，另要求库里没有段号 ≤ maxSeg 的未终态出片步骤（与并发重做互斥）。
func (r *Repo) FinishJob(ctx context.Context, id int64, from []int8, to int8, maxSeg int, set map[string]any) (bool, error) {
	s := conv(set)
	s["status"] = to
	s["tc_updated_at"] = tcNow
	open := bun.In([]string{model.StepHolding, model.StepPending, model.StepRunning, model.StepDelivering})
	return bundb.UpdateIf(ctx, r.db, (*model.Job)(nil), s, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ? AND status IN (?)", id, bun.In(from)).
			Where("NOT EXISTS (SELECT 1 FROM `"+model.TableStep+"` AS st WHERE st.job_id = ? AND st.kind = ? AND st.seg_index <= ? AND st.status IN (?))",
				id, model.StepKindGateway, maxSeg, open)
	})
}

func (r *Repo) TouchJob(ctx context.Context, id int64, statuses []int8) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableJob).Set("updated_at = NOW(3)").
		Where("id = ? AND status IN (?)", id, bun.In(statuses)).Exec(ctx))
}

func (r *Repo) ClaimRedo(ctx context.Context, id int64, max int, statuses []int8) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableJob).Set("redo_count = redo_count + 1").
		Set("status = ?", model.JobRunning).Set("finished_at = NULL").Set("next_run_at = NULL").Set("tc_updated_at = NOW(3)").
		Where("id = ? AND redo_count < ? AND status IN (?)", id, max, bun.In(statuses)).Exec(ctx))
}

func (r *Repo) ReleaseRedo(ctx context.Context, id int64) error {
	_, err := r.db.NewUpdate().Table(model.TableJob).Set("redo_count = redo_count - 1").
		Where("id = ? AND redo_count > 0", id).Exec(ctx)
	return err
}

func (r *Repo) CancelOrphanSteps(ctx context.Context, jobID int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, "UPDATE `"+model.TableStep+"` AS s JOIN `"+model.TableJob+"` AS j ON j.id = s.job_id "+
		"SET s.status = ?, s.error_code = ?, s.finished_at = NOW(3) "+
		"WHERE s.job_id = ? AND s.kind = ? AND s.status = ? AND s.billing_request_id IS NOT NULL AND j.status IN (?, ?, ?, ?)",
		model.StepCanceled, "NOT_SUBMITTED", jobID, model.StepKindGateway, model.StepPending,
		model.JobSucceeded, model.JobFailed, model.JobCanceled, model.JobPartial)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *Repo) RescheduleJob(ctx context.Context, id int64, delay time.Duration) error {
	return r.reschedule(ctx, model.TableJob, id, delay)
}

func (r *Repo) ListJobs(ctx context.Context, userID int64, offset, limit int) ([]*model.Job, int64, error) {
	var out []*model.Job
	n, err := r.db.NewSelect().Model(&out).Where("j.user_id = ?", userID).OrderExpr("j.id DESC").
		Offset(int64(offset)).Limit(int64(limit)).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return out, int64(n), nil
}

func (r *Repo) ClaimContinue(ctx context.Context, id int64, attempt int) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableJob).Set("status = ?", model.JobContinuing).Set("continue_attempt = continue_attempt + 1").Set("tc_updated_at = NOW(3)").
		Where("id = ? AND status = ? AND continue_attempt = ?", id, model.JobWaiting, attempt).Exec(ctx))
}

func (r *Repo) ListSteps(ctx context.Context, jobID int64) ([]*model.Step, error) {
	var out []*model.Step
	err := r.db.NewSelect().Model(&out).Where("s.job_id = ?", jobID).OrderExpr("s.id").Scan(ctx)
	return out, err
}

func (r *Repo) ListCharges(ctx context.Context, jobID int64) ([]*model.Charge, error) {
	var out []*model.Charge
	err := r.db.NewSelect().Model(&out).Where("c.job_id = ?", jobID).OrderExpr("c.id").Scan(ctx)
	return out, err
}

func (r *Repo) UpdateStep(ctx context.Context, id int64, from string, set map[string]any) (bool, error) {
	return bundb.UpdateIf(ctx, r.db, (*model.Step)(nil), conv(set), func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ? AND status = ?", id, from)
	})
}

func (r *Repo) UpdateCharge(ctx context.Context, id int64, from []int8, set map[string]any) (bool, error) {
	ok, err := bundb.UpdateIf(ctx, r.db, (*model.Charge)(nil), conv(set), func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ? AND status IN (?)", id, bun.In(from))
	})
	if ok && err == nil {
		r.touchTaskCenterByCharge(ctx, id) // 任务中心「点数」读镜像汇总：镜像变了刷新同步水位
	}
	return ok, err
}

func (r *Repo) AddCharges(ctx context.Context, charges []*model.Charge, stepReq map[int64]string) error {
	return bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		if len(charges) > 0 {
			if _, err := tx.NewInsert().Model(&charges).Exec(ctx); err != nil {
				return err
			}
		}
		ids := make([]int64, 0, len(stepReq))
		for id := range stepReq {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		for _, id := range ids {
			if _, err := tx.NewUpdate().Table(model.TableStep).Set("billing_request_id = ?", stepReq[id]).
				Where("id = ? AND status = ?", id, model.StepPending).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddRetryStep 只按 id + status CAS：lease_owner 是实例标识（主机名 + pid），重启后就变了，带它做条件会让在途段永远停在 RUNNING。
// 同一 job 同一时刻只有一个推进者由 job 行租约保证，这里的 CAS 只防重复迁移。
func (r *Repo) AddRetryStep(ctx context.Context, failed *model.Step, failSet map[string]any, retry *model.Step) (bool, error) {
	ok := false
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		ok, err = bundb.UpdateIf(ctx, tx, (*model.Step)(nil), conv(failSet), func(q *bun.UpdateQuery) *bun.UpdateQuery {
			return q.Where("id = ? AND status = ?", failed.ID, model.StepRunning)
		})
		if err != nil || !ok {
			return err
		}
		_, err = tx.NewInsert().Model(retry).Exec(ctx)
		return err
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (r *Repo) AddRedo(ctx context.Context, step *model.Step, charge *model.Charge) (*model.Charge, bool, error) {
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(charge).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewInsert().Model(step).Exec(ctx)
		return err
	})
	if err == nil {
		return nil, true, nil
	}
	if !isDup(err) || charge.ClientKey == nil {
		return nil, false, err
	}
	var got model.Charge
	if gerr := r.db.NewSelect().Model(&got).Where("c.job_id = ? AND c.client_key = ?", charge.JobID, *charge.ClientKey).Limit(1).Scan(ctx); gerr != nil {
		return nil, false, err // 撞的是步骤键（并发的另一次重做），不是同一个点击
	}
	return &got, false, nil
}

// DueJobs 需要推进的 job（租约空闲、next_run_at 已到，按 next_run_at 升序）。拆成五条各走索引的查询再在内存里合并
// （一条 OR 大查询会退化成全表扫描）：
//  1. 在途（排队 / 执行中 / 提交中 / 继续中）：idx_status_next_run (status, next_run_at)；
//  2. 待继续且 continue_deadline 已到：idx_status_deadline (status, continue_deadline)；
//     待继续且 decision_remind_at 已到（待决策提醒，Go_V1.0.56）：idx_status_remind (status, decision_remind_at)；
//  3. 任何状态下还有未结镜像（0/1/6）：先走 afv_replicate_charge.idx_status (status, updated_at) 取 job_id，再按主键回 job；
//     终态后的退点 / 扣除失败靠这条补扫；
//  4. 成功 / 部分成功里成片未合成（1 天内结束）或合成待重试（7 天内结束）：idx_status_finished (status, finished_at)。
//
// 推过的 job 由 Service 写回 next_run_at（在途 3 秒、有未结镜像 ≤2 分钟、其余按年龄退避），排到后面，limit 再小也不会饿死。
func (r *Repo) DueJobs(ctx context.Context, limit int) ([]int64, error) {
	type row struct {
		ID        int64      `bun:"id"`
		NextRunAt *time.Time `bun:"next_run_at"`
	}
	const due = "(j.next_run_at IS NULL OR j.next_run_at <= NOW(3))"
	order := "j.next_run_at IS NOT NULL, j.next_run_at, j.id"
	base := func() *bun.SelectQuery {
		return r.db.NewSelect().TableExpr("`"+model.TableJob+"` AS j").Column("j.id", "j.next_run_at").
			Where(lease.FreeCond("j")).Where(due).OrderExpr(order).Limit(int64(limit))
	}
	queries := []*bun.SelectQuery{
		base().Where("j.status IN (?)", bun.In([]int8{model.JobQueued, model.JobRunning, model.JobSubmitting, model.JobContinuing})),
		base().Where("j.status = ? AND j.continue_deadline <= NOW(3)", model.JobWaiting),
		base().Where("j.status = ? AND j.decision_remind_at IS NOT NULL AND j.decision_remind_at <= NOW(3)", model.JobWaiting), // 待决策提醒到点：idx_status_remind
		base().Where("j.id IN (SELECT c.job_id FROM `"+model.TableCharge+"` AS c WHERE c.status IN (?))", bun.In(model.ChargeOpenStatuses)),
		base().Where("j.status IN (?) AND j.finished_at > NOW() - INTERVAL 7 DAY", bun.In([]int8{model.JobSucceeded, model.JobPartial})).
			WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Where("j.result_document IS NULL AND j.finished_at > NOW() - INTERVAL 1 DAY").
					WhereOr("JSON_UNQUOTE(JSON_EXTRACT(j.result_document, '$.status')) = ?", core.ResultRetry)
			}),
	}
	seen := map[int64]bool{}
	var all []row
	for _, q := range queries {
		var rows []row
		if err := q.Scan(ctx, &rows); err != nil {
			return nil, err
		}
		for _, x := range rows {
			if !seen[x.ID] {
				seen[x.ID] = true
				all = append(all, x)
			}
		}
	}
	sort.Slice(all, func(a, b int) bool {
		na, nb := all[a].NextRunAt, all[b].NextRunAt
		if (na == nil) != (nb == nil) {
			return na == nil
		}
		if na != nil && !na.Equal(*nb) {
			return na.Before(*nb)
		}
		return all[a].ID < all[b].ID
	})
	ids := make([]int64, 0, min(len(all), limit))
	for _, x := range all {
		if len(ids) >= limit {
			break
		}
		ids = append(ids, x.ID)
	}
	return ids, nil
}

// AcquireJobLease / ReleaseJobLease Poller 用：同一 job 同一时刻只由一个实例推进。
func (r *Repo) AcquireJobLease(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	return lease.Acquire(ctx, r.db, model.TableJob, id, owner, ttl)
}

func (r *Repo) ReleaseJobLease(ctx context.Context, id int64, owner string) error {
	return lease.Release(ctx, r.db, model.TableJob, id, owner)
}

// ---------------------------------------------------------------- 偏好

func (r *Repo) GetPref(ctx context.Context, userID int64) (*model.UserPref, error) {
	var p model.UserPref
	return noRows(&p, r.db.NewSelect().Model(&p).Where("up.user_id = ?", userID).Limit(1).Scan(ctx))
}

func (r *Repo) SetAutoContinue(ctx context.Context, userID int64, on bool) error {
	return bundb.Upsert(ctx, r.db, &model.UserPref{UserID: userID, AutoContinue: on}, []string{"user_id"}, []string{"auto_continue"})
}
