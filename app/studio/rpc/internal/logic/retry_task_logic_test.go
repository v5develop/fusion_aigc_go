package logic

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// reqStatusBill 按计费请求号返回预留单状态（原单 / 新单状态不同）；未登记的号回 def。
type reqStatusBill struct {
	*fakeFlowBill
	byReq map[string]string
	def   string
}

func (b *reqStatusBill) Status(_ context.Context, _ int64, requestID string) (string, error) {
	if st, ok := b.byReq[requestID]; ok {
		return st, nil
	}
	return b.def, nil
}

// userFailedTask 用户侧失败子任务：原单已退回、已有提示词。
func userFailedTask(attempt int32) *model.Task {
	tk := failedTask(attempt)
	tk.BillingStatus = model.BillingRefunded
	return tk
}

func newRetryFlow(t *testing.T, oldStatus string) (*flow, *fakeFlowBill, func() error) {
	t.Helper()
	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": oldStatus}, def: resvHeld}
	return f, inner, mock.ExpectationsWereMet
}

// 部分成功批次里的失败任务：原单已退回 → 冻结新号 r1（不退、不碰原单）→ 任务排队、失败计数 -1、批次拉回生图中、记 user_retry。
func TestUserRetrySuccess(t *testing.T) {
	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": resvRefunded}, def: resvHeld}
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 0") + ".*`billing_request_id` = 'studio-simg_1-r1'.*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 3)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3") + ".*" + regexp.QuoteMeta("(`status` = 6)")).WillReturnResult(ok1)
	mock.ExpectExec("user_retry").WillReturnResult(ok1)
	if err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchPartial)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inner.holds, []string{"studio-simg_1-r1"}) || len(inner.fails) != 0 || len(inner.confirmed) != 0 {
		t.Fatalf("holds=%v fails=%v confirmed=%v", inner.holds, inner.fails, inner.confirmed)
	}
	met(t, mock)
}

// 前置条件不满足：一律冲突，不碰库、不冻结。
func TestUserRetryRejects(t *testing.T) {
	cases := []struct {
		name   string
		task   func() *model.Task
		batch  int8
		old    string
		code   int
		substr string
	}{
		{"已取消的任务", func() *model.Task { tk := userFailedTask(0); tk.Status = model.TaskCanceled; return tk }, model.BatchPartial, resvRefunded, xerr.Conflict, "只有失败"},
		{"运营封禁", func() *model.Task { tk := userFailedTask(0); tk.AdminLocked = 1; return tk }, model.BatchFailed, resvRefunded, xerr.Conflict, "停止重试"},
		{"批次已取消", func() *model.Task { return userFailedTask(0) }, model.BatchCanceled, resvRefunded, xerr.Conflict, "已取消"},
		{"没有提示词", func() *model.Task {
			tk := userFailedTask(0)
			tk.PromptText.String, tk.PromptText.Valid = "", false
			return tk
		}, model.BatchFailed, resvRefunded, xerr.Conflict, "提示词"},
		{"超过轮次", func() *model.Task { return userFailedTask(userRetryMaxAttempts) }, model.BatchFailed, resvRefunded, xerr.Conflict, "重试多次"},
		{"镜像仍冻结", func() *model.Task { tk := userFailedTask(0); tk.BillingStatus = model.BillingHeld; return tk }, model.BatchFailed, resvRefunded, xerr.Conflict, "结算中"},
		// 镜像写成已退回，但计费服务里原单仍 HELD（结果未知待对账）：以真实状态为准拒绝
		{"原单仍 HELD", func() *model.Task { return userFailedTask(0) }, model.BatchFailed, resvHeld, xerr.Conflict, "结算中"},
		{"原单已扣费", func() *model.Task { return userFailedTask(0) }, model.BatchFailed, resvConfirmed, xerr.Conflict, "结算中"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, inner, met := newRetryFlow(t, c.old)
			err := f.userRetry(context.Background(), c.task(), retryBatch(c.batch))
			code, msg, _ := xerr.Parse(err)
			if code != c.code || !strings.Contains(msg, c.substr) {
				t.Fatalf("err=%v", err)
			}
			if len(inner.holds) != 0 || len(inner.fails) != 0 {
				t.Fatalf("不应冻结 / 退款：holds=%v fails=%v", inner.holds, inner.fails)
			}
			if err := met(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// 原单没冻结成功（计费服务查无此单）/ 已过期释放：可以重试。
func TestUserRetryOldReservationReleased(t *testing.T) {
	for _, st := range []string{"", resvExpired} {
		inner := onBill()
		f, mock, _ := newTestFlow(t, inner)
		f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": st}, def: resvHeld}
		mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
		mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_1-r1'")).WillReturnResult(ok1)
		mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
		mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3")).WillReturnResult(ok1)
		mock.ExpectExec("user_retry").WillReturnResult(ok1)
		if err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchFailed)); err != nil {
			t.Fatalf("原单 %q: %v", st, err)
		}
		met(t, mock)
	}
}

// 第二次重试读到同一快照：轮次已被占 → 冲突（不冻结）；调用方据 retriedInFlight 重读判幂等。
func TestUserRetryConcurrentSecondConflicts(t *testing.T) {
	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": resvRefunded}, def: resvHeld}
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok0)
	if err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchFailed)); codeOf(err) != xerr.Conflict {
		t.Fatalf("err=%v", err)
	}
	if len(inner.holds) != 0 {
		t.Fatalf("holds=%v", inner.holds)
	}
	met(t, mock)
}

