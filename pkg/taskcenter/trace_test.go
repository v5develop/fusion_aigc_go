package taskcenter

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/pkg/taskevent"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func traceSource() Source {
	return Source{
		Table: "afv_digital_human", Prefix: "dh", EntryCode: "digitalHuman", Type: "video", Trace: "digitalhuman",
		Status: "CASE status WHEN 2 THEN 2 WHEN 3 THEN 3 ELSE 1 END", Fail: "fail_reason",
		BillingRequest: "s.`video_task_id`", ExecTaskID: "video_task_id",
	}
}

type qcall struct {
	q    string
	args []any
}

// fakeConn：Exec 记录；QueryRowsPartialCtx 按 rowsFor 返回源表行 / 预留单；gate 非 nil 时查询先等它（模拟慢库）。
type fakeConn struct {
	sqlx.SqlConn
	mu      sync.Mutex
	execs   []string
	queries []qcall
	rowsFor func(q string, args []any) any
	gate    chan struct{}
	qerr    error
}

func (c *fakeConn) ExecCtx(_ context.Context, q string, _ ...any) (sql.Result, error) {
	c.mu.Lock()
	c.execs = append(c.execs, q)
	c.mu.Unlock()
	return nil, nil
}

func (c *fakeConn) QueryRowsPartialCtx(ctx context.Context, v any, q string, args ...any) error {
	c.mu.Lock()
	c.queries = append(c.queries, qcall{q, args})
	c.mu.Unlock()
	if c.qerr != nil {
		return c.qerr
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.rowsFor != nil {
		if res := c.rowsFor(q, args); res != nil {
			dst := reflect.ValueOf(v).Elem()
			if rv := reflect.ValueOf(res); rv.Type() == dst.Type() {
				dst.Set(rv)
			}
		}
	}
	return nil
}

func (c *fakeConn) qs() []qcall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]qcall(nil), c.queries...)
}

// evDB 记录异步写入器落库的事件行。
type evDB struct {
	mu   sync.Mutex
	rows [][]any
}

func (d *evDB) ExecCtx(_ context.Context, _ string, args ...any) (sql.Result, error) {
	d.mu.Lock()
	for i := 0; i+taskevent.RowArgs <= len(args); i += taskevent.RowArgs {
		d.rows = append(d.rows, args[i:i+taskevent.RowArgs])
	}
	d.mu.Unlock()
	return nil, nil
}

func (d *evDB) snapshot() [][]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]any(nil), d.rows...)
}

func sv(v any) string {
	if ns, ok := v.(sql.NullString); ok {
		return ns.String
	}
	s, _ := v.(string)
	return s
}

// 取某一列（kind=3 step=2 status=4 msg=8 dedupe=16）按 step+kind 找行
func findEv(rows [][]any, step, kind string) [][]any {
	var out [][]any
	for _, r := range rows {
		if sv(r[2]) == step && sv(r[3]) == kind {
			out = append(out, r)
		}
	}
	return out
}

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

const tsA = "2026-11-13 10:00:00.000000"
const tsB = "2026-11-13 10:05:00.000000"

func newTraceSyncer(conn *fakeConn) (*Syncer, *evDB) {
	db := &evDB{}
	y := New(conn, time.Minute, traceSource()).WithEvents(taskevent.New(db))
	return y, db
}

