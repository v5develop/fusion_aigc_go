package logic

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"

	"github.com/DATA-DOG/go-sqlmock"
	"google.golang.org/grpc/metadata"
)

// fakeJava 记录取消请求；cancel 决定取消接口回包，task 决定任务查询回包。
type fakeJava struct {
	mu      sync.Mutex
	cancels []string // "<taskUid> <Authorization>"
	gets    int
	cancel  func(w http.ResponseWriter)
	task    string
	submits []string // "<path> <Idempotency-Key> <原始 body>"
	submit  func(w http.ResponseWriter)
}

func (f *fakeJava) start(t *testing.T, c *core) {
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/tools/"):
			f.submits = append(f.submits, r.URL.Path+" "+r.Header.Get("Idempotency-Key")+" "+string(body))
			f.submit(w)
		case strings.HasPrefix(r.URL.Path, "/api/v1/creation/") && strings.HasSuffix(r.URL.Path, "/cancel"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/creation/"), "/cancel")
			f.cancels = append(f.cancels, id+" "+r.Header.Get("Authorization"))
			f.cancel(w)
		case strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
			f.gets++
			_, _ = w.Write([]byte(f.task))
		default:
			w.WriteHeader(404)
		}
	})
	c.s.Java = javatool.New(srv.URL, 5*time.Second)
}

func (f *fakeJava) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancels...)
}

func reply(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

var (
	javaOK       = reply(200, `{"code":0,"msg":"success","data":true}`)
	javaFinished = reply(200, `{"code":3001,"msg":"任务已完成或失败，无法取消"}`)
	javaNotFound = reply(200, `{"code":4001,"msg":"创作任务不存在: JT1"}`)
	java401      = reply(401, `{"code":2001,"msg":"未登录或登录已过期"}`)
	java500      = reply(500, `{"code":5001,"msg":"系统内部错误"}`)
)

var runCols = []string{"id", "canvas_id", "user_id", "status", "backend", "kind", "backend_ref", "billing_status", "request", "error_code", "error_reason", "created_at"}

func javaRunRow(status int8, errCode, errReason any) *sqlmock.Rows {
	return sqlmock.NewRows(runCols).AddRow(21, "cv_1", 7, status, "java", "tool_upscale", "JT1", "java", `{}`, errCode, errReason, time.Now())
}

func runningJava() *model.NodeRun {
	return &model.NodeRun{ID: 21, CanvasID: "cv_1", UserID: 7, Kind: model.KindToolUpscale, Backend: model.BackendJava,
		Status: model.RunRunning, BillingStatus: model.BillJava, BackendRef: sql.NullString{String: "JT1", Valid: true}, CreatedAt: time.Now()}
}

func withBearer(b string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(BearerMetadataKey, b))
}

const qRunGet = "SELECT .* FROM `afv_canvas_node_run` AS `r` WHERE \\(r.id = 21\\)"

