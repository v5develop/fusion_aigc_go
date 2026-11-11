package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	srcRID0  = "inst01-work-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	srcRID1  = "inst01-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"
	srcVideo = "inst01-work-cccccccccccccccccccccccccc"
)

// libMedia 媒体服务替身：resources/copy 按幂等键回素材库地址（failFirst 里的键先回 status 一次）；resources/urls 现签。
type libMedia struct {
	mu        sync.Mutex
	copies    map[string]int
	failFirst map[string]int // 键 → 状态码（只失败一次）
	failAll   int            // >0 时所有 copy 都回这个状态码
	signs     int
}

func newLibMedia(t *testing.T) (*mediasvc.Client, *libMedia) {
	m := &libMedia{copies: map[string]int{}, failFirst: map[string]int{}}
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.URL.Path {
		case "/v1/mediasvc/resources/copy":
			key := r.Header.Get("Idempotency-Key")
			m.copies[key]++
			code := m.failAll
			if c, ok := m.failFirst[key]; ok {
				code = c
				delete(m.failFirst, key)
			}
			if code > 0 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":{"code":"x","message":"copy failed"}}`))
				return
			}
			idx := key[strings.LastIndex(key, ":")+1:]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]any{"rid": "inst01-asset-dddddddddddddddddddddddddd", "width": 10, "height": 10},
				"url":    map[string]any{"kind": "public", "value": "https://cdn/lib" + idx},
			})
		case "/v1/mediasvc/resources/urls":
			m.signs++
			var req struct{ Rid string }
			_ = json.Unmarshal(body, &req)
			_ = json.NewEncoder(w).Encode(map[string]any{"url": "https://signed/" + req.Rid + "?Expires=600"})
		default:
			w.WriteHeader(404)
		}
	})
	return mediasvc.New([]string{srv.URL}, "tok"), m
}

func (m *libMedia) copyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.copies {
		n += v
	}
	return n
}

var copyRunCols = []string{"id", "canvas_id", "node_id", "user_id", "kind", "backend", "status", "billing_status",
	"requested_count", "backend_ref", "task_id", "request", "candidates", "video_url", "error_code", "deleted", "created_at"}

func expectRunRow(mock sqlmock.Sqlmock, status int8, kind, cands string, video any, deleted int) {
	mock.ExpectQuery("WHERE \\(r.id = 5\\) LIMIT 1").WillReturnRows(sqlmock.NewRows(copyRunCols).
		AddRow(5, "cv_1", "m", 7, kind, "gateway", status, model.BillSettled, 1, callA, "cv_t", `{"model":"Banana"}`, cands, video, nil, deleted, time.Now()))
}

func asyncCore(t *testing.T) (*core, sqlmock.Sqlmock, *libMedia, *[]int64) {
	c, mock := mockCore(t)
	c.s.Config.Library.AsyncCopy = true
	media, lm := newLibMedia(t)
	c.s.Media = media
	var kicked []int64
	c.s.KickCopy = func(id int64) { kicked = append(kicked, id) }
	return c, mock, lm, &kicked
}

func copyJob(attempt int32) *model.CopyJob {
	return &model.CopyJob{ID: 9, RunPK: 5, TaskID: "cv_t", UserID: 7, Attempt: attempt, CreatedAt: time.Now()}
}

func refCands(rids ...string) string {
	var list []model.Candidate
	for _, r := range rids {
		list = append(list, model.Candidate{RID: r, URL: mrRef(r, "png"), W: 8, H: 8})
	}
	b, _ := json.Marshal(list)
	return string(b)
}

