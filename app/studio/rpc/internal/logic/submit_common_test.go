package logic

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

// fakeBill 假计费：第 failAt 次 Hold 返回余额不足（0=不失败），记录 Hold / Fail 的请求号；
// onHold 在每次 Hold 前回调（用来模拟中途取消 RPC ctx）；ctxDead 记录收到已取消 ctx 的调用数。
type fakeBill struct {
	mode    string
	failAt  int
	n       int
	holds   []string
	fails   []string
	onHold  func(n int)
	ctxDead int
}

func (f *fakeBill) Mode() string                                     { return f.mode }
func (f *fakeBill) CheckAccess(context.Context, int64, string) error { return nil }
func (f *fakeBill) Hold(ctx context.Context, c billing.Charge) (*billing.Reservation, error) {
	f.n++
	if f.onHold != nil {
		f.onHold(f.n)
	}
	if ctx.Err() != nil {
		f.ctxDead++
	}
	if f.n == f.failAt {
		return nil, xerr.Newf(xerr.InsufficientPoints, "点数不足，还差 %d 点", 5)
	}
	f.holds = append(f.holds, c.RequestID)
	return &billing.Reservation{PricePoints: 10}, nil
}
func (f *fakeBill) Fail(ctx context.Context, _ int64, requestID, _ string) {
	if ctx.Err() != nil {
		f.ctxDead++
	}
	f.fails = append(f.fails, requestID)
}

func newTestDeps(t *testing.T, bill *fakeBill) (*deps, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return &deps{
		repo: repo.New(bun.NewDB(sdb, mysqldialect.New())), bill: bill, mod: moderation.Noop{},
		owns:       func(string) bool { return true },
		personalWS: func(context.Context, int64) (int64, error) { return 1, nil },
		ready:      func(model.Kind) error { return nil },
		prefix: func(k model.Kind) string {
			return map[model.Kind]string{"tryon": "tyo", "pose": "pse", "imagegen": "simg"}[k]
		},
		entry:     func(k model.Kind) string { return "studio_" + string(k) },
		sync:      func(context.Context) {},
		newTaskID: func(p string, id int64) string { return fmt.Sprintf("%s_%d", p, id) },
		now:       time.Now,
	}, mock
}

