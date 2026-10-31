package logic

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"google.golang.org/grpc"
)

// billingOn 真扣费模式的计费服务，底下是一个没有任何预期的 sqlmock：每条 SQL 都报错（模拟计费库不可用 / 冻结失败）。
func billingOn(t *testing.T) *billing.Service {
	t.Helper()
	sdb, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return billing.New(billing.Conf{Mode: billing.ModeOn}, sqlx.NewSqlConnFromDB(sdb))
}

// recorder 记录收到的请求路径。
type recorder struct {
	mu    sync.Mutex
	paths []string
	body  map[string][]byte
}

func (r *recorder) add(path string, b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	if r.body == nil {
		r.body = map[string][]byte{}
	}
	r.body[path] = b
}

func (r *recorder) has(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.paths)
}

func fakeServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.add(r.URL.Path, b)
		h(w, r, b)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func gatewayRun(id int64, status int8) *model.NodeRun {
	return &model.NodeRun{
		ID: id, CanvasID: "cv_1", NodeID: "m", UserID: 7, RunID: "k", Kind: model.KindImage, Backend: model.BackendGateway,
		Status: status, BillingStatus: model.BillNone, RequestedCount: 1, Request: json.RawMessage(`{"model":"Banana"}`),
		TaskID: sql.NullString{String: "cv_t", Valid: true}, BillingRequestID: sql.NullString{String: "canvas-cv_t", Valid: true},
	}
}

var imageSpec = &runSpec{Kind: model.KindImage, Backend: model.BackendGateway, EntryCode: "canvas_image", Count: 1, ModelGroup: "img.nano-banana-2.1", Prompt: "p"}

// ---------------- 提交链路 ----------------