func expectEnd(mock sqlmock.Sqlmock, to int8, reason string) {
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = " + string('0'+rune(to)) + ", .*`error_reason` = '" + reason + ".* WHERE \\(`id` = 21\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestCancelRunJavaUsesRequestBearer(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{cancel: javaOK}
	fj.start(t, c)
	c.s.Bearers.Put(7, "Bearer stale")
	mock.ExpectQuery(qRunGet).WillReturnRows(javaRunRow(1, nil, nil))
	expectEnd(mock, model.RunCanceled, "已取消执行；已请求 Java 取消，点数以 Java 终态为准")
	mock.ExpectQuery(qRunGet).WillReturnRows(javaRunRow(4, "CANCELED", "已取消执行；已请求 Java 取消，点数以 Java 终态为准"))
	out, err := c.CancelRun(withBearer("Bearer fresh"), &canvas.RunIdReq{UserId: 7, Id: 21})
	if err != nil || out.Status != "cancelled" {
		t.Fatalf("%+v %v", out, err)
	}
	if got := fj.calls(); len(got) != 1 || got[0] != "JT1 Bearer fresh" {
		t.Fatalf("应带本次请求的 Bearer 取消 Java 任务: %v", got)
	}
	if c.s.Bearers.Get(7) != "Bearer fresh" {
		t.Fatal("本次请求的 Bearer 应刷新缓存（Poller 重试取消要用）")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestForceFailJavaWithoutBearerDegrades(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{cancel: javaOK}
	fj.start(t, c)
	mock.ExpectQuery(qRunGet).WillReturnRows(javaRunRow(1, nil, nil))
	expectEnd(mock, model.RunFailed, "运营已终止该任务：测试；Java 任务未能取消（无有效登录态），可能仍会执行并扣费")
	if _, err := c.AdminForceFailRun(context.Background(), &canvas.AdminRunReq{Id: 21, AdminId: 1, Reason: "测试"}); err != nil {
		t.Fatal(err)
	}
	if got := fj.calls(); len(got) != 0 {
		t.Fatalf("缓存没有令牌时不应调 Java: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestForceFailJavaUsesCachedBearer(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{cancel: javaOK}
	fj.start(t, c)
	c.s.Bearers.Put(7, "Bearer cached")
	mock.ExpectQuery(qRunGet).WillReturnRows(javaRunRow(1, nil, nil))
	expectEnd(mock, model.RunFailed, "运营已终止该任务；已请求 Java 取消，点数以 Java 终态为准")
	if _, err := c.AdminForceFailRun(context.Background(), &canvas.AdminRunReq{Id: 21}); err != nil {
		t.Fatal(err)
	}
	if got := fj.calls(); len(got) != 1 || got[0] != "JT1 Bearer cached" {
		t.Fatalf("应用缓存的令牌取消: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStopJavaOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		mode   javaStop
		cancel func(http.ResponseWriter)
		task   string
		to     int8
		reason string
	}{
		{"强制失败时 Java 已成功：照常失败并注明已扣费", stopAdmin, javaFinished,
			`{"code":0,"data":{"taskUid":"JT1","status":"SUCCEED","result":{"url":"https://j/r.png"}}}`, model.RunFailed, "运营已终止该任务；Java 任务已成功并已扣费"},
		{"取消时 Java 已失败", stopUser, javaFinished,
			`{"code":0,"data":{"taskUid":"JT1","status":"FAILED"}}`, model.RunCanceled, "已取消执行；Java 任务已结束（失败）"},
		{"任务不存在：停止跟踪", stopUser, javaNotFound, "", model.RunCanceled, "已取消执行；Java 任务已不存在，已停止跟踪"},
		{"令牌失效：退化", stopTimeout, java401, "", model.RunFailed, "工具执行超时；Java 任务未能取消（无有效登录态）"},
		{"画布已删：取消", stopGone, javaOK, "", model.RunCanceled, "画布已删除；已请求 Java 取消"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, mock := mockCore(t)
			fj := &fakeJava{cancel: tc.cancel, task: tc.task}
			fj.start(t, c)
			c.s.Bearers.Put(7, "Bearer b")
			base := map[javaStop]string{stopUser: "已取消执行", stopAdmin: "运营已终止该任务", stopTimeout: "工具执行超时", stopGone: "画布已删除"}[tc.mode]
			expectEnd(mock, tc.to, tc.reason)
			ended, pending := c.stopJava(context.Background(), runningJava(), tc.mode, base, "Bearer b", 0)
			if !ended || pending {
				t.Fatalf("ended=%v pending=%v", ended, pending)
			}
			if tc.cancel != nil && strings.Contains(tc.name, "失效") && c.s.Bearers.Get(7) != "" {
				t.Fatal("401 应清掉缓存的令牌")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStopJavaAlreadyFinishedButNotTerminalRetries(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{cancel: javaFinished, task: `{"code":0,"data":{"taskUid":"JT1","status":"RUNNING"}}`}
	fj.start(t, c)
	// 3001 但任务仍在跑（如「任务状态已变化」）：按暂时性登记待重试
	mock.ExpectExec("UPDATE `afv_canvas_node_run` AS `r` SET `error_code` = 'JAVA_CANCEL_PENDING:u:1:[0-9]+', `error_reason` = '已取消执行' WHERE \\(id = 21\\) AND \\(status = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	ended, pending := c.stopJava(context.Background(), runningJava(), stopUser, "已取消执行", "Bearer b", 0)
	if ended || !pending || fj.gets != 1 {
		t.Fatalf("ended=%v pending=%v gets=%d", ended, pending, fj.gets)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestJavaCancelTransientRetriesThenDegrades(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{cancel: java500}
	fj.start(t, c)
	c.s.Bearers.Put(7, "Bearer b")
	// 首次：登记待重试，run 保持运行中
	mock.ExpectExec("SET `error_code` = 'JAVA_CANCEL_PENDING:a:1:[0-9]+', `error_reason` = '运营已终止该任务' WHERE \\(id = 21\\) AND \\(status = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ended, pending := c.stopJava(context.Background(), runningJava(), stopAdmin, "运营已终止该任务", "Bearer b", 0); ended || !pending {
		t.Fatalf("ended=%v pending=%v", ended, pending)
	}
	// Poller 第 3 次重试（旧格式标记没有时间戳，视为已到点）：仍失败，计数 +1
	run := runningJava()
	run.ErrorCode = sql.NullString{String: "JAVA_CANCEL_PENDING:a:2", Valid: true}
	run.ErrorReason = sql.NullString{String: "运营已终止该任务", Valid: true}
	mock.ExpectExec("SET `error_code` = 'JAVA_CANCEL_PENDING:a:3:[0-9]+'").WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollJavaRun(context.Background(), run, false)
	// 第 6 次（退避表 5 项 + 首次）：达到上限，按停止跟踪置失败（强制失败场景）
	run.ErrorCode.String = "JAVA_CANCEL_PENDING:a:5"
	expectEnd(mock, model.RunFailed, "运营已终止该任务；Java 任务未能取消（取消接口多次失败），可能仍会执行并扣费")
	c.pollJavaRun(context.Background(), run, false)
	if n := len(fj.calls()); n != 3 {
		t.Fatalf("应调了 3 次取消: %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPollJavaGoneCancels(t *testing.T) {
	c, mock := mockCore(t)
	fj := &fakeJava{cancel: javaOK}
	fj.start(t, c)
	c.s.Bearers.Put(7, "Bearer b")
	expectEnd(mock, model.RunCanceled, "画布已删除，执行已取消；已请求 Java 取消，点数以 Java 终态为准")
	c.pollJavaRun(context.Background(), runningJava(), true)
	if got := fj.calls(); len(got) != 1 {
		t.Fatalf("画布已删应取消 Java 任务: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingJavaStopParse(t *testing.T) {
	r := &model.NodeRun{ErrorCode: sql.NullString{String: "JAVA_CANCEL_PENDING:t:3", Valid: true}}
	if m, n, ok := pendingJavaStop(r); !ok || m != stopTimeout || n != 3 {
		t.Fatalf("%c %d %v", m, n, ok)
	}
	for _, s := range []string{"TIMEOUT", "JAVA_CANCEL_PENDING:x:1", "JAVA_CANCEL_PENDING:u", "JAVA_CANCEL_PENDING:u:x"} {
		if _, _, ok := pendingJavaStop(&model.NodeRun{ErrorCode: sql.NullString{String: s, Valid: true}}); ok {
			t.Fatalf("%s 不是待取消标记", s)
		}
	}
}