func flushEv(t *testing.T, y *Syncer) {
	t.Helper()
	y.WaitEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := y.events.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

// 派生事件只用普通 SELECT：主表 upsert 是唯一的 exec，没有任何 INSERT … SELECT 事件语句，也没有 REGEXP_REPLACE / FOR UPDATE / EXISTS 全表模式。
func TestDeriveUsesPlainSelectsNotInsertSelect(t *testing.T) {
	conn := &fakeConn{}
	y, _ := newTraceSyncer(conn)
	if err := y.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	flushEv(t, y)
	if len(conn.execs) != 1 || !strings.HasPrefix(conn.execs[0], "INSERT INTO afv_creation_task") {
		t.Fatalf("只应有主表 upsert 这一条写语句: %v", conn.execs)
	}
	qs := conn.qs()
	if len(qs) != 2 {
		t.Fatalf("应有源表行 + 预留单两条查询: %d", len(qs))
	}
	for _, q := range qs {
		up := strings.ToUpper(q.q)
		if !strings.HasPrefix(q.q, "SELECT ") || strings.Contains(up, "INSERT") || strings.Contains(up, "FOR UPDATE") ||
			strings.Contains(up, "REGEXP_REPLACE") || strings.Contains(up, "EXISTS") || strings.Contains(up, "LOCK IN SHARE") {
			t.Fatalf("派生查询必须是普通 SELECT: %s", q.q)
		}
		if strings.Count(q.q, "?") != len(q.args) {
			t.Fatalf("占位符与参数不一致: %s %v", q.q, q.args)
		}
	}
}

// 首轮（secs=0）只覆盖创建时间下限窗口，不做全量回填；之后窗口轮仍受下限约束；行数上限作为 LIMIT 参数。
func TestDeriveRespectsFloorAndCap(t *testing.T) {
	conn := &fakeConn{}
	y, _ := newTraceSyncer(conn)
	y.WithTrace(2*time.Hour, 77)
	_ = y.Sync(context.Background()) // 首轮：全量同步
	flushEv(t, y)
	first := conn.qs()[0]
	if !strings.Contains(first.q, "s.create_time >= NOW() - INTERVAL ? SECOND") || strings.Contains(first.q, "s.update_time >= NOW()") {
		t.Fatalf("首轮只按创建时间下限: %s", first.q)
	}
	if len(first.args) != 2 || first.args[0] != int64(7200) || first.args[1] != 77 {
		t.Fatalf("首轮参数 = [floor秒, cap]: %v", first.args)
	}
	conn.mu.Lock()
	conn.queries = nil
	conn.mu.Unlock()
	_ = y.Sync(context.Background()) // 窗口轮
	flushEv(t, y)
	w := conn.qs()[0]
	if !strings.Contains(w.q, "s.create_time >= NOW() - INTERVAL ? SECOND") || !strings.Contains(w.q, "s.update_time >= NOW() - INTERVAL ? SECOND") {
		t.Fatalf("窗口轮 = 创建下限 + update 窗口: %s", w.q)
	}
	if len(w.args) != 3 || w.args[0] != int64(7200) || w.args[1] != int64(60) || w.args[2] != 77 {
		t.Fatalf("窗口轮参数: %v", w.args)
	}
	if !strings.Contains(w.q, "LIMIT ?") {
		t.Fatal("缺行数上限")
	}
}

// 派生在 y.mu 之外异步跑：查询卡住时 Sync 照样返回，并发的第二个 Sync 也不被它挡住。
func TestDeriveRunsOutsideLock(t *testing.T) {
	conn := &fakeConn{gate: make(chan struct{})}
	y, _ := newTraceSyncer(conn)
	done := make(chan error, 2)
	go func() { done <- y.Sync(context.Background()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("派生卡住时 Sync 不应被拖住")
	}
	go func() { done <- y.Sync(context.Background()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("第二个 Sync 被派生挡住了")
	}
	close(conn.gate)
	y.WaitEvents()
}

// dedupe 带状态：失败 → 之后成功，两个 span_end 都记；预留单 CONFIRMED → 之后 REFUNDED 各记一条；失败原因在 Go 里脱敏。
func TestDeriveDedupeKeepsLaterStateChanges(t *testing.T) {
	status := int64(3)
	resvStatus := "CONFIRMED"
	conn := &fakeConn{rowsFor: func(q string, _ []any) any {
		if strings.Contains(q, "afv_billing_reservation") {
			return []traceResv{{BizID: "7_req1", Status: ns(resvStatus), Points: ns("12"), Updated: ns(tsB)}}
		}
		return []traceRow{{ID: 5, Created: ns(tsA), Updated: ns(tsB), Status: sql.NullInt64{Int64: status, Valid: true},
			Ext: ns("vt_1"), Fail: ns("拉取 https://oss.x.com/a.png?sig=abc 失败 token=zzz"), BKey: ns("7_req1")}}
	}}
	y, db := newTraceSyncer(conn)
	_ = y.Sync(context.Background())
	flushEv(t, y)
	status, resvStatus = 2, "REFUNDED"
	_ = y.Sync(context.Background())
	flushEv(t, y)
	rows := db.snapshot()

	ends := findEv(rows, "go.digitalhuman/run", "span_end")
	var got []string
	for _, r := range ends {
		got = append(got, sv(r[16])+"|"+sv(r[4]))
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	if !seen["span_end:go.digitalhuman/run:failed|failed"] || !seen["span_end:go.digitalhuman/run:ok|ok"] {
		t.Fatalf("failed 与 ok 的 span_end 都应带状态 dedupe: %v", got)
	}
	if m := sv(ends[0][8]); strings.Contains(m, "sig=") || strings.Contains(m, "zzz") || !strings.Contains(m, "[url]") {
		t.Fatalf("失败原因应在 Go 里脱敏: %q", m)
	}
	settle := findEv(rows, "go.digitalhuman/settle", "mark")
	refund := findEv(rows, "go.digitalhuman/refund", "mark")
	if len(settle) == 0 || sv(settle[0][16]) != "mark:go.digitalhuman/settle:CONFIRMED" || len(refund) == 0 || sv(refund[0][16]) != "mark:go.digitalhuman/refund:REFUNDED" {
		t.Fatalf("settle / refund 应带预留状态: %v %v", settle, refund)
	}
	if sv(settle[0][0]) != Ref("dh", 5) {
		t.Fatalf("task_id 应是任务中心编号: %v", settle[0][0])
	}
	if sub := findEv(rows, "go.digitalhuman/submitted", "mark"); len(sub) == 0 || sv(sub[0][11]) != "exec_task" || sv(sub[0][12]) != "vt_1" {
		t.Fatalf("submitted: %v", sub)
	}
	if len(findEv(rows, "go.digitalhuman/created", "mark")) == 0 || len(findEv(rows, "go.digitalhuman/run", "span_start")) == 0 {
		t.Fatal("缺 created / run 开始")
	}
}

// 预留单在窗口内变过、源行没动（到期回收退款）：按 biz_id 用 IN 取回源行补事件；同一 (biz_id,状态,时间) 只查一次。
func TestDeriveReservationDrivenRowsViaIN(t *testing.T) {
	conn := &fakeConn{rowsFor: func(q string, args []any) any {
		switch {
		case strings.Contains(q, "afv_billing_reservation r"):
			return []traceResv{{BizID: "7_old", Status: ns("EXPIRED"), Points: ns("5"), Updated: ns(tsB)}, {BizID: "9_other", Status: ns("CONFIRMED"), Updated: ns(tsB)}}
		case strings.Contains(q, ") IN (?,?)"):
			return []traceRow{{ID: 8, Created: ns(tsA), Updated: ns(tsA), Status: sql.NullInt64{Int64: 3, Valid: true}, BKey: ns("7_old")}}
		}
		return nil // 窗口里没有源行
	}}
	y, db := newTraceSyncer(conn)
	_ = y.Sync(context.Background())
	flushEv(t, y)
	var in []qcall
	for _, q := range conn.qs() {
		if strings.Contains(q.q, " IN (") {
			in = append(in, q)
		}
	}
	if len(in) != 1 || in[0].args[1] != "7_old" && in[0].args[1] != "9_other" {
		t.Fatalf("应有一次 IN 查询: %+v", in)
	}
	if r := findEv(db.snapshot(), "go.digitalhuman/refund", "mark"); len(r) != 1 || sv(r[0][16]) != "mark:go.digitalhuman/refund:EXPIRED" || sv(r[0][0]) != Ref("dh", 8) {
		t.Fatalf("到期回收应补 refund 事件: %v", r)
	}
	_ = y.Sync(context.Background()) // 窗口轮：已处理过的预留单不再 IN 查询
	flushEv(t, y)
	n := 0
	for _, q := range conn.qs() {
		if strings.Contains(q.q, " IN (") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("同一预留单状态不应重复查，IN 次数=%d", n)
	}
}

// 事件拼装 / 查询失败从不影响 Sync；Trace 为空不派生。
func TestDeriveNeverFailsSyncAndSkipsWithoutTrace(t *testing.T) {
	conn := &fakeConn{}
	s := traceSource()
	s.Trace = ""
	y := New(conn, time.Minute, s)
	if err := y.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	y.WaitEvents()
	if len(conn.qs()) != 0 || y.events != nil {
		t.Fatal("Trace 为空不应派生 / 建写入器")
	}
	// 派生查询报错：Sync 照样成功，没有事件落库
	c2 := &fakeConn{qerr: context.DeadlineExceeded}
	y2, db := newTraceSyncer(c2)
	if err := y2.Sync(context.Background()); err != nil {
		t.Fatalf("派生查询失败不应影响 Sync: %v", err)
	}
	flushEv(t, y2)
	if len(db.snapshot()) != 0 {
		t.Fatal("查询失败不应有事件")
	}
}
