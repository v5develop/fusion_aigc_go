package repo

import (
	"context"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"

	"github.com/uptrace/bun"
)

var _ core.BatchStore = (*Repo)(nil)

// 批量做多条的批次读写（Go_V1.0.54）。

// InsertBatch 新建批次头；(user_id, request_id) 撞键返回已有那条。
func (r *Repo) InsertBatch(ctx context.Context, b *model.Batch) (*model.Batch, bool, error) {
	_, err := r.db.NewInsert().Model(b).ExcludeColumn("id", "created_at", "updated_at", "fail_reason").Exec(ctx)
	if err == nil {
		return b, true, nil
	}
	if !isDup(err) {
		return nil, false, err
	}
	got, err := r.GetBatchByRequest(ctx, b.UserID, b.RequestID)
	if err != nil {
		return nil, false, err
	}
	return got, false, nil
}

func (r *Repo) GetBatch(ctx context.Context, userID, id int64) (*model.Batch, error) {
	var b model.Batch
	return noRows(&b, r.db.NewSelect().Model(&b).Where("b.id = ? AND b.user_id = ?", id, userID).Limit(1).Scan(ctx))
}

func (r *Repo) GetBatchByRequest(ctx context.Context, userID int64, requestID string) (*model.Batch, error) {
	var b model.Batch
	return noRows(&b, r.db.NewSelect().Model(&b).Where("b.user_id = ? AND b.request_id = ?", userID, requestID).Limit(1).Scan(ctx))
}

// UpdateBatch 状态 CAS；总是把 updated_at 刷成现在（提交途中当心跳，回放据此判断「提交中」是否卡死）。
func (r *Repo) UpdateBatch(ctx context.Context, id int64, from []int8, set map[string]any) (bool, error) {
	q := r.db.NewUpdate().Table(model.TableBatch).Where("id = ?", id).Where("status IN (?)", bun.In(from)).Set("updated_at = NOW(3)")
	for col, v := range set {
		q = q.Set(col+" = ?", v)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AddBatchJob 登记批次 → 任务；(batch_id, seq) 或 job_id 撞键忽略（重放安全）。
func (r *Repo) AddBatchJob(ctx context.Context, bj *model.BatchJob) error {
	_, err := r.db.NewInsert().Model(bj).ExcludeColumn("id", "created_at").Ignore().Exec(ctx)
	return err
}

func (r *Repo) ListBatchJobs(ctx context.Context, batchID int64) ([]*model.BatchJob, error) {
	var out []*model.BatchJob
	err := r.db.NewSelect().Model(&out).Where("bj.batch_id = ?", batchID).OrderExpr("bj.seq").Scan(ctx)
	return out, err
}

func (r *Repo) BatchIDsOfJobs(ctx context.Context, jobIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	var rows []*model.BatchJob
	if err := r.db.NewSelect().Model(&rows).Column("batch_id", "job_id").Where("bj.job_id IN (?)", bun.In(jobIDs)).Scan(ctx); err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.JobID] = x.BatchID
	}
	return out, nil
}