// qRelease 冻结失败后退还轮次的 CAS（attempt 仍是 claimed、仍失败 / 已取消、未封禁、计费号仍是原号才 -1）。
func qRelease(claimed int, oldReq string) string {
	return regexp.QuoteMeta(fmt.Sprintf("UPDATE `afv_studio_task` SET attempt = attempt - 1 WHERE (id = 1) AND (attempt = %d) AND (status IN (3, 4)) AND (admin_locked = 0) AND (billing_request_id = '%s')", claimed, oldReq))
}

// 余额预检不够：直接 3003，不占轮次、不冻结（库一次都不碰）；预检的计费参数与冻结同口径（1 张、归一后的分辨率）。
func TestUserRetryPrecheckRejectsWithoutClaim(t *testing.T) {
	f, inner, met := newRetryFlow(t, resvRefunded)
	inner.enoughErr = xerr.Newf(xerr.InsufficientPoints, "点数不足，还差 %d 点", 3)
	err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchPartial))
	if codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("err=%v", err)
	}
	if len(inner.holds) != 0 || inner.n != 0 {
		t.Fatalf("预检失败不应冻结：holds=%v n=%d", inner.holds, inner.n)
	}
	if len(inner.enoughs) != 1 || inner.enoughs[0].Count != 1 || inner.enoughs[0].UserID != 7 || inner.enoughs[0].Resolution != "1K" {
		t.Fatalf("enoughs=%+v", inner.enoughs)
	}
	if err := met(); err != nil { // 没有 ClaimRetry
		t.Fatal(err)
	}
}

// 预检通过但 Hold 仍失败（并发扣点）：新号查无预留单 → 轮次退还（attempt 1 → 0），错误原样返回，任务不动。
// 退还后再重试仍用 r1（不被前一次失败消耗次数）。
func TestUserRetryHoldFailReleasesAttempt(t *testing.T) {
	inner := onBill()
	inner.failAt = 1 // 第一次 Hold 点数不足
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": resvRefunded, "studio-simg_1-r1": ""}, def: resvHeld}
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	mock.ExpectExec(qRelease(1, "studio-simg_1")).WillReturnResult(ok1)
	err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchPartial))
	if codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)

	// 充值后再点：库里 attempt 已回到 0，照常占 r1、冻结 r1
	f.bill.(*reqStatusBill).byReq["studio-simg_1-r1"] = resvHeld
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_1-r1'")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3")).WillReturnResult(ok1)
	mock.ExpectExec("user_retry").WillReturnResult(ok1)
	if err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchPartial)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inner.holds, []string{"studio-simg_1-r1"}) || len(inner.fails) != 0 {
		t.Fatalf("holds=%v fails=%v", inner.holds, inner.fails)
	}
	met(t, mock)
}

// Hold 失败但新号在计费服务里查得到单（冻结超时其实落库了）/ 查询出错：结果不明，不退还轮次（下次换 r2），也不退单。
func TestUserRetryHoldFailUnknownKeepsAttempt(t *testing.T) {
	for _, st := range []string{resvHeld, resvRefunded} {
		inner := onBill()
		inner.failAt = 1
		f, mock, _ := newTestFlow(t, inner)
		f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": resvRefunded, "studio-simg_1-r1": st}, def: resvHeld}
		mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
		if err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchFailed)); codeOf(err) != xerr.InsufficientPoints {
			t.Fatalf("%s: err=%v", st, err)
		}
		if len(inner.fails) != 0 {
			t.Fatalf("%s: 不应退单 %v", st, inner.fails)
		}
		met(t, mock) // 没有 ReleaseRetry
	}
}

// 运营重做同样退还：Hold 失败且查无新单 → attempt 回退。
func TestAdminRetryHoldFailReleasesAttempt(t *testing.T) {
	bill := onBill()
	bill.failAt = 1
	f, mock, _ := newTestFlow(t, bill) // Status 恒 ""：查无此单
	mock.ExpectExec(qClaim(2)).WillReturnResult(ok1)
	mock.ExpectExec(qRelease(3, "studio-simg_1")).WillReturnResult(ok1)
	if err := f.adminRetry(context.Background(), failedTask(2), retryBatch(model.BatchFailed)); codeOf(err) != xerr.InsufficientPoints {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)
}

