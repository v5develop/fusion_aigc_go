package repo

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/bundb"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var ctx = context.Background()

func newMock(t *testing.T) (*Repo, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = sdb.Close()
	})
	return New(bun.NewDB(sdb, mysqldialect.New())), mock
}

func q(s string) string { return regexp.QuoteMeta(s) }

func TestUpdateStepCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_step` AS `s` SET `finished_at` = FROM_UNIXTIME(1763000000123 / 1000), `status` = 'SUCCEEDED' WHERE (id = 5 AND status = 'DELIVERING')")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err := r.UpdateStep(ctx, 5, model.StepDelivering, map[string]any{"status": model.StepSucceeded, "finished_at": time.UnixMilli(1763000000123)})
	if err != nil || ok {
		t.Fatalf("RowsAffected=0 应返回没抢到：ok=%v err=%v", ok, err)
	}
}

func TestTransitJobAndClaimContinue(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_job` AS `j` SET `finished_at` = NULL, `redo_count` = 2, `status` = 1, `tc_updated_at` = NOW(3) WHERE (id = 9 AND status IN (3, 5))")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	ok, err := r.TransitJob(ctx, 9, []int8{model.JobFailed, model.JobPartial}, model.JobRunning, map[string]any{"redo_count": 2, "finished_at": nil})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_job` SET status = 8, continue_attempt = continue_attempt + 1, tc_updated_at = NOW(3) WHERE (id = 9 AND status = 7 AND continue_attempt = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.ClaimContinue(ctx, 9, 0); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestConfirmAndReleasePlan(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_plan` SET status = 'CONFIRMED', job_id = 3 WHERE (id = 1 AND status = 'READY')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, _ := r.ConfirmPlan(ctx, 1, 3); !ok {
		t.Fatal("应抢到")
	}
	mock.ExpectBegin()
	mock.ExpectExec(q("UPDATE `afv_replicate_plan` AS p LEFT JOIN (SELECT MAX(c.id) AS cid FROM `afv_replicate_plan` AS c WHERE c.parent_plan_id = 1) AS k ON 1 = 1 SET p.status = IF(k.cid IS NULL, 'READY', 'STALE'), p.superseded_by = k.cid, p.job_id = NULL WHERE p.id = 1 AND p.status = 'CONFIRMED' AND p.job_id = 3")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("UPDATE `afv_replicate_plan` AS p JOIN (SELECT MAX(c.id) AS cid FROM `afv_replicate_plan` AS c WHERE c.parent_plan_id = 1 AND c.status = 'READY') AS k ON k.cid IS NOT NULL SET p.status = 'STALE', p.superseded_by = k.cid WHERE p.parent_plan_id = 1 AND p.status = 'READY' AND p.id < k.cid")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if ok, _ := r.ReleasePlan(ctx, 1, 3); !ok {
		t.Fatal("应释放")
	}
}