func met(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func codeOf(err error) int {
	c, _, _ := xerr.Parse(err)
	return c
}

func imageGenReq(count int32) *studio.SubmitImageGenReq {
	return &studio.SubmitImageGenReq{UserId: 7, RequestId: "req-1", Prompt: "一只猫", Count: count}
}

func mustImageGenSpec(t *testing.T, in *studio.SubmitImageGenReq) *submitSpec {
	t.Helper()
	v, err := ValidateImageGen(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	return imageGenSpec(in, v)
}

var qBatchByReq = regexp.QuoteMeta("FROM `afv_studio_batch` AS `b` WHERE (b.user_id = 7) AND (b.request_id = 'req-1') LIMIT 1")

func TestSplitCounts(t *testing.T) {
	tv, err := ValidateTryon(&studio.SubmitTryonReq{
		UserId: 7, RequestId: "r", Items: []string{"top"}, CountPerModel: 3,
		ModelImageUrls: []string{"https://a/m1.png", "https://a/m2.png"},
		GarmentImages:  []*studio.GarmentImage{{Url: "https://a/g.png", Role: "上衣"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := tryonSpec(&studio.SubmitTryonReq{UserId: 7, RequestId: "r"}, tv)
	want := []slot{{0, 0, 0}, {0, 1, 0}, {0, 2, 0}, {1, 0, 0}, {1, 1, 0}, {1, 2, 0}}
	if !reflect.DeepEqual(ts.Slots, want) {
		t.Fatalf("2 模特 × 3 张：%v", ts.Slots)
	}

	poses := make([]string, 9)
	for i := range poses {
		poses[i] = fmt.Sprintf("https://a/p%d.png", i)
	}
	pin := &studio.SubmitPoseReq{UserId: 7, RequestId: "r", SourceImageUrl: "https://a/s.png", PoseImageUrls: poses}
	pv, err := ValidatePose(pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	ps := poseSpec(pin, pv)
	if len(ps.Slots) != 9 || ps.Slots[8].Pose != 8 || len(ps.URLs) != 10 || ps.URLs[0].field != "人物图" || ps.URLs[9].field != "姿势图第 9 张" {
		t.Fatalf("9 姿势：slots=%v urls=%d", ps.Slots, len(ps.URLs))
	}

	is := mustImageGenSpec(t, imageGenReq(4))
	if len(is.Slots) != 4 || is.Slots[3].Shot != 3 {
		t.Fatalf("count 4：%v", is.Slots)
	}
}

// 输入图不归本服务：按字段名报错，且在落库之前拦下。
func TestSubmitURLNotOwned(t *testing.T) {
	d, mock := newTestDeps(t, &fakeBill{mode: billing.ModeOn})
	d.owns = func(u string) bool { return u != "https://x/r2.png" }
	in := imageGenReq(1)
	in.ImageUrls = []string{"https://x/r1.png", "https://x/r2.png"}
	mock.ExpectQuery(qBatchByReq).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err := d.submit(context.Background(), mustImageGenSpec(t, in))
	if c, msg, _ := xerr.Parse(err); c != xerr.ParamError || msg != "参考图第 2 张 不是本服务上传的地址" {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)
}

func TestSubmitReplay(t *testing.T) {
	d, mock := newTestDeps(t, &fakeBill{mode: billing.ModeOn})
	sp := mustImageGenSpec(t, imageGenReq(2))
	_, hash, _ := payloadOf(sp.Payload)
	mock.ExpectQuery(qBatchByReq).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "payload_hash", "task_total", "estimated_points"}).
		AddRow(5, 7, hash, 2, 20))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.batch_id = 5) ORDER BY")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "task_id"}).AddRow(1, "simg_1").AddRow(2, "simg_2"))
	resp, err := d.submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Replayed || resp.BatchId != 5 || resp.EstimatedPoints != 20 || !reflect.DeepEqual(resp.TaskIds, []string{"simg_1", "simg_2"}) {
		t.Fatalf("resp=%+v", resp)
	}
	met(t, mock)
}

func TestSubmitIdemConflict(t *testing.T) {
	d, mock := newTestDeps(t, &fakeBill{mode: billing.ModeOn})
	mock.ExpectQuery(qBatchByReq).WillReturnRows(sqlmock.NewRows([]string{"id", "payload_hash"}).AddRow(5, "another-hash"))
	_, err := d.submit(context.Background(), mustImageGenSpec(t, imageGenReq(2)))
	if c, msg, _ := xerr.Parse(err); c != xerr.Conflict || msg != "幂等键已被不同请求使用" {
		t.Fatalf("hash 不同应报幂等冲突：%v", err)
	}
	met(t, mock)
}

// expectSubmitTx 落库事务：批次（提交中）+ n 个子任务（主键 201 起）+ 回填任务号。
func expectSubmitTx(mock sqlmock.Sqlmock, n int) {
	mock.ExpectQuery(qBatchByReq).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_batch`") + ".*VALUES \\(DEFAULT, 7, 1, 'imagegen', 'req-1', .*, 8, ").
		WillReturnResult(sqlmock.NewResult(100, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_task`")).WillReturnResult(sqlmock.NewResult(201, int64(n)))
	rows := sqlmock.NewRows([]string{"id", "batch_id", "user_id", "shot_index"})
	for i := 0; i < n; i++ {
		rows.AddRow(201+i, 100, 7, i)
	}
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.batch_id = 100) ORDER BY")).WillReturnRows(rows)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET task_id = CASE id WHEN 201 THEN 'simg_201'")).
		WillReturnResult(sqlmock.NewResult(0, int64(n)))
	mock.ExpectCommit()
}

