package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"

	"github.com/uptrace/bun"
)

// 上线补齐（Go_V1.0.39）：方案审核结论、审核日志、入素材库发件箱、任务中心水位、全局在途计数、后台查询与审计。

var (
	_ core.ModerationStore  = (*Repo)(nil)
	_ core.ModerationLogger = (*Repo)(nil)
	_ core.DeliveryStore    = (*Repo)(nil)
	// 并发调度器（core/scheduler.go）的可选能力：带 next_run_at 的到期查询、在途调用号批量列出
	_ core.DueAtStore           = (*Repo)(nil)
	_ core.DueDeliveriesAtStore = (*Repo)(nil)
	_ core.InFlightCallStore    = (*Repo)(nil)
)

// ---------------------------------------------------------------- 审核

func (r *Repo) GetModeration(ctx context.Context, planID int64, planHash string) (*model.Moderation, error) {
	m := new(model.Moderation)
	return noRows(m, r.db.NewSelect().Model(m).Where("m.plan_id = ? AND m.plan_hash = ?", planID, planHash).Limit(1).Scan(ctx))
}

func (r *Repo) PutModeration(ctx context.Context, m *model.Moderation) error {
	_, err := r.db.NewInsert().Model(m).ExcludeColumn("id", "created_at", "updated_at").
		On("DUPLICATE KEY UPDATE status = VALUES(status), reason = VALUES(reason), detail = VALUES(detail), policy_sig = VALUES(policy_sig), attempt = VALUES(attempt), updated_at = NOW(3)").Exec(ctx)
	return err
}

// ListModerations 某方案的全部审核结论（后台详情）。
func (r *Repo) ListModerations(ctx context.Context, planID int64) ([]*model.Moderation, error) {
	var out []*model.Moderation
	err := r.db.NewSelect().Model(&out).Where("m.plan_id = ?", planID).OrderExpr("m.id DESC").Limit(20).Scan(ctx)
	return out, err
}

func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// LogModeration 写一条 afv_moderation_log（Java 表，同 Java InputModerationService / copywriting 的口径；task_id 预审阶段为空，
// request_id = replicate:<方案号> 供后台按方案关联）。调用方只告警不重试。
func (r *Repo) LogModeration(ctx context.Context, l core.ModerationLog) error {
	passed := 0
	if l.Passed {
		passed = 1
	}
	_, err := r.db.ExecContext(ctx, "INSERT INTO afv_moderation_log (task_id, user_id, biz_type, provider, passed, result_status, scene, label, "+
		"rejected_target, reason, request_content, raw_response, request_id, out_task_id) VALUES (NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.UserID, l.BizType, l.Provider, passed, nullable(l.ResultStatus), nullable(clip(l.Scene, 64)), nullable(clip(l.Label, 128)),
		nullable(clip(l.RejectedTarget, 2048)), nullable(clip(l.Reason, 1024)), nullable(l.RequestContent), nullable(l.RawResponse),
		nullable(clip(l.RequestID, 128)), nullable(clip(l.OutTaskID, 512)))
	return err
}

// ModerationLogRow afv_moderation_log 的只读映射（后台详情展示）。
type ModerationLogRow struct {
	ID             int64     `bun:"id" json:"id"`
	BizType        string    `bun:"biz_type" json:"bizType"`
	Provider       string    `bun:"provider" json:"provider"`
	Passed         int       `bun:"passed" json:"passed"`
	ResultStatus   *string   `bun:"result_status" json:"resultStatus,omitempty"`
	Scene          *string   `bun:"scene" json:"scene,omitempty"`
	Label          *string   `bun:"label" json:"label,omitempty"`
	RejectedTarget *string   `bun:"rejected_target" json:"rejectedTarget,omitempty"`
	Reason         *string   `bun:"reason" json:"reason,omitempty"`
	OutTaskID      *string   `bun:"out_task_id" json:"outTaskId,omitempty"`
	CreateTime     time.Time `bun:"create_time" json:"createTime"`
}