// taskRows ListByBatch 的结果行：(id, status, billing_request_id)。
func taskRows(rows ...[]any) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"id", "batch_id", "user_id", "status", "billing_request_id"})
	for _, x := range rows {
		r.AddRow(x[0], 9, 7, x[1], x[2])
	}
	return r
}

var qListBatch9 = regexp.QuoteMeta("WHERE (t.batch_id = 9) ORDER BY")

func secondFailedTask() *model.Task {
	tk := userFailedTask(0)
	tk.ID = 2
	tk.TaskID.String = "simg_2"
	tk.BillingRequestID.String = "studio-simg_2"
	return tk
}

// 同批两张失败：第一张重试把批次拉回生图中；第二张紧接着重试 —— 在途的只有第一张的重做轮次（-r1），允许，
// 批次不再拉回（已在生图中），重读确认没被收敛。两张各自占轮次、各冻结一单，失败计数各 -1。
func TestUserRetryTwoInSameBatch(t *testing.T) {
	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": resvRefunded, "studio-simg_2": resvRefunded}, def: resvHeld}
	// 第一张：批次部分成功 → 拉回生图中
	mock.ExpectExec(qClaim(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_1-r1'")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3") + ".*" + regexp.QuoteMeta("(`status` = 6)")).WillReturnResult(ok1)
	mock.ExpectExec("user_retry").WillReturnResult(ok1)
	if err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchPartial)); err != nil {
		t.Fatal(err)
	}
	// 第二张：批次已是生图中；同批：1 在重做中、2 失败、3 成功
	mock.ExpectQuery(qListBatch9).WillReturnRows(taskRows(
		[]any{1, model.TaskQueued, "studio-simg_1-r1"}, []any{2, model.TaskFailed, "studio-simg_2"}, []any{3, model.TaskSucceeded, "studio-simg_3"}))
	mock.ExpectExec(regexp.QuoteMeta("SET attempt = attempt + 1 WHERE (id = 2) AND (attempt = 0)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_2-r1'") + ".*" + regexp.QuoteMeta("WHERE (`id` = 2) AND (`status` = 3)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchGenerating))
	mock.ExpectExec("user_retry").WillReturnResult(ok1)
	if err := f.userRetry(context.Background(), secondFailedTask(), retryBatch(model.BatchGenerating)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inner.holds, []string{"studio-simg_1-r1", "studio-simg_2-r1"}) || len(inner.fails) != 0 {
		t.Fatalf("holds=%v fails=%v", inner.holds, inner.fails)
	}
	met(t, mock)
}

// 第二张重试时第一张的重做恰好结束、批次已按当时计数收敛成部分成功：重读发现终态 → 拉回生图中（之后 converge 按实际计数重算）。
func TestUserRetrySameBatchReopensConverged(t *testing.T) {
	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_2": resvRefunded}, def: resvHeld}
	mock.ExpectQuery(qListBatch9).WillReturnRows(taskRows(
		[]any{1, model.TaskRunning, "studio-simg_1-r1"}, []any{2, model.TaskFailed, "studio-simg_2"}))
	mock.ExpectExec(regexp.QuoteMeta("SET attempt = attempt + 1 WHERE (id = 2) AND (attempt = 0)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("`billing_request_id` = 'studio-simg_2-r1'")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchPartial))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3") + ".*" + regexp.QuoteMeta("(`status` = 6)")).WillReturnResult(ok1)
	mock.ExpectExec("user_retry").WillReturnResult(ok1)
	if err := f.userRetry(context.Background(), secondFailedTask(), retryBatch(model.BatchGenerating)); err != nil {
		t.Fatal(err)
	}
	met(t, mock)
}

// 批次生图中且还有首轮任务在途（原始流程在跑）：拒绝，不占轮次、不冻结。编排阶段（排队 / 切图 / 反推）直接拒绝、不查库。
func TestUserRetryRejectsWhileOriginalRunning(t *testing.T) {
	for _, st := range []int8{model.TaskQueued, model.TaskRunning, model.TaskDelivering} {
		inner := onBill()
		f, sm, _ := newTestFlow(t, inner)
		f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": resvRefunded}, def: resvHeld}
		met := sm.ExpectationsWereMet
		sm.ExpectQuery(qListBatch9).WillReturnRows(taskRows(
			[]any{1, model.TaskFailed, "studio-simg_1"}, []any{2, st, "studio-simg_2"}))
		err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(model.BatchGenerating))
		if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || msg != errBatchRunning {
			t.Fatalf("在途状态 %d: err=%v", st, err)
		}
		if len(inner.holds) != 0 || len(inner.enoughs) != 0 {
			t.Fatalf("holds=%v enoughs=%v", inner.holds, inner.enoughs)
		}
		if err := met(); err != nil {
			t.Fatal(err)
		}
	}
	for _, bs := range []int8{model.BatchQueued, model.BatchCropping, model.BatchPrompting} {
		f, inner, met := newRetryFlow(t, resvRefunded)
		err := f.userRetry(context.Background(), userFailedTask(0), retryBatch(bs))
		if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || msg != errBatchRunning {
			t.Fatalf("批次 %d: err=%v", bs, err)
		}
		if len(inner.holds) != 0 {
			t.Fatalf("holds=%v", inner.holds)
		}
		if err := met(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetriedInFlight(t *testing.T) {
	tk := userFailedTask(1)
	if retriedInFlight(tk) {
		t.Fatal("失败态不算在途")
	}
	tk.Status = model.TaskQueued
	if retriedInFlight(tk) {
		t.Fatal("首轮号（无 -r）不算重做中")
	}
	tk.BillingRequestID.String = "studio-simg_1-r1"
	if !retriedInFlight(tk) {
		t.Fatal("重做轮次在途应判为已重试")
	}
}

func TestNoPersonText(t *testing.T) {
	cases := []struct {
		err  error
		want string
		ok   bool
	}{
		{&reverse.Error{Path: reverse.PathPoseSource, Status: 422, Kind: reverse.KindInvalid, Message: "原图中没有识别到人物，请换一张有模特的照片"}, noPersonSource, true},
		{&reverse.Error{Path: reverse.PathPosePrompt, Status: 422, Kind: reverse.KindInvalid, Message: "姿势参考图中没有识别到人物，请换一张"}, noPersonPose, true},
		{&reverse.Error{Path: reverse.PathPosePrompt, Status: 422, Kind: reverse.KindInvalid, Message: "no person detected"}, noPersonPose, true},
		{&reverse.Error{Path: reverse.PathPoseSource, Status: 422, Kind: reverse.KindInvalid, Message: "No person in image"}, noPersonSource, true},
		{&reverse.Error{Path: reverse.PathPoseSource, Status: 422, Kind: reverse.KindInvalid, Message: "暂不支持未成年人图片"}, "", false},
		{&reverse.Error{Path: reverse.PathPoseSource, Status: 502, Kind: reverse.KindUpstream, Message: "没有识别到人物"}, "", false},
		{errors.New("没有识别到人物"), "", false},
	}
	for i, c := range cases {
		got, ok := noPersonText(c.err)
		if got != c.want || ok != c.ok {
			t.Fatalf("#%d got %q %v", i, got, ok)
		}
	}
}

// 预览：原图 / 姿势图无人物 → 固定文案，错误码不变（参数错误）；单张姿势图失败只写在该项 error。
func TestPreviewNoPersonMessages(t *testing.T) {
	ctx := context.Background()
	rev := &fakeRev{srcErr: &reverse.Error{Path: reverse.PathPoseSource, Status: 422, Kind: reverse.KindInvalid, Message: "原图中没有识别到人物，请换一张有模特的照片"}}
	_, err := newPreviewer(rev, &previewMedia{}).preview(ctx, &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: urls(1)})
	if code, msg, _ := xerr.Parse(err); code != xerr.ParamError || msg != noPersonSource {
		t.Fatalf("原图无人物：%v", err)
	}
	rev = &fakeRev{srcPeople: 1, posePeople: 1, failPose: map[string]error{}}
	poses := urls(2)
	rev.failPose[reverse.DataURL("image/jpeg", []byte(poses[0]))] = &reverse.Error{Path: reverse.PathPosePrompt, Status: 422, Kind: reverse.KindInvalid, Message: "姿势参考图中没有识别到人物，请换一张"}
	out, err := newPreviewer(rev, &previewMedia{}).preview(ctx, &studio.PreviewPoseReq{UserId: 9, SourceImageUrl: okURL, PoseImageUrls: poses})
	if err != nil {
		t.Fatal(err)
	}
	if out.Poses[0].Error != noPersonPose || out.Poses[0].Pose != nil || out.Poses[1].Pose == nil {
		t.Fatalf("poses=%+v", out.Poses)
	}
}

// 编排失败文案：无人物统一成固定句，后缀「点数已退回」不变。
func TestClassifyNoPerson(t *testing.T) {
	err := classify("pose_source", &reverse.Error{Path: reverse.PathPoseSource, Status: 422, Kind: reverse.KindInvalid, Message: "原图中没有识别到人物，请换一张有模特的照片"})
	var se *stepError
	if !errors.As(err, &se) || se.User != noPersonSource+"，点数已退回" || se.Retryable {
		t.Fatalf("err=%+v", err)
	}
}
