package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"

	"github.com/DATA-DOG/go-sqlmock"
)

// 落库的原始请求：key 顺序 / 空白都与 json.Marshal 不同，用来验证逐字节重发。
const storedBody = `{"resolution":"2k",  "imageUrl":"https://x/a.png?sig=OLD","title":"画布 · 高清放大"}`

func queuedJava(withReq bool) *model.NodeRun {
	r := runningJava()
	r.Status, r.BackendRef, r.Attempt = model.RunQueued, sql.NullString{}, 1
	r.TaskID = sql.NullString{String: "cv_t", Valid: true}
	if withReq {
		b, _ := json.Marshal(javaRequest{Path: "/api/v1/tools/ai-image-upscaler", Idem: "canvas-cv_t", Body: storedBody})
		r.BackendRequest = b
	}
	return r
}

var (
	submitOK       = reply(200, `{"code":0,"msg":"success","data":{"taskId":"JT9","taskIds":["JT9"]}}`)
	submitConflict = reply(200, `{"code":1003,"msg":"同一幂等键已用于内容不同的请求，请更换 Idempotency-Key"}`)
	submit502      = reply(502, `<html>bad gateway</html>`)
)

func TestSubmitJavaPersistsBeforeSend(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submitOK}
	fj.start(t, c)
	run := queuedJava(false)
	sp := &runSpec{Kind: model.KindToolUpscale, Backend: model.BackendJava, Images: []refMedia{{URL: "https://x/a.png"}}, Params: map[string]any{"scale": 2}}
	mock.ExpectExec("UPDATE `afv_canvas_node_run` AS `r` SET `backend_request` = '\\{\"path\":\"/api/v1/tools/ai-image-upscaler\",\"idem\":\"canvas-cv_t\",\"body\":.*WHERE \\(id = 21\\) AND \\(status = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	ref, err := c.submitJava(context.Background(), run, sp, "Bearer b")
	if err != nil || ref != "JT9" {
		t.Fatalf("%s %v", ref, err)
	}
	jr, ok := parseJavaRequest(run)
	if !ok || len(fj.submits) != 1 || fj.submits[0] != jr.Path+" "+jr.Idem+" "+jr.Body {
		t.Fatalf("发出的字节应与落库的一致: %v %+v", fj.submits, jr)
	}
	// 再提交（暂时性失败后的重提）：不重算 body，逐字节重发落库的那份
	run.BackendRequest, _ = json.Marshal(javaRequest{Path: jr.Path, Idem: jr.Idem, Body: storedBody})
	if _, err := c.submitJava(context.Background(), run, sp, "Bearer b"); err != nil {
		t.Fatal(err)
	}
	if fj.submits[1] != jr.Path+" canvas-cv_t "+storedBody {
		t.Fatalf("重提应逐字节重发: %s", fj.submits[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitJavaPersistLostDoesNotSend(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submitOK}
	fj.start(t, c)
	sp := &runSpec{Kind: model.KindToolUpscale, Backend: model.BackendJava, Images: []refMedia{{URL: "https://x/a.png"}}, Params: map[string]any{"scale": 2}}
	mock.ExpectExec("SET `backend_request`").WillReturnResult(sqlmock.NewResult(0, 0)) // run 已被取消
	if _, err := c.submitJava(context.Background(), queuedJava(false), sp, "Bearer b"); err == nil {
		t.Fatal("落库落空应报错")
	}
	if len(fj.submits) != 0 {
		t.Fatalf("没落库不应发送: %v", fj.submits)
	}
}

func TestCancelQueuedJavaReplaysThenCancels(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submitOK, cancel: javaOK}
	fj.start(t, c)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 1, .*`backend_ref` = 'JT9'.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectEnd(mock, model.RunCanceled, "已取消执行；已请求 Java 取消，点数以 Java 终态为准")
	ended, pending := c.stopJava(context.Background(), queuedJava(true), stopUser, "已取消执行", "Bearer b", 0)
	if !ended || pending {
		t.Fatalf("ended=%v pending=%v", ended, pending)
	}
	if len(fj.submits) != 1 || fj.submits[0] != "/api/v1/tools/ai-image-upscaler canvas-cv_t "+storedBody {
		t.Fatalf("应按原幂等键逐字节重放: %v", fj.submits)
	}
	if got := fj.calls(); len(got) != 1 || got[0] != "JT9 Bearer b" {
		t.Fatalf("找回后应取消 JT9: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayConflictDegrades(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submitConflict, cancel: javaOK}
	fj.start(t, c)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_reason` = '运营已终止该任务；无法找回 Java 任务号.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ended, _ := c.stopJava(context.Background(), queuedJava(true), stopAdmin, "运营已终止该任务", "Bearer b", 0); !ended {
		t.Fatal("冲突应退化收尾")
	}
	if len(fj.calls()) != 0 {
		t.Fatal("拿不到任务号不应调取消")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayTransientCountsTowardsRetries(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submit502}
	fj.start(t, c)
	mock.ExpectExec("SET `error_code` = 'JAVA_CANCEL_PENDING:g:1:[0-9]+', `error_reason` = '画布已删除，执行已取消' WHERE \\(id = 21\\) AND \\(status = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ended, pending := c.stopJava(context.Background(), queuedJava(true), stopGone, "画布已删除，执行已取消", "Bearer b", 0); ended || !pending {
		t.Fatalf("ended=%v pending=%v", ended, pending)
	}
	// 第 6 次仍失败：按停止跟踪收尾（排队中）
	mock.ExpectExec("SET `status` = 4, .*`error_reason` = '画布已删除，执行已取消；无法找回 Java 任务号.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ended, _ := c.stopJava(context.Background(), queuedJava(true), stopGone, "画布已删除，执行已取消", "Bearer b", 5); !ended {
		t.Fatal("超限应收尾")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelQueuedJavaWithoutRequestNoReplay(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submitOK, cancel: javaOK}
	fj.start(t, c)
	mock.ExpectQuery(qRunGet).WillReturnRows(sqlmock.NewRows(runCols).AddRow(21, "cv_1", 7, 0, "java", "tool_upscale", nil, "java", `{}`, nil, nil, nil))
	mock.ExpectExec("SET `status` = 4, .*`error_reason` = '已取消执行'.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := c.CancelRun(withBearer("Bearer b"), &canvas.RunIdReq{UserId: 7, Id: 21}); err != nil {
		t.Fatal(err)
	}
	if len(fj.submits) != 0 || len(fj.calls()) != 0 {
		t.Fatalf("没发过请求不应重放 / 取消: %v %v", fj.submits, fj.calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// seq 依次回包（用完停在最后一个）。fakeJava 的回调在锁内调用，计数无需再加锁。
func seq(rs ...func(http.ResponseWriter)) func(http.ResponseWriter) {
	i := 0
	return func(w http.ResponseWriter) {
		r := rs[min(i, len(rs)-1)]
		i++
		r(w)
	}
}

var upscaleSpec = &runSpec{Kind: model.KindToolUpscale, Backend: model.BackendJava, Images: []refMedia{{URL: "https://x/a.png"}}, Params: map[string]any{"scale": 2}}

// 已落 backend_request 的排队 run 重提连续暂时失败到上限（测试配置 MaxSubmitAttempts=0）：不直接置失败，
// 走 stopJava 按幂等键重放找回任务号——找回成功就取消 Java 任务（由 Java 退款）。
func TestDispatchJavaRetryExhaustedRecoversAndCancels(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: seq(submit502, submitOK), cancel: javaOK}
	fj.start(t, c)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 1, .*`backend_ref` = 'JT9'.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_code` = 'SUBMIT_FAILED', `error_reason` = '提交失败，请稍后重试；已请求 Java 取消.* WHERE \\(`id` = 21\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.dispatch(context.Background(), queuedJava(true), upscaleSpec, "Bearer b"); err == nil {
		t.Fatal("提交失败应回错误")
	}
	if len(fj.submits) != 2 || fj.submits[1] != "/api/v1/tools/ai-image-upscaler canvas-cv_t "+storedBody {
		t.Fatalf("应先重提、再同键重放找回: %v", fj.submits)
	}
	if got := fj.calls(); len(got) != 1 || got[0] != "JT9 Bearer b" {
		t.Fatalf("找回后应取消 JT9: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 同上但找回也暂时失败：登记待取消（场景 s），由 Poller 退避重试；退避表用完仍找不回 → 退化（计数 + WARN），不调取消。
func TestDispatchJavaRetryExhaustedDegradesWhenUnrecoverable(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submit502, cancel: javaOK}
	fj.start(t, c)
	mock.ExpectExec("SET `error_code` = 'JAVA_CANCEL_PENDING:s:1:[0-9]+', `error_reason` = '提交失败，请稍后重试' WHERE \\(id = 21\\) AND \\(status = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	_ = c.dispatch(context.Background(), queuedJava(true), upscaleSpec, "Bearer b")
	if len(fj.submits) != 2 {
		t.Fatalf("重提 + 找回各一次: %v", fj.submits)
	}
	// 刚登记（第 1 次失败后等 15s）：没到点不重试
	run := queuedJava(true)
	run.ErrorCode = sql.NullString{String: fmt.Sprintf("JAVA_CANCEL_PENDING:s:1:%d", time.Now().Unix()), Valid: true}
	run.ErrorReason = sql.NullString{String: "提交失败，请稍后重试", Valid: true}
	if !c.retryPendingJavaStop(context.Background(), run, "Bearer b") || len(fj.submits) != 2 {
		t.Fatalf("未到退避时间不应重放: %v", fj.submits)
	}
	// 退避表用完（第 6 次）仍失败：退化收尾
	run.ErrorCode.String = fmt.Sprintf("JAVA_CANCEL_PENDING:s:5:%d", time.Now().Add(-5*time.Minute).Unix())
	mock.ExpectExec("SET `status` = 3, .*`error_code` = 'SUBMIT_FAILED', `error_reason` = '提交失败，请稍后重试；无法找回 Java 任务号.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.retryPendingJavaStop(context.Background(), run, "Bearer b")
	if len(fj.submits) != 3 || len(fj.calls()) != 0 {
		t.Fatalf("到点重放一次、拿不到任务号不调取消: %v %v", fj.submits, fj.calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 重提回 1003（如 Java 发版致摘要变化）/ 401：提交场景仍是永久失败，但此前的请求可能已建任务，必须走 stopJava
// （找不回 → 退化告警），不能直接 failRun。
func TestDispatchJavaPermanentAfterUnknownGoesStopJava(t *testing.T) {
	for _, tc := range []struct {
		name   string
		submit func(http.ResponseWriter)
		reason string
	}{
		{"1003", submitConflict, "同一幂等键已用于内容不同的请求，请更换 Idempotency-Key；无法找回 Java 任务号"},
		{"401", java401, "登录已过期，请重新登录；Java 任务未能取消（无有效登录态）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, mock := mockCore(t)
			fj := &fakeJava{submit: tc.submit, cancel: javaOK}
			fj.start(t, c)
			mock.ExpectExec("SET `status` = 3, .*`error_code` = 'SUBMIT_FAILED', `error_reason` = '" + tc.reason + ".* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
				WillReturnResult(sqlmock.NewResult(0, 1))
			_ = c.dispatch(context.Background(), queuedJava(true), upscaleSpec, "Bearer b")
			if len(fj.submits) != 2 || len(fj.calls()) != 0 {
				t.Fatalf("应重放找回一次、不调取消: %v %v", fj.submits, fj.calls())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// 首次提交就被 Java 明确拒绝（attempt=0，同步校验）：Java 没建任务，照常直接置失败，不重放。
func TestDispatchJavaFirstRejectFailsDirectly(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{submit: submitConflict}
	fj.start(t, c)
	run := queuedJava(true)
	run.Attempt = 0
	mock.ExpectExec("SET `status` = 3, .*`error_code` = 'PARAM_INVALID', `error_reason` = '同一幂等键已用于内容不同的请求，请更换 Idempotency-Key'.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	_ = c.dispatch(context.Background(), run, upscaleSpec, "Bearer b")
	if len(fj.submits) != 1 {
		t.Fatalf("不应重放: %v", fj.submits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Poller 场景（超时 / 画布已删 / 提交失败 / 待取消重试）缺令牌：计次等令牌，不立即退化；退避表用完才按无登录态退化。
// 用户 / 运营当次请求缺令牌仍立即退化（见 TestForceFailJavaWithoutBearerDegrades）。
func TestStopJavaNoBearerBackgroundRetries(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{cancel: javaOK}
	fj.start(t, c)
	mock.ExpectExec("SET `error_code` = 'JAVA_CANCEL_PENDING:g:1:[0-9]+', `error_reason` = '画布已删除，执行已取消' WHERE \\(id = 21\\) AND \\(status = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ended, pending := c.stopJava(context.Background(), runningJava(), stopGone, "画布已删除，执行已取消", "", 0); ended || !pending {
		t.Fatalf("ended=%v pending=%v", ended, pending)
	}
	expectEnd(mock, model.RunCanceled, "画布已删除，执行已取消；Java 任务未能取消（无有效登录态）")
	if ended, _ := c.stopJava(context.Background(), runningJava(), stopGone, "画布已删除，执行已取消", "", 5); !ended {
		t.Fatal("退避表用完应退化")
	}
	if len(fj.calls()) != 0 {
		t.Fatal("没有令牌不应调取消")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestJavaStopBackoff(t *testing.T) {
	c := testCore()
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }
	mk := func(tries int, ago time.Duration) *model.NodeRun {
		return &model.NodeRun{ErrorCode: sql.NullString{String: fmt.Sprintf("JAVA_CANCEL_PENDING:t:%d:%d", tries, now.Add(-ago).Unix()), Valid: true}}
	}
	for _, tc := range []struct {
		tries int
		ago   time.Duration
		due   bool
	}{
		{1, 10 * time.Second, false}, {1, 15 * time.Second, true},
		{2, 29 * time.Second, false}, {3, time.Minute, true},
		{4, 119 * time.Second, false}, {5, 4 * time.Minute, true}, {9, 4 * time.Minute, true},
	} {
		if got := c.javaStopDue(mk(tc.tries, tc.ago)); got != tc.due {
			t.Errorf("tries=%d ago=%s due=%v want %v", tc.tries, tc.ago, got, tc.due)
		}
	}
	if c.javaStopMaxTries() != 6 {
		t.Fatalf("默认 5 项退避 + 首次: %d", c.javaStopMaxTries())
	}
	c.s.Config.Poll.JavaStopBackoffSeconds = []int{1, 2}
	if c.javaStopMaxTries() != 3 || c.javaStopDue(mk(2, time.Second)) {
		t.Fatal("退避表可配置")
	}
}
