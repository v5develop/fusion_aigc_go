package taskcenter

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]SyncMode{"": ModeLegacy, "legacy": ModeLegacy, " NEW ": ModeNew, "Shadow": ModeShadow} {
		if got, ok := ParseMode(in); !ok || got != want {
			t.Errorf("ParseMode(%q) = %v,%v", in, got, ok)
		}
	}
	if got, ok := ParseMode("bogus"); ok || got != ModeLegacy {
		t.Errorf("非法取值应按 legacy 且 ok=false: %v %v", got, ok)
	}
}

// 计费状态映射与 legacy 的 CASE 表达式逐项一致。
func TestBillingFieldsMapping(t *testing.T) {
	s := Source{BillingRequest: "x"}
	cases := map[string]string{
		"HELD": "HELD", "CONFIRMED": "CONFIRMED", "PARTIALLY_REFUNDED": "PARTIALLY_REFUNDED", "REFUNDED": "REFUNDED",
		"CANCELLED": "REFUNDED", "EXPIRED": "REFUNDED", "WEIRD": "BILLING_PENDING_REVIEW", "": "BILLING_PENDING_REVIEW",
	}
	for in, want := range cases {
		got, pts, feat, _, no := s.billingFields(&resvRow{Status: sql.NullString{String: in, Valid: true},
			Points: sql.NullInt64{Int64: 9, Valid: true}, Feature: sql.NullString{String: "f", Valid: true}, No: sql.NullString{String: "N", Valid: true}})
		if got != want || pts.Int64 != 9 || feat.String != "f" || no.String != "N" {
			t.Errorf("status %q -> %q (want %q)", in, got, want)
		}
	}
	// 没有预留单 / 没配计费：NONE + 全 NULL
	if st, p, f, m, n := s.billingFields(nil); st != "NONE" || p.Valid || f.Valid || m.Valid || n.Valid {
		t.Error("无预留单应为 NONE 且字段全 NULL")
	}
	if st, p, _, _, _ := (Source{}).billingFields(&resvRow{Status: sql.NullString{String: "HELD", Valid: true}, Points: sql.NullInt64{Int64: 1, Valid: true}}); st != "NONE" || p.Valid {
		t.Error("没配 BillingRequest 时不同步计费字段")
	}
}

func TestBuildTarget(t *testing.T) {
	s := Source{Table: "t", Prefix: "cw", EntryCode: "e", Type: "copywriting", Status: "1", Result: "u", BillingRequest: "x", ResultText: "body"}
	r := srcRow{ID: 7, UserID: 3, Status: sql.NullInt64{Int64: 2, Valid: true}, Ext: sql.NullString{String: "E1", Valid: true},
		BKey: sql.NullString{String: "3_x", Valid: true}, Result: sql.NullString{String: "http://r", Valid: true},
		Created: "2026-01-01 00:00:00.000000", Updated: "2026-01-02 00:00:00.000000", Text: sql.NullString{String: "正文", Valid: true}}
	resv := map[string]resvRow{"3_x": {BizID: "3_X", Status: sql.NullString{String: "EXPIRED", Valid: true}}}
	subs := map[string]sql.NullString{"e1": {String: `{"a":1}`, Valid: true}} // 大小写不敏感，同 utf8mb4_unicode_ci
	tr, tx := s.buildTarget(r, resv, subs)
	if tr.TaskID != Ref("cw", 7) || tr.OriginRef != "cw:7" || tr.OriginService != OriginLocalLife || tr.Billing != "REFUNDED" ||
		tr.ExecParams.String != `{"a":1}` || tx == nil || tx.Content != "正文" || tx.TaskID != tr.TaskID {
		t.Fatalf("%+v %+v", tr, tx)
	}
	// 空正文不写文本表
	r.Text = sql.NullString{String: "", Valid: true}
	if _, tx := s.buildTarget(r, resv, subs); tx != nil {
		t.Error("空正文不应产生文本行")
	}
	// 封面缺省取 Result
	if got := s.coverExpr(); got != "u" {
		t.Errorf("coverExpr=%q", got)
	}
}