func TestInsertPlanExpiresByDBClockAndDup(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(`INSERT INTO .afv_replicate_plan.*FROM_UNIXTIME\(1763000000000 / 1000\)`).
		WillReturnError(&mysql.MySQLError{Number: 1062})
	mock.ExpectQuery(q("WHERE (p.user_id = 7 AND p.plan_hash = 'h')")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_no", "plan_hash"}).AddRow(4, "VRPx", "h"))
	got, created, err := r.InsertPlan(ctx, &model.Plan{UserID: 7, PlanHash: "h", ExpiresAt: time.UnixMilli(1763000000000), Annotations: []byte(`{}`), Segments: []byte(`[]`), Prompts: []byte(`[]`)})
	if err != nil || created || got.ID != 4 {
		t.Fatalf("撞唯一键应返回已有方案：%+v created=%v err=%v", got, created, err)
	}
}

// H1：自动重试只按 id + status CAS，不带 lease_owner（重启后实例标识会变）。
func TestAddRetryStepStatusCASOnly(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(q("UPDATE `afv_replicate_step` AS `s` SET `error_code` = 'VENDOR_FAILED', `status` = 'FAILED' WHERE (id = 11 AND status = 'RUNNING')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO `afv_replicate_step`").WillReturnResult(sqlmock.NewResult(12, 1))
	mock.ExpectCommit()
	ok, err := r.AddRetryStep(ctx, &model.Step{ID: 11}, map[string]any{"status": model.StepFailed, "error_code": "VENDOR_FAILED"}, &model.Step{StepKey: "x#r1"})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(q("WHERE (id = 11 AND status = 'RUNNING')")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if ok, err := r.AddRetryStep(ctx, &model.Step{ID: 11}, map[string]any{"status": model.StepFailed}, &model.Step{StepKey: "x#r1"}); err != nil || ok {
		t.Fatalf("已不是 RUNNING 时不建重试：ok=%v err=%v", ok, err)
	}
}

func TestFinishJobNotExistsOpenSteps(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_job` AS `j` SET `status` = 3, `tc_updated_at` = NOW(3) WHERE (id = 9 AND status IN (0, 1)) AND (NOT EXISTS (SELECT 1 FROM `afv_replicate_step` AS st WHERE st.job_id = 9 AND st.kind = 'gateway' AND st.seg_index <= 3 AND st.status IN ('HOLDING', 'PENDING', 'RUNNING', 'DELIVERING')))")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	ok, err := r.FinishJob(ctx, 9, []int8{model.JobQueued, model.JobRunning}, model.JobFailed, 3, nil)
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestRedoCounterTouchAndReschedule(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_job` SET redo_count = redo_count + 1, status = 1, finished_at = NULL, next_run_at = NULL, tc_updated_at = NOW(3) WHERE (id = 9 AND redo_count < 10 AND status IN (0, 1, 3, 5))")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.ClaimRedo(ctx, 9, 10, []int8{0, 1, 3, 5}); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_job` SET redo_count = redo_count - 1 WHERE (id = 9 AND redo_count > 0)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.ReleaseRedo(ctx, 9); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_job` SET updated_at = NOW(3) WHERE (id = 9 AND status IN (6))")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if ok, _ := r.TouchJob(ctx, 9, []int8{6}); ok {
		t.Fatal("已不在提交中应返回 false")
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_job` SET `next_run_at` = DATE_ADD(NOW(3), INTERVAL 3000000 MICROSECOND), `updated_at` = `updated_at` WHERE `id` = 9")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.RescheduleJob(ctx, 9, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_plan` SET expires_at = FROM_UNIXTIME(1763000000000 / 1000), quote = '{}' WHERE (id = 4 AND status = 'READY')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.RefreshPlan(ctx, 4, time.UnixMilli(1763000000000), []byte(`{}`)); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestCreateJobDupReturnsExisting(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO `afv_replicate_job`").WillReturnError(&mysql.MySQLError{Number: 1062})
	mock.ExpectRollback()
	mock.ExpectQuery(q("WHERE (j.user_id = 7 AND j.request_id = 'k1')")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "request_id"}).AddRow(42, 7, "k1"))
	got, created, err := r.CreateJob(ctx, &model.Job{UserID: 7, RequestID: "k1", Input: []byte(`{}`)}, nil)
	if err != nil || created || got.ID != 42 {
		t.Fatalf("同键应返回已有 job：%+v %v %v", got, created, err)
	}
}

// 集成：REPLICATE_TEST_DSN 指向已执行 Go_V1.0.37 的临时库（库名含 scratch / test）。
func integrationRepo(t *testing.T) (*Repo, *bun.DB) {
	t.Helper()
	dsn := os.Getenv("REPLICATE_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 REPLICATE_TEST_DSN，跳过")
	}
	if !strings.Contains(dsn, "scratch") && !strings.Contains(dsn, "test") {
		t.Fatalf("REPLICATE_TEST_DSN 必须指向临时库：%s", dsn)
	}
	db, err := bundb.New(sqlx.NewMysql(dsn))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = db.ExecContext(ctx, "DELETE c FROM afv_replicate_charge c JOIN afv_replicate_job j ON j.id = c.job_id WHERE j.user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE s FROM afv_replicate_step s JOIN afv_replicate_job j ON j.id = s.job_id WHERE j.user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_job WHERE user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_analysis WHERE user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_plan WHERE user_id = 990077")
	}
	clean()
	t.Cleanup(clean)
	return New(db), db
}