// expectHoldFailAt3 4 张里第 3 张冻结失败：前 2 张每张冻结后立刻写计费镜像；善后退回前 2 张、子任务与批次（提交中→失败）置失败。
func expectHoldFailAt3(mock sqlmock.Sqlmock) {
	expectSubmitTx(mock, 4)
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 1, billing_request_id = 'studio-simg_201' WHERE (id = 201)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 1, billing_request_id = 'studio-simg_202' WHERE (id = 202)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 3 WHERE (id = 201)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 3 WHERE (id = 202)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET status = 3, finished_at = NOW(3), error_reason = '点数不足，还差 5 点' WHERE (batch_id = 100) AND (status = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 5, `updated_at` = NOW(3), `error_reason` = '点数不足，还差 5 点', `task_failed` = 4 WHERE (`id` = 100) AND (`status` = 8)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET step_log = JSON_ARRAY_APPEND") + ".*hold_failed").WillReturnResult(sqlmock.NewResult(0, 1))
}

// 第 3 张冻结失败：失败那张也 Fail 一次，前 2 张逐个退回，子任务与批次置失败，返回余额不足。
func TestSubmitHoldFailRefundsHeld(t *testing.T) {
	bill := &fakeBill{mode: billing.ModeOn, failAt: 3}
	d, mock := newTestDeps(t, bill)
	expectHoldFailAt3(mock)

	_, err := d.submit(context.Background(), mustImageGenSpec(t, imageGenReq(4)))
	if codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("应返回余额不足：%v", err)
	}
	if want := []string{"studio-simg_201", "studio-simg_202"}; !reflect.DeepEqual(bill.holds, want) {
		t.Fatalf("holds=%v", bill.holds)
	}
	if want := []string{"studio-simg_203", "studio-simg_201", "studio-simg_202"}; !reflect.DeepEqual(bill.fails, want) {
		t.Fatalf("fails=%v", bill.fails)
	}
	met(t, mock)
}

// RPC ctx 在冻结途中被取消（客户端断开 / 网关 2s 超时）：冻结与善后用脱钩的 ctx，照常走完、照常退款。
func TestSubmitHoldFailAfterCtxCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bill := &fakeBill{mode: billing.ModeOn, failAt: 3, onHold: func(n int) {
		if n == 2 {
			cancel()
		}
	}}
	d, mock := newTestDeps(t, bill)
	expectHoldFailAt3(mock)

	_, err := d.submit(ctx, mustImageGenSpec(t, imageGenReq(4)))
	if codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("应返回余额不足：%v", err)
	}
	if bill.ctxDead != 0 {
		t.Fatalf("冻结 / 退款不应拿到已取消的 ctx（%d 次）", bill.ctxDead)
	}
	if want := []string{"studio-simg_203", "studio-simg_201", "studio-simg_202"}; !reflect.DeepEqual(bill.fails, want) {
		t.Fatalf("fails=%v", bill.fails)
	}
	met(t, mock)
}

func TestSubmitSuccess(t *testing.T) {
	bill := &fakeBill{mode: billing.ModeOn}
	d, mock := newTestDeps(t, bill)
	expectSubmitTx(mock, 2)
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 1, billing_request_id = 'studio-simg_201' WHERE (id = 201)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 1, billing_request_id = 'studio-simg_202' WHERE (id = 202)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET estimated_points = 20 WHERE (id = 100)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 0, `updated_at` = NOW(3) WHERE (`id` = 100) AND (`status` = 8)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND.*submitted.*\"task_total\":2").WillReturnResult(sqlmock.NewResult(0, 1))
	resp, err := d.submit(context.Background(), mustImageGenSpec(t, imageGenReq(2)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.BatchId != 100 || resp.TaskTotal != 2 || resp.EstimatedPoints != 20 || resp.Replayed ||
		!reflect.DeepEqual(resp.TaskIds, []string{"simg_201", "simg_202"}) {
		t.Fatalf("resp=%+v", resp)
	}
	met(t, mock)
}

// 提交中的批次不能取消。
func TestCancelSubmittingRejected(t *testing.T) {
	d, mock := newTestDeps(t, &fakeBill{mode: billing.ModeOn})
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.id = 9) AND (b.user_id = 7)")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchSubmitting))
	_, _, _, err := d.cancel(context.Background(), 9, 7)
	if c, msg, _ := xerr.Parse(err); c != xerr.Conflict || msg != "正在提交，请稍后再试" {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)
}

var taskCols = []string{"id", "batch_id", "user_id", "status", "billing_status", "billing_request_id"}

