package repo

import (
	"context"

	"aigc-local-life/app/studio/model"

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
func (r *CopyJobRepo) Enqueue(ctx context.Context, j *model.CopyJob, delaySec int) error {
	row := &model.CopyJob{TaskPK: j.TaskPK, TaskID: j.TaskID, UserID: j.UserID, SrcRid: j.SrcRid, Ref: j.Ref, Status: model.CopyPending}
	_, err := r.db.NewInsert().Model(row).
		ExcludeColumn("id", "created_at", "updated_at", "finished_at", "last_error").
		Value("next_at", "NOW(3) + INTERVAL ? SECOND", delaySec).
		Ignore().Exec(ctx)
	return err
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

// SwapLibraryRef 转存完成：成功、未删除且 result_url 仍是 oldRef 的子任务，成品换成素材库公有地址（CAS）。返回是否改到。
// 没改到 = 期间被重新生成 / 运营重做 / 删除，原引用已不是它的成品，不再改。
func (r *TaskRepo) SwapLibraryRef(ctx context.Context, id int64, oldRef, rid, url string) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableTask).
		Set("result_rid = ?", rid).Set("result_url = ?", url).
		Where("id = ?", id).Where("status = ?", model.TaskSucceeded).Where("deleted = 0").Where("result_url = ?", oldRef).Exec(ctx))
	return n > 0, err
}