func insJob(t *testing.T, db *bun.DB, req string, status int8, extra string) int64 {
	t.Helper()
	cols, vals := "", ""
	if extra != "" {
		kv := strings.SplitN(extra, "=", 2)
		cols, vals = ", "+kv[0], ", "+kv[1]
	}
	res, err := db.ExecContext(ctx, "INSERT INTO afv_replicate_job (user_id, request_id, plan_id, plan_no, plan_hash, status, input, seg_total"+cols+
		") VALUES (990077, ?, 1, 'p', 'h', ?, '{}', 1"+vals+")", req, status)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// H2 / H3：DueJobs 取数口径与 next_run_at 轮转。
func TestIntegrationDueJobs(t *testing.T) {
	r, db := integrationRepo(t)
	running := insJob(t, db, "r1", model.JobRunning, "")
	fresh := insJob(t, db, "r2", model.JobSucceeded, "finished_at=NOW()")
	stale := insJob(t, db, "r3", model.JobSucceeded, "finished_at=NOW() - INTERVAL 2 DAY")
	retry := insJob(t, db, "r4", model.JobPartial, "finished_at=NOW() - INTERVAL 3 DAY")
	_, _ = db.ExecContext(ctx, `UPDATE afv_replicate_job SET result_document = '{"status":"retry"}' WHERE id = ?`, retry)
	oldRetry := insJob(t, db, "r11", model.JobPartial, "finished_at=NOW() - INTERVAL 8 DAY") // 合成待重试只看 7 天内结束的
	_, _ = db.ExecContext(ctx, `UPDATE afv_replicate_job SET result_document = '{"status":"retry"}' WHERE id = ?`, oldRetry)
	ready := insJob(t, db, "r5", model.JobSucceeded, "finished_at=NOW()")
	_, _ = db.ExecContext(ctx, `UPDATE afv_replicate_job SET result_document = '{"status":"ready"}' WHERE id = ?`, ready)
	failed := insJob(t, db, "r6", model.JobFailed, "finished_at=NOW()")
	waitingFuture := insJob(t, db, "r7", model.JobWaiting, "continue_deadline=NOW() + INTERVAL 1 HOUR")
	waitingDue := insJob(t, db, "r8", model.JobWaiting, "continue_deadline=NOW() - INTERVAL 1 MINUTE")
	refundPending := insJob(t, db, "r9", model.JobFailed, "finished_at=NOW() - INTERVAL 3 DAY")
	later := insJob(t, db, "r10", model.JobQueued, "next_run_at=NOW(3) + INTERVAL 1 HOUR")
	if _, err := db.ExecContext(ctx, "INSERT INTO afv_replicate_charge (job_id, user_id, seg_index, stage, request_id, seconds, status) VALUES (?, 990077, 1, 'all', ?, 5, 6)",
		refundPending, fmt.Sprintf("replicate-it-%d", refundPending)); err != nil {
		t.Fatal(err)
	}
	ids, err := r.DueJobs(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := map[int64]bool{}
	for _, id := range ids {
		in[id] = true
	}
	want := map[int64]bool{running: true, fresh: true, retry: true, waitingDue: true, refundPending: true,
		stale: false, ready: false, failed: false, waitingFuture: false, later: false, oldRetry: false}
	for id, w := range want {
		if in[id] != w {
			t.Fatalf("job %d 取数应为 %v：%v", id, w, ids)
		}
	}
	// 推进过的排到后面：running 写回 next_run_at 后不在本轮
	if err := r.RescheduleJob(ctx, running, time.Hour); err != nil {
		t.Fatal(err)
	}
	var upd1, upd2 time.Time
	_ = db.NewSelect().Table(model.TableJob).Column("updated_at").Where("id = ?", fresh).Scan(ctx, &upd1)
	_ = r.RescheduleJob(ctx, fresh, 0)
	_ = db.NewSelect().Table(model.TableJob).Column("updated_at").Where("id = ?", fresh).Scan(ctx, &upd2)
	if !upd1.Equal(upd2) {
		t.Fatalf("写 next_run_at 不得改 updated_at（提交心跳依赖它）：%v → %v", upd1, upd2)
	}
	ids, _ = r.DueJobs(ctx, 1000)
	for _, id := range ids {
		if id == running {
			t.Fatal("next_run_at 未到不应再取")
		}
	}
	if ids[len(ids)-1] != fresh {
		t.Fatalf("刚写回 next_run_at 的应排在 NULL 之后：%v", ids)
	}
}

// H1 / M4 / L1 / L4 / M2：条件更新在真实 MySQL 上的语义。
func TestIntegrationConditionalUpdates(t *testing.T) {
	r, db := integrationRepo(t)
	id := insJob(t, db, "c1", model.JobRunning, "")
	step := func(key, status string, seg int, owner string) int64 {
		res, err := db.ExecContext(ctx, "INSERT INTO afv_replicate_step (job_id, step_key, kind, seg_index, status, idem_key, lease_owner) VALUES (?, ?, 'gateway', ?, ?, ?, ?)",
			id, key, seg, status, fmt.Sprintf("it-%d:%s", id, key), owner)
		if err != nil {
			t.Fatal(err)
		}
		x, _ := res.LastInsertId()
		return x
	}
	s1 := step("seg/01/generate", model.StepRunning, 1, "old-pod")
	ok, err := r.AddRetryStep(ctx, &model.Step{ID: s1}, map[string]any{"status": model.StepFailed},
		&model.Step{JobID: id, StepKey: "seg/01/generate#r1", Kind: model.StepKindGateway, SegIndex: 1, Status: model.StepPending, IdemKey: fmt.Sprintf("it-%d:r1", id)})
	if err != nil || !ok {
		t.Fatalf("换了实例也要能建重试：ok=%v err=%v", ok, err)
	}
	// 有未终态步骤（#r1 PENDING）：不能汇总到终态
	if ok, err := r.FinishJob(ctx, id, []int8{model.JobRunning}, model.JobFailed, 1, map[string]any{"finished_at": time.Now()}); err != nil || ok {
		t.Fatalf("有未终态步骤不得终态：ok=%v err=%v", ok, err)
	}
	_, _ = db.ExecContext(ctx, "UPDATE afv_replicate_step SET status = 'FAILED' WHERE job_id = ?", id)
	step("seg/02/generate", model.StepPending, 2, "") // 段号 > maxSeg（未放行）：不挡
	if ok, err := r.FinishJob(ctx, id, []int8{model.JobRunning}, model.JobFailed, 1, nil); err != nil || !ok {
		t.Fatalf("应能终态：ok=%v err=%v", ok, err)
	}
	// 重做名额原子自增
	_, _ = db.ExecContext(ctx, "UPDATE afv_replicate_job SET redo_count = 0 WHERE id = ?", id)
	got := 0
	for i := 0; i < 3; i++ {
		if ok, _ := r.ClaimRedo(ctx, id, 2, []int8{model.JobFailed, model.JobRunning}); ok {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("上限 2 只能占 2 次：%d", got)
	}
	_ = r.ReleaseRedo(ctx, id)
	if j, _ := r.GetJob(ctx, id); j.RedoCount != 1 {
		t.Fatalf("还回后应为 1：%d", j.RedoCount)
	}
	if j, _ := r.GetJob(ctx, id); j.Status != model.JobRunning || j.FinishedAt != nil {
		t.Fatalf("ClaimRedo 应同一条语句把 job 拉回执行中：%+v", j)
	}
	// 孤儿排队步骤：job 在执行中不动；回到终态才取消（只取消挂着预留单的）
	_, _ = db.ExecContext(ctx, "UPDATE afv_replicate_step SET billing_request_id = 'rq-orphan' WHERE job_id = ? AND step_key = 'seg/02/generate'", id)
	if n, err := r.CancelOrphanSteps(ctx, id); err != nil || n != 0 {
		t.Fatalf("执行中的 job 不取消：n=%d err=%v", n, err)
	}
	_, _ = db.ExecContext(ctx, "UPDATE afv_replicate_job SET status = 3 WHERE id = ?", id)
	if n, err := r.CancelOrphanSteps(ctx, id); err != nil || n != 1 {
		t.Fatalf("终态 job 的孤儿步骤应取消：n=%d err=%v", n, err)
	}
	// 心跳
	if ok, _ := r.TouchJob(ctx, id, []int8{model.JobSubmitting}); ok {
		t.Fatal("不在提交中，心跳应失败")
	}
	// 过期方案刷新
	p, _, err := r.InsertPlan(ctx, &model.Plan{PlanNo: fmt.Sprintf("VRPit%d", id), UserID: 990077, AnalysisID: 1, Status: model.PlanReady, ModelGroup: "g",
		Annotations: []byte(`{}`), Segments: []byte(`[]`), Prompts: []byte(`[]`), PlanHash: fmt.Sprintf("%064d", id), ExpiresAt: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if ok, err := r.RefreshPlan(ctx, p.ID, exp, []byte(`{"total":1}`)); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if cur, _ := r.GetPlan(ctx, p.ID); !cur.ExpiresAt.After(time.Now()) || !strings.Contains(string(cur.Quote), "total") {
		t.Fatalf("应刷新：%+v", cur)
	}
	// 历史列表
	jobs, total, err := r.ListJobs(ctx, 990077, 0, 10)
	if err != nil || total < 1 || len(jobs) < 1 || jobs[0].ID != id {
		t.Fatalf("列表：%v total=%d", err, total)
	}
}