// 取消只动排队的：生成中的保留（批次不改，交给轮询器），已冻结的排队任务退款。
func TestCancelOnlyQueued(t *testing.T) {
	bill := &fakeBill{mode: billing.ModeOn}
	d, mock := newTestDeps(t, bill)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.id = 9) AND (b.user_id = 7) AND (b.deleted = 0) LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchGenerating))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.batch_id = 9) ORDER BY")).WillReturnRows(sqlmock.NewRows(taskCols).
		AddRow(1, 9, 7, model.TaskQueued, model.BillingHeld, "studio-a").
		AddRow(2, 9, 7, model.TaskRunning, model.BillingHeld, "studio-b").
		AddRow(3, 9, 7, model.TaskSucceeded, model.BillingSettled, "studio-c"))
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 4, `updated_at` = NOW(3), `error_reason` = '已取消', `finished_at` = NOW(3) WHERE (`id` = 1) AND (`status` = 0)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 3 WHERE (id = 1)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET task_done = task_done + 0, task_failed = task_failed + 1 WHERE (id = 9)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.id = 9) LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchGenerating))

	b, canceled, running, err := d.cancel(context.Background(), 9, 7)
	if err != nil {
		t.Fatal(err)
	}
	if canceled != 1 || running != 1 || b.Status != model.BatchGenerating || !reflect.DeepEqual(bill.fails, []string{"studio-a"}) {
		t.Fatalf("canceled=%d running=%d status=%d fails=%v", canceled, running, b.Status, bill.fails)
	}
	met(t, mock)
}

// 全部排队时取消：逐个 CAS，有计费请求号的都退（不看镜像；没冻结成功的 Fail 是空操作），批次 → 已取消。
func TestCancelAllQueued(t *testing.T) {
	bill := &fakeBill{mode: billing.ModeOn}
	d, mock := newTestDeps(t, bill)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.id = 9) AND (b.user_id = 7)")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchPrompting))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (t.batch_id = 9) ORDER BY")).WillReturnRows(sqlmock.NewRows(taskCols).
		AddRow(1, 9, 7, model.TaskQueued, model.BillingHeld, "studio-a").
		AddRow(2, 9, 7, model.TaskQueued, model.BillingNone, "studio-b"))
	mock.ExpectExec(regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 3 WHERE (id = 1)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("WHERE (`id` = 2) AND (`status` = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SET billing_status = 3 WHERE (id = 2)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + 2")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*) AS n")).WillReturnRows(sqlmock.NewRows([]string{"status", "n"}).AddRow(model.TaskCanceled, 2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 7, `updated_at` = NOW(3) WHERE (`id` = 9) AND (`status` = 2)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (b.id = 9) LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchCanceled))

	b, canceled, running, err := d.cancel(context.Background(), 9, 7)
	if err != nil {
		t.Fatal(err)
	}
	if canceled != 2 || running != 0 || b.Status != model.BatchCanceled || !reflect.DeepEqual(bill.fails, []string{"studio-a", "studio-b"}) {
		t.Fatalf("canceled=%d running=%d status=%d fails=%v", canceled, running, b.Status, bill.fails)
	}
	met(t, mock)
}

func TestTaskProgressAndSummary(t *testing.T) {
	cases := []struct {
		b, t int8
		want int32
	}{
		{model.BatchQueued, model.TaskQueued, 0}, {model.BatchCropping, model.TaskQueued, 10},
		{model.BatchPrompting, model.TaskQueued, 30}, {model.BatchGenerating, model.TaskQueued, 60},
		{model.BatchGenerating, model.TaskRunning, 60}, {model.BatchPartial, model.TaskFailed, 100},
	}
	for _, c := range cases {
		if got := TaskProgress(c.b, c.t); got != c.want {
			t.Fatalf("batch=%d task=%d got %d", c.b, c.t, got)
		}
	}
	tk := &model.Task{InputImages: []byte(`[{"url":"u"}]`)}
	tk.PromptText.String = "图1是模特，图2是服装\n正文"
	if PromptSummary(tk) != "正文" {
		t.Fatalf("应去掉图序声明：%q", PromptSummary(tk))
	}
	tk.InputImages = nil
	if PromptSummary(tk) != "图1是模特，图2是服装\n正文" {
		t.Fatalf("纯文生图不去首行：%q", PromptSummary(tk))
	}
}
