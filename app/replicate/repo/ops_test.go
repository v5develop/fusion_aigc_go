package repo

import (
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/lease"
)

// 集成（REPLICATE_TEST_DSN，已执行 Go_V1.0.37 + Go_V1.0.39 的临时库）：入库发件箱 CAS、任务中心水位、审核、审计、在途计数。
func TestIntegrationDeliveryClaimCAS(t *testing.T) {
	r, db := integrationRepo(t)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_delivery WHERE user_id = 990077") })
	job := insJob(t, db, "d1", model.JobSucceeded, "")
	d := &model.Delivery{JobID: job, UserID: 990077, Kind: model.DeliveryFinal, SrcRid: "rid-1", Status: model.DeliveryPending}
	if created, err := r.EnqueueDelivery(ctx, d); err != nil || !created {
		t.Fatalf("首次入队：%v %v", created, err)
	}
	if created, err := r.EnqueueDelivery(ctx, &model.Delivery{JobID: job, UserID: 990077, Kind: model.DeliveryFinal, SrcRid: "rid-2"}); err != nil || created {
		t.Fatalf("同 job 成片只入队一次：%v %v", created, err)
	}
	ids, err := r.DueDeliveries(ctx, 10)
	if err != nil || len(ids) != 1 {
		t.Fatalf("到期：%v %v", ids, err)
	}
	// 并发认领：只有一个抢到
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := r.ClaimDelivery(ctx, ids[0], "tok"+string(rune('a'+i)), time.Minute)
			if err != nil {
				t.Error(err)
			}
			if got != nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("只能一个实例认领：%d", won)
	}
	if ids, _ := r.DueDeliveries(ctx, 10); len(ids) != 0 {
		t.Fatal("认领期内不再到期")
	}
	list, _ := r.ListDeliveries(ctx, job)
	tok := *list[0].LeaseToken
	if list[0].Attempts != 1 || list[0].SrcRid != "rid-1" {
		t.Fatalf("认领加次数、源 rid 冻结：%+v", list[0])
	}
	if ok, _ := r.FinishDelivery(ctx, list[0].ID, "stale", map[string]any{"status": model.DeliveryDone}, 0); ok {
		t.Fatal("令牌不对写入应落空")
	}
	if ok, err := r.FinishDelivery(ctx, list[0].ID, tok, map[string]any{"last_error": "x", "lease_token": nil, "lease_until": nil}, time.Hour); err != nil || !ok {
		t.Fatalf("退避写回：%v %v", ok, err)
	}
	if ids, _ := r.DueDeliveries(ctx, 10); len(ids) != 0 {
		t.Fatal("退避期内不到期")
	}
	_, _ = db.ExecContext(ctx, "UPDATE afv_replicate_delivery SET next_run_at = NOW(3) - INTERVAL 1 SECOND WHERE job_id = ?", job)
	got, err := r.ClaimDelivery(ctx, list[0].ID, "tok2", time.Minute)
	if err != nil || got == nil || got.Attempts != 2 {
		t.Fatalf("到期后再认领：%+v %v", got, err)
	}
	if ok, _ := r.FinishDelivery(ctx, got.ID, "tok2", map[string]any{"status": model.DeliveryDone, "video_url": "https://pub/x", "finished_at": time.Now()}, 0); !ok {
		t.Fatal("完成")
	}
}

