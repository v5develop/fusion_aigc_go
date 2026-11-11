package repo

import (
	"context"
	"database/sql"
	"errors"

	"aigc-local-life/app/studio/model"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// TableCopyJob 付费成品转存素材库 outbox（Go_V1.0.30）。
const TableCopyJob = "afv_studio_copy_job"

// CopyJobRepo afv_studio_copy_job（与画布 afv_canvas_copy_job 同一套协议）。时间一律用库时钟（NOW(3)）。
//
// 认领协议：attempt 是乐观锁。Claim 以 (id, status=0, attempt=旧值) 为条件把 attempt+1 并把 next_at 推到认领超时之后，
// 抢到的实例独占这一轮；之后的 Renew / Retry / Finish 都带 attempt=新值，认领超时被别的实例接手（attempt 又+1）后旧持有者的写入落空。
type CopyJobRepo struct{ db bun.IDB }

// Enqueue 落一行待转存（同一子任务同一产物已有行时什么都不做：INSERT IGNORE 撞 uk_task_src）。delaySec 后才对扫描可见，
// 留给本进程置成功后的即时转存（Kick），进程崩溃时由扫描接手。
//
// j.BillingRequestID 写进 billing_request_id（Go_V1.0.31）：这张成品的预留单，成品已换时据此判断旧成品是否付过费。
// 列还不存在（迁移未执行）时去掉这一列重写一次——MySQL 的 1054 只回滚这一条语句、不影响所在事务，行为退回改造前。
func (r *CopyJobRepo) Enqueue(ctx context.Context, j *model.CopyJob, delaySec int) error {
	insert := func(withBill bool) error {
		row := &model.CopyJob{TaskPK: j.TaskPK, TaskID: j.TaskID, UserID: j.UserID, SrcRid: j.SrcRid, Ref: j.Ref, Status: model.CopyPending}
		q := r.db.NewInsert().Model(row).
			ExcludeColumn("id", "created_at", "updated_at", "finished_at", "last_error").
			Value("next_at", "NOW(3) + INTERVAL ? SECOND", delaySec)
		if withBill {
			q = q.Value("billing_request_id", "?", j.BillingRequestID)
		}
		_, err := q.Ignore().Exec(ctx)
		return err
	}
	withBill := j.BillingRequestID != ""
	err := insert(withBill)
	if err != nil && withBill && IsUnknownColumn(err) {
		err = insert(false)
	}
	return err
}

// BillingRequestID 行的 billing_request_id（没有 / 列还不存在返回 "", nil）。只在成品已不是本行引用时读，
// 常规路径不碰这一列，迁移未执行时扫描 / 认领照常工作。
func (r *CopyJobRepo) BillingRequestID(ctx context.Context, id int64) (string, error) {
	var v sql.NullString
	err := r.db.NewSelect().Table(TableCopyJob).Column("billing_request_id").Where("id = ?", id).Limit(1).Scan(ctx, &v)
	switch {
	case errors.Is(err, sql.ErrNoRows), IsUnknownColumn(err):
		return "", nil
	case err != nil:
		return "", err
	}
	return v.String, nil
}

// HasNewerJob 同一子任务是否有比 id 更晚落的 outbox 行（任何状态）。一轮交付最多落一行（uk_task_src），行按交付先后递增，
// 有更晚的行 = 本行不是子任务最近一轮交付的成品（用于 billing_request_id 为空的旧行判定「成品已换」）。
func (r *CopyJobRepo) HasNewerJob(ctx context.Context, taskPK, id int64) (bool, error) {
	n, err := r.db.NewSelect().Table(TableCopyJob).Where("task_pk = ?", taskPK).Where("id > ?", id).Limit(1).Count(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// IsUnknownColumn MySQL 1054（列不存在：新增列的迁移还没执行）。
func IsUnknownColumn(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1054
}

// GetPendingByTask 子任务的待转存行（没有返回 nil, nil）。
func (r *CopyJobRepo) GetPendingByTask(ctx context.Context, taskPK int64) (*model.CopyJob, error) {
	x := new(model.CopyJob)
	return noRows(x, r.db.NewSelect().Model(x).Where("j.task_pk = ?", taskPK).Where("j.status = ?", model.CopyPending).
		OrderExpr("j.id DESC").Limit(1).Scan(ctx))
}

// ListDue 到点的待转存行（next_at 升序）。
func (r *CopyJobRepo) ListDue(ctx context.Context, limit int) ([]*model.CopyJob, error) {
	var list []*model.CopyJob
	err := r.db.NewSelect().Model(&list).Where("j.status = ?", model.CopyPending).Where("j.next_at <= NOW(3)").
		OrderExpr("j.next_at ASC, j.id ASC").Limit(int64(max(1, limit))).Scan(ctx)
	return list, err
}

// Claim 认领一轮：attempt CAS +1，next_at 推到 leaseSec 之后（本轮没在这段时间内写回结果就会被扫描接手）。
func (r *CopyJobRepo) Claim(ctx context.Context, id int64, attempt int32, leaseSec int) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCopyJob).
		Set("attempt = attempt + 1").Set("next_at = NOW(3) + INTERVAL ? SECOND", leaseSec).
		Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

// Renew 确认仍持有本轮认领并续到 leaseSec 之后；false = 认领已被接手（或已进终态），调用方不应再写。
// next_at 每次至少推后 1ms：MySQL 影响行数只算值真变了的行，同一毫秒内连续续期写入同值会被误判成「认领已丢」。
func (r *CopyJobRepo) Renew(ctx context.Context, id int64, attempt int32, leaseSec int) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCopyJob).
		Set("next_at = GREATEST(NOW(3) + INTERVAL ? SECOND, next_at + INTERVAL 1000 MICROSECOND)", leaseSec).
		Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

// Retry delaySec 后再试（只对 attempt 仍是给定值的行生效：认领后的失败传新值；没认领就推迟传原值，不占尝试次数）。
func (r *CopyJobRepo) Retry(ctx context.Context, id int64, attempt int32, delaySec int, reason string) error {
	_, err := r.db.NewUpdate().Table(TableCopyJob).
		Set("next_at = NOW(3) + INTERVAL ? SECOND", delaySec).Set("last_error = ?", Trunc(reason, 255)).
		Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx)
	return err
}