// ListModerationLogs 某方案的审核日志（request_id = replicate:<方案号>，最近 50 条）。
func (r *Repo) ListModerationLogs(ctx context.Context, planNo string) ([]ModerationLogRow, error) {
	var out []ModerationLogRow
	err := r.db.NewSelect().TableExpr("afv_moderation_log").
		Column("id", "biz_type", "provider", "passed", "result_status", "scene", "label", "rejected_target", "reason", "out_task_id", "create_time").
		Where("request_id = ? AND deleted = 0", "replicate:"+planNo).OrderExpr("id DESC").Limit(50).Scan(ctx, &out)
	return out, err
}

// ---------------------------------------------------------------- 入素材库发件箱

func (r *Repo) EnqueueDelivery(ctx context.Context, d *model.Delivery) (bool, error) {
	res, err := r.db.NewInsert().Model(d).
		ExcludeColumn("id", "attempts", "lease_token", "lease_until", "next_run_at", "video_url", "cover_url", "last_error", "created_at", "updated_at", "finished_at").
		On("DUPLICATE KEY UPDATE id = id").Exec(ctx)
	if err != nil {
		if isDup(err) {
			return false, nil
		}
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *Repo) ListDeliveries(ctx context.Context, jobID int64) ([]*model.Delivery, error) {
	var out []*model.Delivery
	err := r.db.NewSelect().Model(&out).Where("d.job_id = ?", jobID).OrderExpr("d.id").Scan(ctx)
	return out, err
}

func (r *Repo) DueDeliveries(ctx context.Context, limit int) ([]int64, error) {
	items, err := r.DueDeliveriesAt(ctx, limit)
	return dueIDs(items), err
}

// DueDeliveriesAt 同 DueDeliveries，带 next_run_at（core.DueDeliveriesAtStore）。
func (r *Repo) DueDeliveriesAt(ctx context.Context, limit int) ([]core.DueItem, error) {
	var rows []dueRow
	err := r.db.NewSelect().Model((*model.Delivery)(nil)).Column("d.id", "d.next_run_at").
		Where("d.status = ?", model.DeliveryPending).
		Where("(d.next_run_at IS NULL OR d.next_run_at <= NOW(3))").
		Where("(d.lease_until IS NULL OR d.lease_until < NOW(3))").
		OrderExpr("d.next_run_at IS NOT NULL, d.next_run_at, d.id").Limit(int64(limit)).Scan(ctx, &rows)
	return dueItems(rows), err
}

func (r *Repo) ClaimDelivery(ctx context.Context, id int64, token string, ttl time.Duration) (*model.Delivery, error) {
	ok, err := one(r.db.NewUpdate().Table(model.TableDelivery).
		Set("lease_token = ?", token).
		Set("lease_until = NOW(3) + INTERVAL ? MICROSECOND", ttl.Microseconds()).
		Set("attempts = attempts + 1").
		Where("id = ? AND status = ?", id, model.DeliveryPending).
		Where("(next_run_at IS NULL OR next_run_at <= NOW(3))").
		Where("(lease_until IS NULL OR lease_until < NOW(3))").Exec(ctx))
	if err != nil || !ok {
		return nil, err
	}
	d := new(model.Delivery)
	return noRows(d, r.db.NewSelect().Model(d).Where("d.id = ? AND d.lease_token = ?", id, token).Scan(ctx))
}

func (r *Repo) FinishDelivery(ctx context.Context, id int64, token string, set map[string]any, retryAfter time.Duration) (bool, error) {
	q := setAll(r.db.NewUpdate().Table(model.TableDelivery), conv(set))
	if retryAfter > 0 {
		q = q.Set("next_run_at = NOW(3) + INTERVAL ? MICROSECOND", retryAfter.Microseconds())
	}
	return one(q.Where("id = ? AND lease_token = ?", id, token).Exec(ctx))
}

func (r *Repo) RenewDelivery(ctx context.Context, id int64, token string, ttl time.Duration) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableDelivery).
		Set("lease_until = NOW(3) + INTERVAL ? MICROSECOND", ttl.Microseconds()).
		Where("id = ? AND lease_token = ? AND status = ?", id, token, model.DeliveryPending).Exec(ctx))
}