// 中6：同步水位只在业务状态写路径变；租约 / 续租 / next_run_at / 提交心跳都不动它。
func TestIntegrationTaskCenterWatermark(t *testing.T) {
	r, db := integrationRepo(t)
	job := insJob(t, db, "w1", model.JobRunning, "")
	read := func() (tc, upd time.Time) {
		_ = db.QueryRowContext(ctx, "SELECT tc_updated_at, updated_at FROM afv_replicate_job WHERE id = ?", job).Scan(&tc, &upd)
		return
	}
	step := func() { time.Sleep(15 * time.Millisecond) }
	tc0, _ := read()
	step()
	_ = r.RescheduleJob(ctx, job, time.Second)
	if ok, err := lease.Acquire(ctx, db, model.TableJob, job, "pod-a", time.Minute); err != nil || !ok {
		t.Fatalf("抢租约：%v %v", ok, err)
	}
	step()
	if ok, err := lease.Extend(ctx, db, model.TableJob, job, "pod-a", time.Minute); err != nil || !ok {
		t.Fatalf("续租：%v %v", ok, err)
	}
	_ = lease.Release(ctx, db, model.TableJob, job, "pod-a")
	_, _ = r.TouchJob(ctx, job, []int8{model.JobRunning})
	if tc1, _ := read(); !tc1.Equal(tc0) {
		t.Fatalf("租约 / next_run_at / 心跳不得改水位：%v → %v", tc0, tc1)
	}
	step()
	if ok, err := r.TransitJob(ctx, job, []int8{model.JobRunning}, model.JobRunning, map[string]any{"seg_done": 1}); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	tc2, _ := read()
	if !tc2.After(tc0) {
		t.Fatalf("段数 / 状态变化要刷新水位：%v → %v", tc0, tc2)
	}
	step()
	if err := r.TouchTaskCenter(ctx, job); err != nil {
		t.Fatal(err)
	}
	tc3, _ := read()
	if !tc3.After(tc2) {
		t.Fatal("入库后显式刷新")
	}
	res, _ := db.ExecContext(ctx, "INSERT INTO afv_replicate_charge (job_id, user_id, seg_index, stage, request_id, seconds, status) VALUES (?, 990077, 1, 'all', ?, 5, 0)",
		job, "rq-wm")
	cid, _ := res.LastInsertId()
	step()
	if ok, err := r.UpdateCharge(ctx, cid, []int8{model.ChargePending}, map[string]any{"status": model.ChargeHeld, "points": 50}); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	tc4, _ := read()
	if !tc4.After(tc3) {
		t.Fatal("镜像变化刷新水位（任务中心点数）")
	}
	step()
	if ok, _ := r.FinishJob(ctx, job, []int8{model.JobRunning}, model.JobSucceeded, 1, map[string]any{"finished_at": time.Now()}); !ok {
		t.Fatal("终态")
	}
	tc5, _ := read()
	if !tc5.After(tc4) {
		t.Fatalf("终态刷新水位：%v", tc5)
	}
	if n, err := r.CountInFlight(ctx); err != nil || n < 0 {
		t.Fatalf("在途计数：%d %v", n, err)
	}
}