// Finish 进终态（完成 / 永久失败 / 作废；只对 attempt 仍是给定值的待转存行生效）。返回是否写到。
func (r *CopyJobRepo) Finish(ctx context.Context, id int64, attempt int32, status int8, reason string) (bool, error) {
	q := r.db.NewUpdate().Table(TableCopyJob).Set("status = ?", status).Set("finished_at = NOW(3)")
	if reason != "" {
		q = q.Set("last_error = ?", Trunc(reason, 255))
	}
	n, err := affected(q.Where("id = ?", id).Where("status = ?", model.CopyPending).Where("attempt = ?", attempt).Exec(ctx))
	return n > 0, err
}

// copyState 一张产物的转存状态（StatusByRefs 用）。
type copyState struct {
	TaskPK int64  `bun:"task_pk"`
	Ref    string `bun:"ref"`
	Status int8   `bun:"status"`
}

// StatusByRefs 子任务主键 → 当前成品引用对应的转存状态（只认 ref 与给定值相同的行；没有行的不在结果里）。一次 IN 查询。
func (r *CopyJobRepo) StatusByRefs(ctx context.Context, refs map[int64]string) (map[int64]int8, error) {
	out := map[int64]int8{}
	if len(refs) == 0 {
		return out, nil
	}
	pks := make([]int64, 0, len(refs))
	for pk := range refs {
		pks = append(pks, pk)
	}
	var rows []copyState
	err := r.db.NewSelect().Table(TableCopyJob).Column("task_pk", "ref", "status").
		Where("task_pk IN (?)", bun.In(pks)).Scan(ctx, &rows)
	for _, row := range rows {
		if refs[row.TaskPK] == row.Ref {
			out[row.TaskPK] = row.Status
		}
	}
	return out, err
}

// SwapLibraryRef 转存完成：成功且 result_url 仍是 oldRef 的子任务，成品换成素材库公有地址（CAS）。返回是否改到。
// 不看 deleted：用户删了批次也照常入素材库，已软删的行一并换成公有地址（用户看不到，无害）。
// 没改到 = 期间被重新生成 / 运营重做，原引用已不是它的成品，不再改。
func (r *TaskRepo) SwapLibraryRef(ctx context.Context, id int64, oldRef, rid, url string) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableTask).
		Set("result_rid = ?", rid).Set("result_url = ?", url).
		Where("id = ?", id).Where("status = ?", model.TaskSucceeded).Where("result_url = ?", oldRef).Exec(ctx))
	return n > 0, err
}

// LatestByRef 子任务当前成品引用对应的 outbox 行（ref 相同的最新一行；没有返回 nil, nil）。
func (r *CopyJobRepo) LatestByRef(ctx context.Context, taskPK int64, ref string) (*model.CopyJob, error) {
	x := new(model.CopyJob)
	return noRows(x, r.db.NewSelect().Model(x).Where("j.task_pk = ?", taskPK).Where("j.ref = ?", ref).
		OrderExpr("j.id DESC").Limit(1).Scan(ctx))
}

// CopyAttemptEpoch 用户重试转存时 attempt 跳到的代际步长：ResetFailed 把 attempt 推到下一个整千（1000、2000…）而不是清零。
// 认领超时被接手的旧持有者手里还拿着旧 attempt，若重置为 0、新一轮又数回同一个值，旧持有者的 Renew / Finish / 换地址会重新
// 「对上号」与新持有者并行写（素材可能入两条）；attempt 只增不减就永远对不上。本代第几次用 CopyRound 取，退避与 MaxAttempts 按它算。
const CopyAttemptEpoch int32 = 1000

// CopyRound 本代第几次尝试（1 起；未认领为 0）。
func CopyRound(attempt int32) int32 { return attempt % CopyAttemptEpoch }

// ResetFailed 用户重试转存：永久失败 / 作废的行重置为待转存（attempt 推到下一代 = 下一个整千、next_at=NOW(3) 立即可被认领、
// finished_at 清空；last_error 保留供排障）。调用方须已确认子任务仍是成功态、成品仍是本行的 ref（作废的行只有这种情况才会展示为 failed）。
// 冷却：上次进终态不足 cooldownSec 秒的不重置。返回是否改到（false = 不是终态失败 / 冷却中 / 并发的另一次已重置）。
func (r *CopyJobRepo) ResetFailed(ctx context.Context, id int64, cooldownSec int) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableCopyJob).
		Set("status = ?", model.CopyPending).Set("attempt = (attempt DIV ? + 1) * ?", CopyAttemptEpoch, CopyAttemptEpoch).
		Set("next_at = NOW(3)").Set("finished_at = NULL").
		Where("id = ?", id).Where("status IN (?, ?)", model.CopyFailed, model.CopyVoid).
		Where("(finished_at IS NULL OR finished_at <= NOW(3) - INTERVAL ? SECOND)", cooldownSec).Exec(ctx))
	return n > 0, err
}