// TouchTaskCenter 刷新任务中心同步水位（显式 updated_at = updated_at：updated_at 是提交 / 继续心跳，不能动）。
func (r *Repo) TouchTaskCenter(ctx context.Context, jobID int64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE `"+model.TableJob+"` SET `tc_updated_at` = NOW(3), `updated_at` = `updated_at` WHERE `id` = ?", jobID)
	return err
}

// touchTaskCenterByCharge 预留镜像变化后刷新所属 job 的同步水位（任务中心「点数」读镜像汇总）。尽力而为：失败只影响同步时机。
func (r *Repo) touchTaskCenterByCharge(ctx context.Context, chargeID int64) {
	_, _ = r.db.ExecContext(ctx, "UPDATE `"+model.TableJob+"` SET `tc_updated_at` = NOW(3), `updated_at` = `updated_at` "+
		"WHERE `id` = (SELECT `job_id` FROM `"+model.TableCharge+"` WHERE `id` = ?)", chargeID)
}

// ---------------------------------------------------------------- 限频：全局在途

// CountInFlight 全局在途 job 数（排队 / 执行中 / 提交中 / 继续中；待继续不占生成资源，不算）。走 idx_status_next_run 的 status 前缀。
func (r *Repo) CountInFlight(ctx context.Context) (int64, error) {
	n, err := r.db.NewSelect().Model((*model.Job)(nil)).
		Where("j.status IN (?)", bun.In([]int8{model.JobQueued, model.JobRunning, model.JobSubmitting, model.JobContinuing})).Count(ctx)
	return int64(n), err
}

// ---------------------------------------------------------------- 后台

// JobFilter 后台检索条件（全部可选；时间按 created_at 左闭右开）。
type JobFilter struct {
	UserID   int64
	Statuses []int8
	From, To *time.Time
	JobID    int64
	TaskID   string
	PlanNo   string
}

// AdminListJobs 后台检索（id 倒序）。
func (r *Repo) AdminListJobs(ctx context.Context, f JobFilter, offset, limit int) ([]*model.Job, int64, error) {
	var out []*model.Job
	q := r.db.NewSelect().Model(&out)
	if f.JobID > 0 {
		q = q.Where("j.id = ?", f.JobID)
	}
	if f.TaskID != "" {
		q = q.Where("j.task_id = ?", f.TaskID)
	}
	if f.PlanNo != "" {
		q = q.Where("j.plan_no = ?", f.PlanNo)
	}
	if f.UserID > 0 {
		q = q.Where("j.user_id = ?", f.UserID)
	}
	if len(f.Statuses) > 0 {
		q = q.Where("j.status IN (?)", bun.In(f.Statuses))
	}
	if f.From != nil {
		q = q.Where("j.created_at >= FROM_UNIXTIME(? / 1000)", f.From.UnixMilli())
	}
	if f.To != nil {
		q = q.Where("j.created_at < FROM_UNIXTIME(? / 1000)", f.To.UnixMilli())
	}
	n, err := q.OrderExpr("j.id DESC").Offset(int64(offset)).Limit(int64(limit)).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return out, int64(n), nil
}