func TestIntegrationModerationAndAudit(t *testing.T) {
	r, db := integrationRepo(t)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_moderation WHERE user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_moderation_log WHERE user_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_admin_action WHERE admin_id = 990077")
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_admin_refund_line WHERE job_id = 5")
	})
	m := &model.Moderation{UserID: 990077, PlanID: 77, PlanNo: "VRPit", PlanHash: "h1", Status: model.ModerationRejected, Reason: "r", Detail: []byte(`{"a":1}`)}
	if err := r.PutModeration(ctx, m); err != nil {
		t.Fatal(err)
	}
	m.Status, m.Reason = model.ModerationPassed, ""
	if err := r.PutModeration(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetModeration(ctx, 77, "h1")
	if err != nil || got == nil || got.Status != model.ModerationPassed {
		t.Fatalf("同一版方案 upsert：%+v %v", got, err)
	}
	if x, _ := r.GetModeration(ctx, 77, "h2"); x != nil {
		t.Fatal("指纹变了就没有结论")
	}
	if err := r.LogModeration(ctx, core.ModerationLog{UserID: 990077, BizType: core.ModBizImage, Provider: core.ModProviderGateway,
		ResultStatus: core.ModResultRejected, Label: "porn", RequestID: "replicate:VRPit", OutTaskID: "call_1"}); err != nil {
		t.Fatal(err)
	}
	logs, err := r.ListModerationLogs(ctx, "VRPit")
	if err != nil || len(logs) != 1 || logs[0].Passed != 0 || *logs[0].OutTaskID != "call_1" {
		t.Fatalf("审核日志：%+v %v", logs, err)
	}
	tok0 := "tok0"
	a := &model.AdminAction{JobID: 5, AdminID: 990077, Action: model.AdminActionRefund, RequestKey: "k1", Reason: "x", Status: model.AdminActionRunning, LeaseToken: &tok0}
	if _, created, err := r.BeginAdminAction(ctx, a, time.Minute); err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	b, created, err := r.BeginAdminAction(ctx, &model.AdminAction{JobID: 5, AdminID: 990077, Action: model.AdminActionRefund, RequestKey: "k1", Reason: "y", Status: model.AdminActionRunning}, time.Minute)
	if err != nil || created || b.Reason != "x" || b.LeaseUntil == nil {
		t.Fatalf("同键返回已有行（带认领）：%+v %v %v", b, created, err)
	}
	if ok, _ := r.TakeOverAdminAction(ctx, b.ID, "x", time.Minute); ok {
		t.Fatal("RUNNING 且认领未过期不能接管")
	}
	_, _ = db.ExecContext(ctx, "UPDATE afv_replicate_admin_action SET lease_until = NOW(3) - INTERVAL 1 SECOND WHERE id = ?", b.ID)
	// 并发接管：只有一个成功
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	winner := ""
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := "t" + string(rune('a'+i))
			if ok, _ := r.TakeOverAdminAction(ctx, b.ID, tok, time.Minute); ok {
				mu.Lock()
				won++
				winner = tok
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("过期认领只能被一个接管：%d", won)
	}
	if ok, _ := r.FinishAdminAction(ctx, b.ID, "tok0", model.AdminActionFailed, 10, []byte(`[]`)); ok {
		t.Fatal("被接管后旧令牌写审计应落空")
	}
	if ok, err := r.FinishAdminAction(ctx, b.ID, winner, model.AdminActionFailed, 10, []byte(`[]`)); err != nil || !ok {
		t.Fatalf("接管者写审计：%v %v", ok, err)
	}
	// 接管令牌不能与上面并发接管的 ta..tf 重名，否则 winner 恰为同名时「旧令牌写明细应落空」随机失败
	if ok, _ := r.TakeOverAdminAction(ctx, b.ID, "t-next", time.Minute); !ok {
		t.Fatal("FAILED 可以同键接管")
	}
	// 逐单明细：同动作同请求号只有一条
	l1, err := r.UpsertRefundLine(ctx, &model.AdminRefundLine{ActionID: b.ID, JobID: 5, SegIndex: 1, RequestID: "rq1", ReservationNo: "BR1", ExpectedPoints: 80, Status: model.RefundLinePending})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.FinishRefundLine(ctx, l1.ID, winner, model.RefundLineFailed, 0, "db down"); ok {
		t.Fatal("旧令牌写明细应落空")
	}
	if ok, err := r.FinishRefundLine(ctx, l1.ID, "t-next", model.RefundLineFailed, 0, "db down"); err != nil || !ok {
		t.Fatalf("当前令牌写明细：%v %v", ok, err)
	}
	// 同键重试：FAILED 重置为 PENDING 并刷新可退点数 / 退前状态，清错误
	l2, _ := r.UpsertRefundLine(ctx, &model.AdminRefundLine{ActionID: b.ID, JobID: 5, SegIndex: 1, RequestID: "rq1", ReservationNo: "BR1",
		BeforeStatus: "CONFIRMED", ExpectedPoints: 60, Status: model.RefundLinePending})
	if l2.ID != l1.ID || l2.Status != model.RefundLinePending || l2.ExpectedPoints != 60 || l2.BeforeStatus != "CONFIRMED" || l2.Error != nil {
		t.Fatalf("同单只一条、FAILED 重置：%+v", l2)
	}
	_, _ = r.FinishRefundLine(ctx, l1.ID, "t-next", model.RefundLineDone, 80, "")
	l3, _ := r.UpsertRefundLine(ctx, &model.AdminRefundLine{ActionID: b.ID, JobID: 5, SegIndex: 1, RequestID: "rq1", ExpectedPoints: 1, Status: model.RefundLinePending})
	if l3.Status != model.RefundLineDone || l3.ExpectedPoints != 60 || l3.Points != 80 {
		t.Fatalf("DONE 原样保留：%+v", l3)
	}
	lines, _ := r.ListRefundLines(ctx, b.ID)
	if len(lines) != 1 || lines[0].Status != model.RefundLineDone || lines[0].Points != 80 {
		t.Fatalf("%+v", lines)
	}
	_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_admin_refund_line WHERE job_id = 5")
	// 计费流水只读
	// 临时库里可能已有 Java 建的真表（ledger_no / balance_after NOT NULL 无默认）：stub 与插入都带上这些列，插入失败直接报错
	_, _ = db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS afv_points_log (id BIGINT AUTO_INCREMENT PRIMARY KEY, ledger_no VARCHAR(64) NOT NULL, user_id BIGINT, change_type VARCHAR(32), amount BIGINT, balance_after BIGINT NOT NULL DEFAULT 0, idempotency_key VARCHAR(128), description VARCHAR(512))")
	if _, err := db.ExecContext(ctx, "INSERT INTO afv_points_log (ledger_no, user_id, change_type, amount, balance_after, idempotency_key, description) VALUES ('PL-it-990077', 990077, 'REFUND', 80, 80, 'refunded:BRit', '后台人工退点[a9]：x')"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DELETE FROM afv_points_log WHERE user_id = 990077") })
	amt, desc, found, err := r.RefundLog(ctx, 990077, "refunded:BRit")
	if err != nil || !found || amt != 80 || desc != "后台人工退点[a9]：x" {
		t.Fatalf("读流水：%d %q %v %v", amt, desc, found, err)
	}
	if _, _, found, _ := r.RefundLog(ctx, 990077, "refunded:none"); found {
		t.Fatal("没有流水")
	}
}