// 交付先于转存：落 outbox → CAS 成功（候选是 mr:// 引用，不是签名地址）→ 结算 → 交给转存池；收尾期间不调媒体服务复制。
func TestAsyncSucceedVisibleBeforeCopy(t *testing.T) {
	c, mock, lm, kicked := asyncCore(t)
	c.s.Billing = billingOn(t) // 替身计费库没有预留单：结算报错 → unsettled，证明结算照常执行且不等转存
	run := gatewayRun(5, model.RunRunning)
	run.BillingStatus = model.BillHeld
	run.RequestedCount = 2
	// 置成功与落 outbox 同一事务（先 CAS 再落行，同生同灭）
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*mr://" + srcRID0 + "\\.png.*mr://" + srcRID1 + "\\.webp.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT IGNORE INTO `afv_canvas_copy_job` .*VALUES \\(.*5, 'cv_t', 7, 0, 0, NOW\\(3\\) \\+ INTERVAL 10 SECOND\\)").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` AS `r` SET .*`billing_status` = 'unsettled'.*WHERE \\(id = 5\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.succeed(context.Background(), run, outcome{done: true, ok: true, actual: 2, outputs: []mediaOutput{
		{RID: srcRID0, Mime: "image/png", W: 8, H: 8}, {RID: srcRID1, Mime: "image/webp", W: 8, H: 8}}}, nil)
	if run.Status != model.RunSucceeded {
		t.Fatal("应立即置成功")
	}
	if lm.copyCount() != 0 {
		t.Fatalf("收尾不应同步转存: %v", lm.copies)
	}
	if len(*kicked) != 1 || (*kicked)[0] != 5 {
		t.Fatalf("应交给转存池: %v", *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 视频：封面与成片都写 mr://（成片按 mime 带 .mp4），读出口现签，library_status=pending。
func TestAsyncSucceedVideoRefsResolvedOnRead(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	run := gatewayRun(5, model.RunRunning)
	run.Kind = model.KindVideo
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*mr://" + srcRID0 + "\\.jpg.*`video_url` = 'mr://" + srcVideo + "\\.mp4'.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT IGNORE INTO `afv_canvas_copy_job`").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	c.succeed(context.Background(), run, outcome{done: true, ok: true, actual: 1,
		video: &mediaOutput{RID: srcVideo, Mime: "video/mp4"}, cover: &mediaOutput{RID: srcRID0, Mime: "image/jpeg"}}, nil)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	stored := &model.NodeRun{ID: 5, Status: model.RunSucceeded, Kind: model.KindVideo,
		Candidates: json.RawMessage(`[{"rid":"` + srcRID0 + `","url":"mr://` + srcRID0 + `.jpg"}]`),
		VideoURL:   sql.NullString{String: "mr://" + srcVideo + ".mp4", Valid: true}}
	res := c.toResult(context.Background(), stored)
	if res.LibraryStatus != LibraryPending || !strings.HasPrefix(res.VideoUrl, "https://signed/"+srcVideo) ||
		len(res.Candidates) != 1 || !strings.HasPrefix(res.Candidates[0].Url, "https://signed/"+srcRID0) {
		t.Fatalf("读出口应现签视频与封面并标 pending: %+v", res)
	}
	if lm.signs != 2 {
		t.Fatalf("应各签一次: %d", lm.signs)
	}
	// 画布回填同样带 libraryStatus，且保存时会被剥离
	d, _ := ParseSnapshot([]byte(`{"nodes":[{"id":"m","type":"model","data":{}}],"edges":[]}`), 0)
	stored.NodeID = "m"
	d.Backfill(nil, c.resolvedRuns(context.Background(), []*model.NodeRun{stored}), c.progressOf)
	if d.Nodes[0].Data["libraryStatus"] != LibraryPending || !strings.HasPrefix(d.Nodes[0].Data["video"].(string), "https://signed/") {
		t.Fatalf("回填应带 libraryStatus 与现签视频: %v", d.Nodes[0].Data)
	}
	d.StripRuntime(map[string]bool{"m": true})
	if _, ok := d.Nodes[0].Data["libraryStatus"]; ok {
		t.Fatal("libraryStatus 不能落进快照")
	}
}