// ON DUPLICATE KEY UPDATE 的列与条件只有一处定义：legacy 与新路径必须共用。
func TestUpsertClauseSharedWithLegacy(t *testing.T) {
	s := Source{Table: "t", Prefix: "x", EntryCode: "e", Type: "image", Status: "1"}
	if !strings.Contains(s.sql(), upsertUpdateClause) || !strings.Contains(s.sql(), upsertColumns) {
		t.Fatal("legacy 没有使用共享的 upsert 列 / 更新子句")
	}
	q := upsertSQL(3)
	if strings.Count(q, "?") != 3*upsertColumnCount || !strings.HasSuffix(q, upsertUpdateClause) {
		t.Fatalf("多行 upsert 语句不对: %s", q)
	}
	if got := len(strings.Split(upsertColumns, ",")); got != upsertColumnCount {
		t.Fatalf("列数常量 %d != %d", upsertColumnCount, got)
	}
	if n := len((targetRow{}).args()); n != upsertColumnCount {
		t.Fatalf("targetRow.args %d 个，与列数 %d 不符", n, upsertColumnCount)
	}
	for _, kw := range []string{"GREATEST(afv_creation_task.deleted", "COALESCE(VALUES(execution_params), execution_params)"} {
		if !strings.Contains(q, kw) {
			t.Errorf("缺少 %s", kw)
		}
	}
}

// syncFake 只实现新路径用到的两个方法。
type syncFake struct {
	sqlx.SqlConn
	rows     []srcRow // 源表数据（已按 update_time,id 排序）
	txCalls  int
	txErrs   []error // 依次返回的 TransactCtx 错误（用完后成功）
	upserted []string
	queries  []string
}