// BeginAdminAction 占一条审计并认领 ttl（幂等：同 job + 动作 + 幂等键已存在则返回已有行与 created=false）。
// a.LeaseToken 由调用方生成（结果写回都带它做条件）。
func (r *Repo) BeginAdminAction(ctx context.Context, a *model.AdminAction, ttl time.Duration) (*model.AdminAction, bool, error) {
	_, err := r.db.NewInsert().Model(a).ExcludeColumn("id", "points", "result", "created_at", "updated_at").
		Value("lease_until", "NOW(3) + INTERVAL ? MICROSECOND", ttl.Microseconds()).Exec(ctx)
	if err == nil {
		return a, true, nil
	}
	if !isDup(err) {
		return nil, false, err
	}
	got, gerr := r.GetAdminAction(ctx, a.JobID, a.Action, a.RequestKey)
	if gerr != nil || got == nil {
		return nil, false, errors.Join(err, gerr)
	}
	return got, false, nil
}

// FinishAdminAction 写审计结果（RUNNING → DONE / FAILED），清认领。条件带认领令牌：被接管的旧执行者写入落空（返回 false）。
func (r *Repo) FinishAdminAction(ctx context.Context, id int64, token string, status string, points int64, result []byte) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableAdminAction).Set("status = ?", status).Set("points = ?", points).
		Set("result = ?", string(result)).Set("lease_until = NULL").Where("id = ? AND lease_token = ?", id, token).Exec(ctx))
}

// TakeOverAdminAction 同幂等键接管：FAILED，或 RUNNING 但认领已过期（处理中途崩溃）→ RUNNING + 新认领。没抢到返回 false。
func (r *Repo) TakeOverAdminAction(ctx context.Context, id int64, token string, ttl time.Duration) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableAdminAction).Set("status = ?", model.AdminActionRunning).Set("lease_token = ?", token).
		Set("lease_until = NOW(3) + INTERVAL ? MICROSECOND", ttl.Microseconds()).
		Where("id = ?", id).
		Where("(status = ? OR (status = ? AND (lease_until IS NULL OR lease_until < NOW(3))))", model.AdminActionFailed, model.AdminActionRunning).
		Exec(ctx))
}

// UpsertRefundLine 动钱前先落一条 PENDING。同动作同请求号已有：DONE 原样保留；其余（FAILED / 遗留 PENDING）重置为 PENDING，
// 刷新 预留单号 / 退前状态 / 可退点数、清错误（同键重试按当前真实状态重来）。返回落库后的行。
func (r *Repo) UpsertRefundLine(ctx context.Context, l *model.AdminRefundLine) (*model.AdminRefundLine, error) {
	if _, err := r.db.NewInsert().Model(l).ExcludeColumn("id", "points", "error", "created_at", "updated_at").
		On("DUPLICATE KEY UPDATE " +
			"reservation_no = IF(status = 'DONE', reservation_no, VALUES(reservation_no)), " +
			"before_status = IF(status = 'DONE', before_status, VALUES(before_status)), " +
			"expected_points = IF(status = 'DONE', expected_points, VALUES(expected_points)), " +
			"error = IF(status = 'DONE', error, NULL), " +
			"status = IF(status = 'DONE', status, 'PENDING')").Exec(ctx); err != nil {
		return nil, err
	}
	got := new(model.AdminRefundLine)
	err := r.db.NewSelect().Model(got).Where("rl.action_id = ? AND rl.request_id = ?", l.ActionID, l.RequestID).Limit(1).Scan(ctx)
	return got, err
}

// FinishRefundLine 写一张单的结果。条件：所属动作的认领令牌还是 token（被接管后旧执行者写入落空，返回 false）。
func (r *Repo) FinishRefundLine(ctx context.Context, id int64, token string, status string, points int64, errMsg string) (bool, error) {
	return one(r.db.ExecContext(ctx, "UPDATE `"+model.TableRefundLine+"` AS l JOIN `"+model.TableAdminAction+"` AS a ON a.id = l.action_id "+
		"SET l.status = ?, l.points = ?, l.error = ? WHERE l.id = ? AND a.lease_token = ?",
		status, points, nullable(clip(errMsg, 250)), id, token))
}