// 落 outbox 失败（如表还没建）：本次退回同步转存，全部转存完才置成功（候选是素材库地址）。
func TestAsyncEnqueueFailureFallsBackToSyncCopy(t *testing.T) {
	c, mock, lm, kicked := asyncCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	run := gatewayRun(5, model.RunRunning)
	// 事务里 CAS 成功后落 outbox 失败 → 整个事务回滚（run 仍是运行中）→ 同步转存 → 再 CAS 成功（素材库地址）
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*mr://.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT IGNORE INTO `afv_canvas_copy_job`").WillReturnError(errors.New("Error 1146: Table doesn't exist"))
	mock.ExpectRollback()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, .*https://cdn/lib0.* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.succeed(context.Background(), run, outcome{done: true, ok: true, actual: 1, outputs: []mediaOutput{{RID: srcRID0}}}, nil)
	if run.Status != model.RunSucceeded || lm.copies["canvas-copy:cv_t:0"] != 1 || len(*kicked) != 0 {
		t.Fatalf("应同步转存后成功: status=%d copies=%v kicked=%v", run.Status, lm.copies, *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 后台转存：暂时失败 → 退避重试（不动 run）；重试用同一批幂等键 → 换成素材库地址 → 完成。
func TestCopyJobRetryThenSwap(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	lm.failFirst["canvas-copy:cv_t:1"] = 500
	cands := refCands(srcRID0, srcRID1)

	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1, next_at = NOW\\(3\\) \\+ INTERVAL 300 SECOND WHERE \\(id = 9\\) AND \\(status = 0\\) AND \\(attempt = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", cands, nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = NOW\\(3\\) \\+ INTERVAL 5 SECOND, last_error = .* WHERE \\(id = 9\\) AND \\(status = 0\\) AND \\(attempt = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.runCopyJob(context.Background(), copyJob(0))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1.* AND \\(attempt = 1\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", cands, nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = GREATEST\\(NOW\\(3\\) \\+ INTERVAL 300 SECOND, next_at \\+ INTERVAL 1000 MICROSECOND\\) WHERE \\(id = 9\\) AND \\(status = 0\\) AND \\(attempt = 2\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET candidates = '.*https://cdn/lib0.*https://cdn/lib1.*', error_code = IF\\(error_code = 'LIBRARY_COPY_FAILED', NULL, error_code\\) WHERE \\(id = 5\\) AND \\(status = 2\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = GREATEST").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 1, finished_at = NOW\\(3\\) WHERE \\(id = 9\\) AND \\(status = 0\\) AND \\(attempt = 2\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.runCopyJob(context.Background(), copyJob(1))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(lm.copies) != 2 || lm.copies["canvas-copy:cv_t:0"] != 2 || lm.copies["canvas-copy:cv_t:1"] != 2 {
		t.Fatalf("两轮只用同一批幂等键: %v", lm.copies)
	}
}

// 已换成素材库地址（上一轮换完、标记完成前崩溃）：重做不再复制，只补完成。
func TestCopyJobRerunAfterSwapIsNoop(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", `[{"rid":"inst01-asset-dddddddddddddddddddddddddd","url":"https://cdn/lib0"}]`, nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = GREATEST").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 1").WillReturnResult(sqlmock.NewResult(0, 1))
	c.runCopyJob(context.Background(), copyJob(3))
	if lm.copyCount() != 0 {
		t.Fatalf("不应再复制: %v", lm.copies)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 永久失败（4xx）：run 打 LIBRARY_COPY_FAILED、outbox 记永久失败；不动账；产物仍以 mr:// 现签可看（library_status=failed）。
func TestCopyJobPermanentFailureKeepsOutputVisible(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	lm.failAll = 404
	cands := refCands(srcRID0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", cands, nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET error_code = 'LIBRARY_COPY_FAILED' WHERE \\(id = 5\\) AND \\(status = 2\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 2, finished_at = NOW\\(3\\), last_error = .* AND \\(attempt = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.runCopyJob(context.Background(), copyJob(0)) // c.s.Billing 为 nil：任何动账都会 panic
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	failed := &model.NodeRun{ID: 5, Status: model.RunSucceeded, Kind: model.KindImage, Candidates: json.RawMessage(cands),
		ErrorCode: sql.NullString{String: model.CopyFailedCode, Valid: true}}
	res := c.toResult(context.Background(), failed)
	if res.LibraryStatus != LibraryFailed || len(res.Candidates) != 1 || !strings.HasPrefix(res.Candidates[0].Url, "https://signed/"+srcRID0) || res.Error != "" {
		t.Fatalf("永久失败后产物仍应可看、标 failed、不当失败展示: %+v", res)
	}
}

// 暂时失败超过 MaxAttempts：同样记永久失败。
func TestCopyJobGivesUpAfterMaxAttempts(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	c.s.Config.Library.MaxAttempts = 3
	lm.failAll = 500
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1.* AND \\(attempt = 2\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", refCands(srcRID0), nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET error_code = 'LIBRARY_COPY_FAILED'").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 2").WillReturnResult(sqlmock.NewResult(0, 1))
	c.runCopyJob(context.Background(), copyJob(2))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// run 已不是成功态 / 已删除：作废，不复制。
func TestCopyJobVoidWhenRunGone(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", refCands(srcRID0), nil, 1)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 3").WillReturnResult(sqlmock.NewResult(0, 1))
	c.runCopyJob(context.Background(), copyJob(0))
	if lm.copyCount() != 0 {
		t.Fatal("已删除的 run 不应转存")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 置成功的 CAS 没抢到（已取消 / 强制失败）：事务回滚，不留 outbox、不结算、不交给转存池。
func TestAsyncSucceedCASLostLeavesNoJob(t *testing.T) {
	c, mock, _, kicked := asyncCore(t)
	run := gatewayRun(5, model.RunRunning)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit() // CAS 没写到任何行，提交空事务；关键是没有 INSERT outbox
	c.succeed(context.Background(), run, outcome{done: true, ok: true, actual: 1, outputs: []mediaOutput{{RID: srcRID0}}}, nil)
	if run.Status == model.RunSucceeded || len(*kicked) != 0 {
		t.Fatalf("CAS 没抢到不应成功 / 转存: status=%d kicked=%v", run.Status, *kicked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 置成功事务报错（非落 outbox 失败）：什么都没写，本轮不收尾（下轮重做），不退回同步转存。
func TestAsyncSucceedTxErrorRetriesNextRound(t *testing.T) {
	c, mock, lm, kicked := asyncCore(t)
	run := gatewayRun(5, model.RunRunning)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2").WillReturnError(errors.New("deadlock"))
	mock.ExpectRollback()
	c.succeed(context.Background(), run, outcome{done: true, ok: true, actual: 1, outputs: []mediaOutput{{RID: srcRID0}}}, nil)
	if run.Status == model.RunSucceeded || len(*kicked) != 0 || lm.copyCount() != 0 {
		t.Fatalf("事务失败应整轮放弃: status=%d kicked=%v copies=%v", run.Status, *kicked, lm.copies)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// outbox 指着还没结束的 run（正常不会出现）：不作废，退避后再看——作废后 run 再成功时 INSERT IGNORE 是空操作，就永远没人转存了。
func TestCopyJobRunNotFinishedRetriesInsteadOfVoid(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunRunning, "image", refCands(srcRID0), nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = NOW\\(3\\) \\+ INTERVAL 5 SECOND, last_error = .* AND \\(attempt = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.runCopyJob(context.Background(), copyJob(0))
	if lm.copyCount() != 0 {
		t.Fatal("run 未结束不应转存")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 复制期间认领已被别的实例接手（复制超过认领时长）：旧持有者不换引用、不入素材库、不写终态。
func TestCopyJobClaimTakenOverDuringCopyStopsWriting(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 1))
	expectRunRow(mock, model.RunSucceeded, "image", refCands(srcRID0), nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = GREATEST").WillReturnResult(sqlmock.NewResult(0, 0))
	c.runCopyJob(context.Background(), copyJob(0))
	if lm.copyCount() != 1 {
		t.Fatalf("复制照做（同幂等键，无害）: %v", lm.copies)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 认领没抢到（别的实例 / 协程在做）：什么都不做。
func TestCopyJobClaimLost(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 0))
	c.runCopyJob(context.Background(), copyJob(0))
	if lm.copyCount() != 0 {
		t.Fatal("没抢到认领不应转存")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 并发：即时转存与扫描同时拿到同一行，attempt CAS 只放一个进去，每个键只复制一次。
func TestCopyJobConcurrentClaimSingleWinner(t *testing.T) {
	c, mock, lm, _ := asyncCore(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET attempt = attempt \\+ 1").WillReturnResult(sqlmock.NewResult(0, 0))
	expectRunRow(mock, model.RunSucceeded, "image", refCands(srcRID0), nil, 0)
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = GREATEST").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET next_at = GREATEST").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET candidates").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_copy_job` SET status = 1").WillReturnResult(sqlmock.NewResult(0, 1))
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runCopyJob(context.Background(), copyJob(0))
		}()
	}
	wg.Wait()
	if lm.copyCount() != 1 {
		t.Fatalf("只应复制一次: %v", lm.copies)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 转存池：同一 run 本进程只起一个；关闭后不再接新的。
func TestCopierDedupAndClose(t *testing.T) {
	c, _ := mockCore(t)
	c.s.Config.Library.Workers = 2
	ctx, cancel := context.WithCancel(context.Background())
	p := newCopier(ctx, c)
	release := make(chan struct{})
	if !p.spawn(5, func(context.Context) { <-release }) {
		t.Fatal("第一次应起协程")
	}
	if p.spawn(5, func(context.Context) {}) {
		t.Fatal("同一 run 不应重复起")
	}
	close(release)
	cancel()
	p.close()
	if p.spawn(6, func(context.Context) {}) {
		t.Fatal("关闭后不应再起")
	}
}

func TestLibraryStatusAndRefs(t *testing.T) {
	r := &model.NodeRun{Status: model.RunSucceeded, Candidates: json.RawMessage(refCands(srcRID0))}
	if libraryStatus(r) != LibraryPending {
		t.Fatal("私有引用应是 pending")
	}
	r.Candidates = json.RawMessage(`[{"url":"mrw://` + srcRID0 + `.png"}]`)
	if libraryStatus(r) != "" {
		t.Fatal("studio 水印视图不归这里转存")
	}
	r.Candidates = json.RawMessage(`[{"url":"mr://inst01-asset-dddddddddddddddddddddddddd.png"}]`)
	if libraryStatus(r) != "" {
		t.Fatal("素材库 rid 的引用视为已在素材库")
	}
	r.Status = model.RunRunning
	r.Candidates = json.RawMessage(refCands(srcRID0))
	if libraryStatus(r) != "" {
		t.Fatal("非成功 run 不报 library_status")
	}
	if mimeExt("image/jpeg", false) != "jpg" || mimeExt("", true) != "mp4" || mimeExt("", false) != "png" || refExtOf("mr://x.webp") != "webp" {
		t.Fatal("扩展名映射不对")
	}
}
