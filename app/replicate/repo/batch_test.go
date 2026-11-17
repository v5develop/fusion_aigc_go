package repo

import (
	"testing"

	"aigc-local-life/app/replicate/model"

	"github.com/DATA-DOG/go-sqlmock"
)

// 批次状态 CAS：只在状态属于 from 时改，并刷新 updated_at（回放据它判断提交中是否卡死）。
func TestUpdateBatchCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_batch` SET updated_at = NOW(3), status = 1 WHERE (id = 3) AND (status IN (0))")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := r.UpdateBatch(ctx, 3, []int8{model.BatchSubmitting}, map[string]any{"status": model.BatchSubmitted})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// 任务 → 批次号；空入参不查库。
func TestBatchIDsOfJobs(t *testing.T) {
	r, mock := newMock(t)
	if m, err := r.BatchIDsOfJobs(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("%v %v", m, err)
	}
	mock.ExpectQuery(q("FROM `afv_replicate_batch_job` AS `bj` WHERE (bj.job_id IN (5, 6))")).
		WillReturnRows(sqlmock.NewRows([]string{"batch_id", "job_id"}).AddRow(9, 5))
	m, err := r.BatchIDsOfJobs(ctx, []int64{5, 6})
	if err != nil || m[5] != 9 || len(m) != 1 {
		t.Fatalf("%v %v", m, err)
	}
}