func TestHoldFailureFailsRun(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billingOn(t)
	run := gatewayRun(5, model.RunSubmitting)
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, `updated_at` = NOW\\(3\\), `billing_status` = 'none', `error_code` = 'BILLING_REJECTED'.* WHERE \\(`id` = 5\\) AND \\(`status` = 5\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := c.holdAndQueue(context.Background(), run, imageSpec)
	if _, _, ok := xerr.Parse(err); !ok || run.Status != model.RunFailed {
		t.Fatalf("冻结失败应置失败并回业务错误: status=%d err=%v", run.Status, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHoldAndQueueSurvivesCanceledCtx(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	run := gatewayRun(5, model.RunSubmitting)
	// 调用方 ctx 已取消（前端断开）：冻结后的 CAS 仍要用独立上下文写进去，不能留下「已冻结未写库」
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET `status` = 0, `updated_at` = NOW(3), `billing_status` = 'none' WHERE (`id` = 5) AND (`status` = 5)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := c.holdAndQueue(ctx, run, imageSpec); err != nil || run.Status != model.RunQueued {
		t.Fatalf("ctx 取消后仍应转排队: status=%d err=%v", run.Status, err)
	}
	// CAS 没抢到（提交中被取消）→ 4002，不当成功
	run2 := gatewayRun(6, model.RunSubmitting)
	mock.ExpectExec("WHERE \\(`id` = 6\\) AND \\(`status` = 5\\)").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := c.holdAndQueue(ctx, run2, imageSpec); err == nil {
		t.Fatal("提交中已被取消应返回错误")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNoTaskIDNoDegradedKey(t *testing.T) {
	c := testCore()
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { w.WriteHeader(500) })
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	run := gatewayRun(5, model.RunQueued)
	run.TaskID = sql.NullString{}
	if _, err := gatewayIdem(run, 0); err == nil {
		t.Fatal("task_id 为空必须报错，不能退化成 canvas::0")
	}
	if _, err := runKey(run); err == nil {
		t.Fatal("Java / studio 幂等键同理")
	}
	if _, err := c.submitGateway(context.Background(), run, imageSpec); err == nil || isTemporary(err) {
		t.Fatalf("空 task_id 应直接失败（非暂时性）: %v", err)
	}
	if rec.count() != 0 {
		t.Fatal("空 task_id 不能打到 gateway")
	}
}

func TestAdvanceSubmittingFallback(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.s.Config.Poll.BatchSize = 100
	mock.ExpectQuery("FROM `afv_canvas_node_run` AS `r` WHERE \\(r.status = 5\\) AND \\(r.created_at < .*\\) ORDER BY r.id ASC LIMIT 100").
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status", "backend", "billing_request_id"}).AddRow(9, 7, 5, "gateway", "canvas-cv_x"))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, `updated_at` = NOW\\(3\\), `billing_status` = 'refunded', `error_code` = 'SUBMIT_TIMEOUT'.*`points` = 0 WHERE \\(`id` = 9\\) AND \\(`status` = 5\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.advanceSubmitting(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------- 结算 / 轮询健壮性 ----------------

func TestSucceedSettleFailureMarksUnsettled(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billingOn(t) // Settle 查预留单报错（等价于预留单已被回收 / 库不可用）
	run := gatewayRun(5, model.RunRunning)
	run.BillingStatus, run.Points = model.BillHeld, 40
	// 先 CAS 成功（不写 settled），再结算；结算失败 → unsettled
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 2, `updated_at` = NOW\\(3\\), `candidates` = .* WHERE \\(`id` = 5\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` .*SET `billing_status` = 'unsettled'.*WHERE \\(id = 5\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.succeed(context.Background(), run, outcome{done: true, ok: true, actual: 1}, []model.Candidate{{URL: "https://c/1.png"}})
	if run.Status != model.RunSucceeded || run.BillingStatus != model.BillUnsettled {
		t.Fatalf("结算失败应标 unsettled: %+v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPollGatewayBatchGetFailureStillTimesOut(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "call_aaaaaaaaaa01", "status": "CANCELLED"})
			return
		}
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"down"}}`))
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: srv.URL, KeyID: "k", Secret: "s"})
	old := gatewayRun(11, model.RunRunning)
	old.BackendRef = sql.NullString{String: "call_aaaaaaaaaa01", Valid: true}
	old.CreatedAt = time.Now().Add(-time.Hour)
	fresh := gatewayRun(12, model.RunRunning)
	fresh.BackendRef = sql.NullString{String: "call_aaaaaaaaaa02", Valid: true}
	fresh.CreatedAt = time.Now()
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_code` = 'TIMEOUT'.* WHERE \\(`id` = 11\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollGateway(context.Background(), []*model.NodeRun{old, fresh})
	if !rec.has("/v1/calls/call_aaaaaaaaaa01/cancel") || rec.has("/v1/calls/call_aaaaaaaaaa02/cancel") {
		t.Fatalf("batchGet 失败仍应对超时的 run 取消上游、未超时的不动: %v", rec.paths)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPollJava401ClearsBearer(t *testing.T) {
	c, mock := mockCore(t)
	srv, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"code":2001,"msg":"未登录"}`))
	})
	c.s.Java = javatool.New(srv.URL, 5*time.Second)
	c.s.Bearers.Put(7, "Bearer old")
	run := &model.NodeRun{ID: 21, UserID: 7, Kind: model.KindToolUpscale, Backend: model.BackendJava, Status: model.RunRunning,
		BillingStatus: model.BillJava, BackendRef: sql.NullString{String: "JT1", Valid: true}, CreatedAt: time.Now().Add(-time.Hour)}
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_reason` = '工具执行超时；Java 任务未能取消（无有效登录态）.* WHERE \\(`id` = 21\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollJava(context.Background(), run)
	if c.s.Bearers.Get(7) != "" {
		t.Fatal("Java 401 应清掉缓存的令牌")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type fakeStudio struct {
	studio.Studio
	canceled []int64
}

func (f *fakeStudio) GetBatch(context.Context, *studio.GetBatchReq, ...grpc.CallOption) (*studio.BatchDetail, error) {
	return &studio.BatchDetail{Batch: &studio.BatchInfo{Status: 3}}, nil
}

func (f *fakeStudio) CancelBatch(_ context.Context, in *studio.BatchUserReq, _ ...grpc.CallOption) (*studio.CancelBatchResp, error) {
	f.canceled = append(f.canceled, in.BatchId)
	return &studio.CancelBatchResp{}, nil
}

func TestStudioTimeoutCancelsBatch(t *testing.T) {
	c, mock := mockCore(t)
	fs := &fakeStudio{}
	c.s.Studio = fs
	run := &model.NodeRun{ID: 31, UserID: 7, Kind: model.KindToolTryon, Backend: model.BackendStudio, Status: model.RunRunning,
		BillingStatus: model.BillStudio, BackendRef: sql.NullString{String: "88", Valid: true}, CreatedAt: time.Now().Add(-time.Hour)}
	mock.ExpectExec("WHERE \\(`id` = 31\\) AND \\(`status` = 1\\)").WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollStudio(context.Background(), run)
	if len(fs.canceled) != 1 || fs.canceled[0] != 88 {
		t.Fatalf("试穿超时应取消 studio 批次: %v", fs.canceled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceQueuedCancelsDeletedCanvas(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.s.Config.Poll.BatchSize = 100
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_canvas_node_run` AS `r` WHERE (r.status = 0) AND ((r.lease_until IS NULL OR r.lease_until < NOW(3))) ORDER BY r.updated_at ASC")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "canvas_id", "user_id", "status", "backend", "kind", "billing_status", "created_at"}).
			AddRow(41, "cv_gone", 7, 0, "gateway", "image", "held", time.Now()))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `id` FROM `afv_canvas` WHERE (id IN ('cv_gone')) AND (deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 4, .*`billing_status` = 'refunded'.* WHERE \\(`id` = 41\\) AND \\(`status` = 0\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	c.advanceQueued(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------- 蒙版 ----------------

// pngHeader 只有签名 + IHDR 的 PNG（声明尺寸 w×h，CRC 正确），DecodeConfig 能读、Decode 会失败。
func pngHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8 bit RGBA
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func maskPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for y := 2; y < 5; y++ {
		for x := 3; x < 7; x++ {
			img.Set(x, y, color.NRGBA{255, 255, 255, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestMaskRejectsHugeDeclaredSize(t *testing.T) {
	bomb := pngHeader(50000, 50000)
	if _, err := decodeMask(bomb); err == nil || !strings.Contains(err.Error(), "超出上限") {
		t.Fatalf("声明 50000² 的 PNG 应在解码前被拒: %v", err)
	}
	if _, err := decodeMask(pngHeader(4096, 4097)); err == nil {
		t.Fatal("单边超 4096 应拒")
	}
	// RunNode 入口：dataURL 先校验尺寸，不会走到媒体服务（Media=nil 时若放行会报「暂未开放」）
	c := testCore()
	du := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bomb)
	params, _ := json.Marshal(map[string]any{"mask_data_url": du})
	err := c.normalizeMask(context.Background(), &canvas.RunNodeRequest{Kind: "tool", Tool: "inpaint", ToolParams: string(params)})
	if err == nil || !strings.Contains(err.Error(), "超出上限") {
		t.Fatalf("入口应拒绝超大蒙版: %v", err)
	}
	// Pix 直读与 At 结果一致（RGBA / Gray）
	rgba := image.NewRGBA(image.Rect(0, 0, 8, 8))
	gray := image.NewGray(image.Rect(0, 0, 8, 8))
	for y := 1; y < 3; y++ {
		for x := 2; x < 6; x++ {
			rgba.Set(x, y, color.White)
			gray.Set(x, y, color.White)
		}
	}
	for _, m := range []image.Image{rgba, gray} {
		if b, ok := MaskBounds(m); !ok || b.X != 0.25 || b.Y != 0.125 || b.W != 0.5 || b.H != 0.25 {
			t.Fatalf("%T: %+v", m, b)
		}
	}
}

const testRID = "tst001-user-aaaaaaaaaaaaaaaaaaaaaaaaaa"

// fakeMedia 媒体服务：ingest / ingest/upload 返回 testRID，resources/urls 签出本服务的 /obj，/obj 回蒙版图。
func fakeMedia(t *testing.T, mask []byte) (*mediasvc.Client, *recorder) {
	t.Helper()
	var srv *httptest.Server
	srv, rec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		res := map[string]any{"object": map[string]any{"rid": testRID, "class": "work", "store": "private", "sha256": "abc", "mime": "image/png", "width": 10, "height": 10}}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/mediasvc/ingest"):
			_ = json.NewEncoder(w).Encode(res)
		case r.URL.Path == "/v1/mediasvc/resources/urls":
			_ = json.NewEncoder(w).Encode(map[string]any{"url": srv.URL + "/obj/mask.png"})
		case r.URL.Path == "/obj/mask.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(mask)
		default:
			w.WriteHeader(404)
		}
	})
	return mediasvc.New([]string{srv.URL}, "tok"), rec
}

func TestMaskURLGoesThroughMedia(t *testing.T) {
	inner, innerRec := fakeServer(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { w.WriteHeader(200) })
	media, rec := fakeMedia(t, maskPNG(t))
	c := testCore()
	c.s.Media = media
	maskURL := inner.URL + "/internal/mask.png" // 127.0.0.1：内网地址
	b, err := c.maskBox(context.Background(), gatewayRun(1, model.RunQueued), &runSpec{Params: map[string]any{"mask_url": maskURL}})
	if err != nil || b.X != 0.3 || b.Y != 0.2 {
		t.Fatalf("box=%+v err=%v", b, err)
	}
	if innerRec.count() != 0 {
		t.Fatal("本进程不能直接访问用户给的 mask_url")
	}
	var body struct {
		Source struct{ URL string } `json:"source"`
	}
	_ = json.Unmarshal(rec.body["/v1/mediasvc/ingest"], &body)
	if body.Source.URL != maskURL {
		t.Fatalf("mask_url 应交给媒体服务 ingest 拉取: %v %s", rec.paths, rec.body["/v1/mediasvc/ingest"])
	}
	// 媒体服务没配：不退回到本进程直连
	c.s.Media = nil
	if _, err := c.maskBox(context.Background(), gatewayRun(1, model.RunQueued), &runSpec{Params: map[string]any{"mask_url": maskURL}}); err == nil {
		t.Fatal("无媒体服务时不能自己下载")
	}
}

func TestNormalizeMaskStoresRID(t *testing.T) {
	media, rec := fakeMedia(t, maskPNG(t))
	c := testCore()
	c.s.Media = media
	du := "data:image/png;base64," + base64.StdEncoding.EncodeToString(maskPNG(t))
	params, _ := json.Marshal(map[string]any{"mask_data_url": du, "x": 1})
	req := &canvas.RunNodeRequest{Kind: "tool", Tool: "inpaint", ToolParams: string(params)}
	if err := c.normalizeMask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.ToolParams, "data:image") || !strings.Contains(req.ToolParams, `"mask_rid":"`+testRID+`"`) {
		t.Fatalf("request 列只存 rid: %s", req.ToolParams)
	}
	if !rec.has("/v1/mediasvc/ingest/upload") {
		t.Fatal("dataURL 应上传到媒体服务")
	}
	// 落库后的重提：按 rid 取回蒙版算框
	p, _ := parseParams(req.ToolParams)
	if b, err := c.maskBox(context.Background(), gatewayRun(1, model.RunQueued), &runSpec{Params: p}); err != nil || b.W != 0.4 {
		t.Fatalf("按 rid 算框: %+v %v", b, err)
	}
	// 客户端自带的 mask_rid 不认
	req2 := &canvas.RunNodeRequest{Kind: "tool", Tool: "inpaint", ToolParams: `{"mask_rid":"` + testRID + `","bbox":[]}`}
	_ = c.normalizeMask(context.Background(), req2)
	if strings.Contains(req2.ToolParams, "mask_rid") {
		t.Fatalf("mask_rid 只能由服务端写入: %s", req2.ToolParams)
	}
}

// ---------------- 回填 / 选候选 ----------------

func TestBackfillLatestFailedKeepsLastSuccess(t *testing.T) {
	d := mustDoc(t, `{"nodes":[{"id":"m","type":"model","data":{"model":"Banana","selectedCandidate":0}}],"edges":[]}`)
	cands, _ := json.Marshal([]model.Candidate{{URL: "https://c/1.png"}, {URL: "https://c/2.png"}})
	ok := &model.NodeRun{ID: 5, NodeID: "m", RunID: "r1", Kind: model.KindImage, Status: model.RunSucceeded, Candidates: cands, SelectedIndex: 1}
	bad := &model.NodeRun{ID: 6, NodeID: "m", RunID: "r2", Kind: model.KindImage, Status: model.RunFailed, ErrorReason: sql.NullString{String: "boom", Valid: true}}
	d.Backfill([]*model.NodeRun{bad}, []*model.NodeRun{ok}, func(*model.NodeRun) int32 { return 0 })
	m := d.Nodes[0].Data
	if m["status"] != "error" || m["errorMessage"] != "boom" || m["runId"] != "r2" {
		t.Fatalf("状态取最近一次: %+v", m)
	}
	if c, _ := m["candidates"].([]string); len(c) != 2 || m["image"] != "https://c/2.png" || m["selectedCandidate"] != 1 {
		t.Fatalf("成品取最近一次成功、选中以 run 为准: %+v", m)
	}
	// selectedCandidate 是用户数据：保存时不剥离
	d2 := mustDoc(t, `{"nodes":[{"id":"m","type":"model","data":{"selectedCandidate":1,"candidates":["x"]}}],"edges":[]}`)
	d2.StripRuntime(map[string]bool{"m": true})
	if _, ok := d2.Nodes[0].Data["selectedCandidate"]; !ok {
		t.Fatal("selectedCandidate 不应剥离")
	}
	if _, ok := d2.Nodes[0].Data["candidates"]; ok {
		t.Fatal("candidates 仍剥离")
	}
}

func TestSelectCandidate(t *testing.T) {
	c, mock := mockCore(t)
	cands, _ := json.Marshal([]model.Candidate{{URL: "https://c/1.png"}, {URL: "https://c/2.png"}})
	cols := []string{"id", "canvas_id", "node_id", "user_id", "run_id", "status", "candidates", "selected_index", "request"}
	expectCanvas(mock, "1-aaaa")
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.user_id = 7) AND (r.canvas_id = 'cv_1') AND (r.node_id = 'm') AND (r.run_id = 'r1') AND (r.deleted = 0)")).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(5, "cv_1", "m", 7, "r1", 2, string(cands), 0, `{}`))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET selected_index = 1 WHERE (id = 5)")).WillReturnResult(sqlmock.NewResult(0, 1))
	res, err := c.SelectCandidate(context.Background(), &canvas.SelectCandidateReq{UserId: 7, CanvasId: "cv_1", NodeId: "m", RunId: "r1", Index: 1})
	if err != nil || res.SelectedIndex != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	expectCanvas(mock, "1-aaaa")
	mock.ExpectQuery("FROM `afv_canvas_node_run`").WillReturnRows(sqlmock.NewRows(cols).AddRow(5, "cv_1", "m", 7, "r1", 2, string(cands), 0, `{}`))
	_, err = c.SelectCandidate(context.Background(), &canvas.SelectCandidateReq{UserId: 7, CanvasId: "cv_1", NodeId: "m", RunId: "r1", Index: 2})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("越界 1003: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------- 保存 / 校验 ----------------

func TestSaveRequiresIfMatchOrForce(t *testing.T) {
	c, mock := mockCore(t)
	expectCanvas(mock, "100-aaaa")
	_, err := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", Snapshot: snap2})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamMissing {
		t.Fatalf("无 if_match 且非 force 应拒绝: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInlineMediaFullScan(t *testing.T) {
	bad := []string{
		// data 嵌套层的文本键不再豁免
		`{"nodes":[{"id":"t","type":"text","data":{"mentions":[{"text":"data:image/png;base64,AA"}]}}],"edges":[]}`,
		// 节点外层
		`{"nodes":[{"id":"i","type":"image","style":{"backgroundImage":"data:image/png;base64,AA"},"data":{}}],"edges":[]}`,
		// 连线
		`{"nodes":[{"id":"t","type":"text","data":{}},{"id":"m","type":"model","data":{}}],"edges":[{"id":"e","source":"t","target":"m","sourceHandle":"text","targetHandle":"prompt","data":{"label":"blob:http://x/1"}}]}`,
		// 顶层
		`{"nodes":[],"edges":[],"background":"file:///etc/passwd"}`,
	}
	for i, s := range bad {
		if err := ValidateDoc(mustDoc(t, s), 100, nil); err == nil || !strings.Contains(err.Error(), "dataURL") {
			t.Fatalf("case %d 应拒绝: %v", i, err)
		}
	}
	ok := `{"nodes":[{"id":"t","type":"text","data":{"text":"data: 第一层文本键可以写 data:","prompt":"data:x"}}],"edges":[]}`
	if err := ValidateDoc(mustDoc(t, ok), 100, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyLastFrameRejected(t *testing.T) {
	c := testCore()
	req := &canvas.RunNodeRequest{Kind: "video", Model: "Seedance 2.0", Prompt: "走秀", Ratio: "16:9", Size: "720p", Duration: "4s",
		References: []*canvas.RunReference{{Handle: "image2", Image: "https://x/l.png"}}}
	if _, err := c.buildSpec(req, nil); err == nil || !strings.Contains(err.Error(), "尾帧") {
		t.Fatalf("只有尾帧应本地拦: %v", err)
	}
}

// ---------------- 官方模板种子 ----------------

// TestOfficialSeedTemplates 三份官方模板快照（从 SQL 里解析）都能过 ValidateDoc，且每个可执行的模型节点
// 按连线拼出的执行请求都能过 buildSpec + 执行前连线校验（element-replace 曾因参考视频接首帧口而跑不起来）。
func TestOfficialSeedTemplates(t *testing.T) {
	raw, err := os.ReadFile("../../../../../deploy/sql/seed/canvas-official-templates.sql")
	if err != nil {
		t.Fatal(err)
	}
	snaps := regexp.MustCompile(`(?m)^\s*'(\{"nodes".*\})',\s*$`).FindAllStringSubmatch(string(raw), -1)
	if len(snaps) != 3 {
		t.Fatalf("应解析出 3 份快照，得到 %d", len(snaps))
	}
	c := testCore()
	for _, m := range snaps {
		d, err := ParseSnapshot([]byte(m[1]), c.s.Config.Limits.MaxSnapshotBytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateDoc(d, c.s.Config.Limits.MaxNodes, c.imageLimit); err != nil {
			t.Fatalf("ValidateDoc: %v", err)
		}
		by := d.NodeByID()
		runs := 0
		for _, n := range d.Nodes {
			if n.Type != "model" || n.Data["inputOnly"] == true {
				continue
			}
			kind := "image"
			if n.Kind() == "video" {
				kind = "video"
			}
			req := &canvas.RunNodeRequest{Kind: kind, Model: strData(n.Data, "model"), Ratio: strData(n.Data, "ratio"), Size: strData(n.Data, "size"),
				Quality: strData(n.Data, "quality"), Duration: strData(n.Data, "duration"), Count: int32(intData(n.Data, "count")),
				Prompt: strData(n.Data, "prompt")}
			if s, ok := n.Data["sound"].(bool); ok {
				req.Sound = s
			}
			for _, e := range d.Edges {
				if e.Target != n.ID {
					continue
				}
				src := by[e.Source]
				ref := &canvas.RunReference{Handle: e.TargetHandle, Index: int32(e.Order), NodeId: src.ID, SourceHandle: e.SourceHandle}
				switch NormalizeHandle(e.TargetHandle) {
				case HandlePrompt:
					ref.Text = strData(src.Data, "text")
				case HandleVideo:
					ref.Video = "https://example.com/" + src.ID + ".mp4"
				default:
					ref.Image = "https://example.com/" + src.ID + ".png"
				}
				req.References = append(req.References, ref)
			}
			sp, err := c.buildSpec(req, nil)
			if err != nil {
				t.Fatalf("节点 %s buildSpec: %v", n.ID, err)
			}
			if err := c.checkRefs(d, &canvas.RunNodeReq{NodeId: n.ID, Request: req}, sp); err != nil {
				t.Fatalf("节点 %s 执行前连线校验: %v", n.ID, err)
			}
			runs++
		}
		if runs == 0 {
			t.Fatal("每份模板至少有一个可执行的模型节点")
		}
	}
}

// 视频按秒计费（Quote.Quantity = 秒数）：成功必须全额确认，不能把「成功 1 个视频」当成实际产出 1 秒按比例结算。
// image 按张：部分成功按成功张数结算。
func TestSettleQuantityByKind(t *testing.T) {
	cases := []struct {
		kind   model.RunKind
		actual int
		want   int
	}{
		{model.KindImage, 1, 1},
		{model.KindImage, 2, 2},
		{model.KindVideo, 1, 0},
		{model.KindToolI2P, 1, 0},
	}
	for _, c := range cases {
		if got := settleQuantity(c.kind, c.actual); got != c.want {
			t.Fatalf("%s actual=%d → %d，want %d", c.kind, c.actual, got, c.want)
		}
	}
}