func (c *syncFake) QueryRowsPartialCtx(_ context.Context, v any, q string, args ...any) error {
	c.queries = append(c.queries, q)
	p, ok := v.(*[]srcRow)
	if !ok {
		return nil // 预留单 / 提交快照 / 窗口：空
	}
	limit := int(args[len(args)-1].(int))
	var out []srcRow
	switch len(args) {
	case 1: // 全量
		out = c.rows
	case 4: // after: t, t, id, limit
		at, aid := args[0].(string), args[2].(int64)
		for _, r := range c.rows {
			if r.Updated > at || (r.Updated == at && r.ID > aid) {
				out = append(out, r)
			}
		}
	case 3: // from, overlap, limit
		from := args[0].(string)
		for _, r := range c.rows {
			if r.Updated >= from {
				out = append(out, r)
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	*p = out
	return nil
}

type syncFakeSess struct {
	sqlx.Session
	c *syncFake
}

func (s syncFakeSess) ExecCtx(_ context.Context, q string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(q, "INSERT INTO afv_creation_task ") {
		for i := 0; i < len(args); i += upsertColumnCount {
			s.c.upserted = append(s.c.upserted, args[i].(string))
		}
	}
	return nil, nil
}

func (c *syncFake) TransactCtx(ctx context.Context, fn func(context.Context, sqlx.Session) error) error {
	c.txCalls++
	if len(c.txErrs) > 0 {
		e := c.txErrs[0]
		c.txErrs = c.txErrs[1:]
		return e
	}
	return fn(ctx, syncFakeSess{c: c})
}

func srow(id int64, upd string) srcRow {
	return srcRow{ID: id, UserID: 1, Status: sql.NullInt64{Int64: 1, Valid: true}, Created: upd, Updated: upd}
}

func testSyncer(c *syncFake) *Syncer {
	y := New(c, 10*time.Minute, Source{Table: "t", Prefix: "p", EntryCode: "e", Type: "image", Status: "1"}).WithMode(ModeNew)
	y.nw.sleep = func(time.Duration) {}
	return y
}

// 首轮分页读全表；页数上限到了转 catch-up，下一轮从游标续读直到追平；稳态不重复写没变的行。
func TestScanPagingCatchupAndDedupe(t *testing.T) {
	c := &syncFake{}
	for i := int64(1); i <= 5; i++ {
		c.rows = append(c.rows, srow(i, "2026-01-01 00:00:0"+string(rune('0'+i))+".000000"))
	}
	y := testSyncer(c).WithSyncLimits(2, 2, 100, 0) // 每页 2 行、每轮 2 页
	ctx := context.Background()
	if err := y.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := y.nw.cur[0].st; got != stCatchup || len(c.upserted) != 4 {
		t.Fatalf("首轮应读到页数上限转 catch-up：st=%v upserted=%d", got, len(c.upserted))
	}
	if err := y.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if y.nw.cur[0].st != stSteady || y.nw.cur[0].wm != "2026-01-01 00:00:05.000000" || len(c.upserted) != 5 {
		t.Fatalf("第二轮应追平：%+v upserted=%d", y.nw.cur[0], len(c.upserted))
	}
	// 稳态：回看窗口里的行再读到，但整行摘要相同 → 不写
	before := c.txCalls
	if err := y.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c.txCalls != before {
		t.Fatalf("没有变化的行不应再写：事务 %d → %d", before, c.txCalls)
	}
	// 行变了（update_time 前进）→ 写
	c.rows[4] = srow(5, "2026-01-01 00:00:09.000000")
	if err := y.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c.txCalls != before+1 || c.upserted[len(c.upserted)-1] != Ref("p", 5) {
		t.Fatalf("变化的行应写入：tx=%d upserted=%v", c.txCalls, c.upserted)
	}
}

// 写失败：游标不前进（稳态下 wm 不动），下一轮重来；成功后才推进。
func TestScanFailureKeepsWatermark(t *testing.T) {
	c := &syncFake{rows: []srcRow{srow(1, "2026-01-01 00:00:01.000000")}}
	y := testSyncer(c)
	c.txErrs = []error{&mysql.MySQLError{Number: 1062, Message: "dup"}} // 不可重试
	if err := y.Sync(context.Background()); err == nil {
		t.Fatal("写失败应返回错误")
	}
	if y.nw.cur[0].st != stFresh || y.synced[0] {
		t.Fatalf("失败后不应前进: %+v synced=%v", y.nw.cur[0], y.synced[0])
	}
	if err := y.Sync(context.Background()); err != nil || y.nw.cur[0].st != stSteady || !y.synced[0] {
		t.Fatalf("重试成功后应追平: %v %+v", err, y.nw.cur[0])
	}
}

// 1213 / 1205 整批重试（退避），其它错误不重试；批内按 task_id 排序（统一加锁顺序）。
func TestUpsertRetryOnDeadlock(t *testing.T) {
	c := &syncFake{}
	y := testSyncer(c)
	var slept []time.Duration
	y.nw.sleep = func(d time.Duration) { slept = append(slept, d) }
	c.txErrs = []error{&mysql.MySQLError{Number: 1213}, &mysql.MySQLError{Number: 1205}}
	rows := []targetRow{{TaskID: "b"}, {TaskID: "a"}, {TaskID: "c"}}
	if err := y.upsertBatch(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	if c.txCalls != 3 || len(slept) != 2 {
		t.Fatalf("应重试 2 次后成功: tx=%d sleeps=%v", c.txCalls, slept)
	}
	if !reflect.DeepEqual(c.upserted, []string{"a", "b", "c"}) {
		t.Fatalf("批内应按 task_id 排序: %v", c.upserted)
	}
	// 重试次数用尽
	c2 := &syncFake{txErrs: []error{&mysql.MySQLError{Number: 1213}, &mysql.MySQLError{Number: 1213}, &mysql.MySQLError{Number: 1213}, &mysql.MySQLError{Number: 1213}}}
	y2 := testSyncer(c2)
	if err := y2.upsertBatch(context.Background(), rows, nil); !retryable(err) || c2.txCalls != len(retryBackoff)+1 {
		t.Fatalf("用尽重试应返回原错误: err=%v tx=%d", err, c2.txCalls)
	}
	// 不可重试
	c3 := &syncFake{txErrs: []error{&mysql.MySQLError{Number: 1048}}}
	if err := testSyncer(c3).upsertBatch(context.Background(), rows, nil); err == nil || c3.txCalls != 1 {
		t.Fatalf("非死锁错误不应重试: %v tx=%d", err, c3.txCalls)
	}
}

// 新路径的取数语句必须是普通 SELECT，不得出现任何加锁子句 / INSERT … SELECT。
func TestNewReadsAreNonLocking(t *testing.T) {
	s := Source{Table: "t", Prefix: "p", EntryCode: "e", Type: "image", Status: "1", BillingRequest: "s.`r`", ResultText: "body", ExecTaskID: "tid", Kind: "k", Include: "a=1"}
	var qs []string
	for _, f := range []scanFilter{{limit: 1}, {from: "x", overlap: 5, limit: 1}, {after: &pos{"x", 1}, limit: 1}, {bkeys: 2, limit: 1}, {ids: 2, limit: 1}, {window: 60, limit: 1}} {
		q, _ := s.newRowsSQL(f)
		qs = append(qs, q)
	}
	qs = append(qs, resvByKeysSQL(3))
	for _, q := range qs {
		u := strings.ToUpper(q)
		if !strings.HasPrefix(u, "SELECT") || strings.Contains(u, "FOR UPDATE") || strings.Contains(u, "LOCK IN SHARE MODE") || strings.Contains(u, "INSERT") {
			t.Errorf("取数语句不是普通 SELECT: %s", q)
		}
		if strings.Contains(q, "afv_billing_reservation r") || strings.Contains(q, "EXISTS") {
			t.Errorf("源表查询不应再含预留单相关子查询: %s", q)
		}
	}
}
