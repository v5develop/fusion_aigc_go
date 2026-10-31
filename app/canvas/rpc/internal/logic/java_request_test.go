package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

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
	expectEnd(mock, model.RunCanceled, "已取消执行；Java 任务已取消，点数由 Java 退回")
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
	mock.ExpectExec("SET `error_code` = 'JAVA_CANCEL_PENDING:g:1', `error_reason` = '画布已删除，执行已取消' WHERE \\(id = 21\\) AND \\(status = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ended, pending := c.stopJava(context.Background(), queuedJava(true), stopGone, "画布已删除，执行已取消", "Bearer b", 0); ended || !pending {
		t.Fatalf("ended=%v pending=%v", ended, pending)
	}
	// 第 5 次仍失败：按停止跟踪收尾（排队中）
	mock.ExpectExec("SET `status` = 4, .*`error_reason` = '画布已删除，执行已取消；无法找回 Java 任务号.* WHERE \\(`id` = 21\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ended, _ := c.stopJava(context.Background(), queuedJava(true), stopGone, "画布已删除，执行已取消", "Bearer b", 4); !ended {
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