// ListRefundLines 某动作的逐单明细。
func (r *Repo) ListRefundLines(ctx context.Context, actionID int64) ([]*model.AdminRefundLine, error) {
	var out []*model.AdminRefundLine
	err := r.db.NewSelect().Model(&out).Where("rl.action_id = ?", actionID).OrderExpr("rl.id").Scan(ctx)
	return out, err
}

// ListJobRefundLines 某 job 的全部人工退点明细（后台详情）。
func (r *Repo) ListJobRefundLines(ctx context.Context, jobID int64) ([]*model.AdminRefundLine, error) {
	var out []*model.AdminRefundLine
	err := r.db.NewSelect().Model(&out).Where("rl.job_id = ?", jobID).OrderExpr("rl.id DESC").Limit(200).Scan(ctx)
	return out, err
}

// RefundLog 读计费流水（afv_points_log，Java 表，只读）：某幂等键的流水金额与描述。用来判断一笔退回是不是本动作写的
// （本动作的退回原因里带 [a<动作 id>] 标记）。没有返回 found=false。
func (r *Repo) RefundLog(ctx context.Context, userID int64, idemKey string) (int64, string, bool, error) {
	var row struct {
		Amount      int64          `bun:"amount"`
		Description sql.NullString `bun:"description"`
	}
	err := r.db.NewSelect().TableExpr("afv_points_log").Column("amount", "description").
		Where("user_id = ? AND idempotency_key = ?", userID, idemKey).Limit(1).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return row.Amount, row.Description.String, true, nil
}

// ListAdminActions 某 job 的后台操作（最近 50 条）。
func (r *Repo) ListAdminActions(ctx context.Context, jobID int64) ([]*model.AdminAction, error) {
	var out []*model.AdminAction
	err := r.db.NewSelect().Model(&out).Where("aa.job_id = ?", jobID).OrderExpr("aa.id DESC").Limit(50).Scan(ctx)
	return out, err
}

// AdminGetPlan 后台用：按 id 读方案（不按用户）。
func (r *Repo) AdminGetPlan(ctx context.Context, id int64) (*model.Plan, error) {
	p := new(model.Plan)
	err := r.db.NewSelect().Model(p).Where("p.id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// GetAdminAction 按幂等键取审计行（没有返回 nil）。
func (r *Repo) GetAdminAction(ctx context.Context, jobID int64, action, key string) (*model.AdminAction, error) {
	a := new(model.AdminAction)
	return noRows(a, r.db.NewSelect().Model(a).Where("aa.job_id = ? AND aa.action = ? AND aa.request_key = ?", jobID, action, key).Limit(1).Scan(ctx))
}

// ---------------------------------------------------------------- 业务漏斗（Go_V1.0.53）

var _ core.FunnelStore = (*Repo)(nil)

// InsertFunnelEvent INSERT IGNORE（dedupe_key 唯一）：已有同键事件不再写，返回 inserted=false。
func (r *Repo) InsertFunnelEvent(ctx context.Context, e *model.FunnelEvent) (bool, error) {
	res, err := r.db.NewInsert().Model(e).ExcludeColumn("id", "created_at").Ignore().Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FunnelChannel 同一分析 / job 上最早一条用户事件（agent / web）的渠道；没有返回空串。
// 系统推进的事件（第 1 段做完、成片交付、坏片…）据此继承用户入口的渠道。
func (r *Repo) FunnelChannel(ctx context.Context, analysisID, jobID int64) (string, error) {
	q := r.db.NewSelect().Model((*model.FunnelEvent)(nil)).Column("channel").Where("channel <> 'system'").OrderExpr("id ASC").Limit(1)
	switch {
	case jobID > 0:
		q = q.Where("job_id = ?", jobID)
	case analysisID > 0:
		q = q.Where("analysis_id = ?", analysisID)
	default:
		return "", nil
	}
	var ch string
	if err := q.Scan(ctx, &ch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return ch, nil
}
