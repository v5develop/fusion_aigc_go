package taskcenter

// 新同步实现（SyncMode=new）。语义与 legacy 逐列一致（见 SYNC_SPEC.md），只换取数和写入的方式：
//
//	读：全部是普通 SELECT（RR 下为快照读，不加任何锁），源表行、预留单、提交快照各自单独读，
//	    源行用 (update_time, id) 水位 + 每页行数上限分页，预留单只读窗口内变过的 biz_id；
//	算：源行里的 SQL 表达式（Status / Result / RequestPairs …）仍由 MySQL 在 SELECT 里求值（语义不变），
//	    预留单 → billing_status / 点数、提交快照 → execution_params、task_id / origin_ref 在 Go 里拼；
//	写：多行 INSERT … VALUES … ON DUPLICATE KEY UPDATE（更新列与条件同 legacy，见 upsertUpdateClause），
//	    按批、短事务、按 task_id 排序（统一加锁顺序）、1213 / 1205 退避重试；
//	    只写 afv_creation_task / afv_creation_task_text，绝不对 afv_billing_reservation 与源表加锁。
//
// 为什么不用 bun 读：取数列是每个 Source 各自配的 SQL 表达式（动态表名 / 列表达式），静态结构体映射表达不了；
// 且 taskcenter 的公开 API 以 sqlx.SqlConn 为底（老服务也在用），trace.go 同样走 sqlx 普通读。
//
// 水位放内存（不落库）的理由：legacy 的 synced 标记也在内存，首轮全量，新实现首轮同样从头分页读全表
// （普通读，不锁），覆盖面等价；落库要多一张状态表，并引入「水位比数据新」的跨宕机风险；
// 稳态每轮从 水位 - overlap（默认等于同步窗口 10 分钟）起读，与 legacy 的窗口覆盖一致，且不依赖本机时钟。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SyncMode 同步实现。
type SyncMode string

const (
	// ModeLegacy INSERT … SELECT（默认，线上现状）。
	ModeLegacy SyncMode = "legacy"
	// ModeNew 普通 SELECT + Go 拼装 + 批量 upsert。
	ModeNew SyncMode = "new"
	// ModeShadow 旧路径写；新路径只算不写，与旧路径写入的结果逐列比对，不一致记 判据=TASKCENTER_SHADOW_DIFF。
	ModeShadow SyncMode = "shadow"
)

// ModeEnv 选择同步实现的环境变量（所有用 taskcenter 的服务统一生效；也可用 Syncer.WithMode 显式指定）。
const ModeEnv = "TASKCENTER_SYNC_MODE"

// ParseMode 解析模式名（大小写不敏感）；空串 = legacy。
func ParseMode(s string) (SyncMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "legacy":
		return ModeLegacy, true
	case "new":
		return ModeNew, true
	case "shadow":
		return ModeShadow, true
	}
	return ModeLegacy, false
}

// DefaultMode 读 ModeEnv；取值非法时按 legacy 并记警告。
func DefaultMode() SyncMode {
	raw := os.Getenv(ModeEnv)
	m, ok := ParseMode(raw)
	if !ok {
		logx.Errorf("taskcenter: %s=%q 不是 legacy|new|shadow，按 legacy 运行", ModeEnv, raw)
	}
	return m
}

// WithMode 指定同步实现（必须在首次 Sync 之前调用）。
func (y *Syncer) WithMode(m SyncMode) *Syncer {
	if y != nil {
		y.mu.Lock()
		y.mode = m
		y.mu.Unlock()
	}
	return y
}

// Mode 当前同步实现。
func (y *Syncer) Mode() SyncMode {
	if y == nil {
		return ModeLegacy
	}
	return y.mode
}

// WithSyncLimits 调整新路径的分页参数（≤0 保持默认）：pageRows 每页源行数、pageCap 每轮最多页数、
// batchRows 每个 upsert 事务的行数、overlap 稳态回看时长（默认 = 同步窗口）。
func (y *Syncer) WithSyncLimits(pageRows, pageCap, batchRows int, overlap time.Duration) *Syncer {
	if y == nil {
		return y
	}
	if pageRows > 0 {
		y.nw.pageRows = pageRows
	}
	if pageCap > 0 {
		y.nw.pageCap = pageCap
	}
	if batchRows > 0 {
		y.nw.batchRows = batchRows
	}
	if overlap > 0 {
		y.nw.overlap = overlap
	}
	return y
}

const (
	defaultPageRows  = 500
	defaultPageCap   = 20
	defaultBatchRows = 100
	resvPageRows     = 1000
	resvPageCap      = 5
	keyChunk         = 200 // 按预留单 biz_id 反查源行 / 批量取预留单的 IN 列表长度
	sentMax          = 50000
	seenMax          = 50000
	tsLayout         = "'%Y-%m-%d %H:%i:%s.%f'"
)

// upsertColumns / upsertUpdateClause legacy 与新路径共用：ON DUPLICATE KEY UPDATE 的列与条件只有这一处定义。
const upsertColumns = "task_id, user_id, type, status, entry_code, request_params, execution_task_id, execution_params, " +
	"billing_status, price_points, feature_code, model_id, reservation_no, " +
	"result_url, result_urls, cover_url, error_msg, is_transferred, deleted, create_time, update_time, origin_service, origin_ref"

const upsertColumnCount = 23

const upsertUpdateClause = "ON DUPLICATE KEY UPDATE status = VALUES(status), result_url = VALUES(result_url), result_urls = VALUES(result_urls), " +
	"cover_url = VALUES(cover_url), error_msg = VALUES(error_msg), request_params = VALUES(request_params), " +
	// 没有提交快照的行（上线前提交的）不覆盖已有的执行参数，免得同步一次就被清空
	"execution_task_id = VALUES(execution_task_id), execution_params = COALESCE(VALUES(execution_params), execution_params), " +
	"billing_status = VALUES(billing_status), price_points = VALUES(price_points), feature_code = VALUES(feature_code), " +
	"model_id = VALUES(model_id), reservation_no = VALUES(reservation_no), " +
	"deleted = GREATEST(afv_creation_task.deleted, VALUES(deleted)), update_time = VALUES(update_time), " +
	"origin_service = VALUES(origin_service), origin_ref = VALUES(origin_ref)"

// pos 排序位置 (update_time, id)；t 为 'YYYY-MM-DD HH:MM:SS.ffffff'，可直接字典序比较。
type pos struct {
	t  string
	id int64
}

type scanState int

const (
	stFresh   scanState = iota // 还没做过全量：从头分页
	stCatchup                  // 分页没读完（行数多）：从 after 续读，不回看
	stSteady                   // 已追平：从 wm - overlap 读
)

type cursor struct {
	st    scanState
	wm    string // 已追平到的最大 update_time（空 = 还没有数据）
	after pos
}

type newState struct {
	cur  []cursor
	sent []map[string]uint64 // 每源：task_id → 上次成功写入的整行摘要（相同则跳过，回看窗口内不重复写）
	rcur cursor              // 预留单窗口水位（所有源共用，biz_type=LOCALLIFE）
	rok  bool

	seenResv map[string]struct{}

	pageRows, pageCap, batchRows int
	overlap                      time.Duration

	sleep func(time.Duration)

	shadow *shadowState
}

func newNewState(n int) newState {
	st := newState{
		cur: make([]cursor, n), sent: make([]map[string]uint64, n),
		pageRows: defaultPageRows, pageCap: defaultPageCap, batchRows: defaultBatchRows,
		seenResv: map[string]struct{}{}, sleep: time.Sleep,
	}
	for i := range st.sent {
		st.sent[i] = map[string]uint64{}
	}
	st.shadow = newShadowState()
	return st
}

func (y *Syncer) overlapDur() time.Duration {
	if y.nw.overlap > 0 {
		return y.nw.overlap
	}
	return y.window
}

// ---------- 读 ----------

// srcRow 源表一行（SQL 表达式求值后的结果）。
type srcRow struct {
	ID      int64          `db:"id"`
	UserID  int64          `db:"user_id"`
	Status  sql.NullInt64  `db:"status"`
	Request sql.NullString `db:"request_params"`
	Ext     sql.NullString `db:"ext"`
	Result  sql.NullString `db:"result_url"`
	Results sql.NullString `db:"result_urls"`
	Cover   sql.NullString `db:"cover_url"`
	Fail    sql.NullString `db:"error_msg"`
	Deleted int64          `db:"deleted"`
	Created string         `db:"create_time"`
	Updated string         `db:"upd_time"`
	BKey    sql.NullString `db:"bkey"`
	Text    sql.NullString `db:"result_text"`
}

type resvRow struct {
	BizID   string         `db:"biz_id"`
	Status  sql.NullString `db:"status"`
	Points  sql.NullInt64  `db:"price_points"`
	Feature sql.NullString `db:"feature_code"`
	ModelID sql.NullInt64  `db:"model_id"`
	No      sql.NullString `db:"reservation_no"`
}

type resvChange struct {
	BizID   string `db:"biz_id"`
	Updated string `db:"t"`
	ID      int64  `db:"id"`
}

type subRow struct {
	ExecID string         `db:"exec_task_id"`
	Input  sql.NullString `db:"input"`
}

// scanFilter 源行查询的取数条件。
type scanFilter struct {
	from    string // 非空：update_time >= from - overlapSecs
	overlap int64  // 秒
	after   *pos   // 非空：(update_time, id) > after
	bkeys   int    // >0：按计费键 IN（个数）
	ids     int    // >0：按主键 IN（个数）
	window  int64  // >0：update_time >= NOW() - window 秒（shadow 用）
	limit   int
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// bkeyExpr 计费键表达式；没配计费时为 NULL。
func (s Source) bkeyExpr() string {
	if strings.TrimSpace(s.BillingRequest) == "" {
		return "NULL"
	}
	return s.billingKeySQL()
}

// newRowsSQL 一页源行的普通 SELECT（无任何锁）。参数顺序：from, overlap | after.t, after.t, after.id | keys... | ids... | window，最后 limit。
func (s Source) newRowsSQL(f scanFilter) (string, []any) {
	upd := s.updateCol()
	conds := s.filters()
	var args []any
	switch {
	case f.after != nil:
		conds = append(conds, "("+upd+" > ? OR ("+upd+" = ? AND s.id > ?))")
		args = append(args, f.after.t, f.after.t, f.after.id)
	case f.from != "":
		conds = append(conds, upd+" >= DATE_SUB(CAST(? AS DATETIME(6)), INTERVAL ? SECOND)")
		args = append(args, f.from, f.overlap)
	case f.window > 0:
		conds = append(conds, upd+" >= NOW() - INTERVAL ? SECOND")
		args = append(args, f.window)
	}
	q := "SELECT s.id AS id, s.user_id AS user_id, (" + s.Status + ") AS status, " + s.requestSQL() + " AS request_params, " +
		orNull(s.ExecTaskID) + " AS ext, " + orNull(s.Result) + " AS result_url, " + orNull(s.Results) + " AS result_urls, " +
		orNull(s.coverExpr()) + " AS cover_url, " + orNull(s.Fail) + " AS error_msg, s.deleted AS deleted, " +
		"DATE_FORMAT(" + s.createCol() + ", " + tsLayout + ") AS create_time, DATE_FORMAT(" + upd + ", " + tsLayout + ") AS upd_time, " +
		s.bkeyExpr() + " AS bkey, " + orNull(s.ResultText) + " AS result_text FROM " + s.Table + " s"
	if f.bkeys > 0 {
		conds = append(conds, "("+s.bkeyExpr()+") IN ("+marks(f.bkeys)+")")
	}
	if f.ids > 0 {
		conds = append(conds, "s.id IN ("+marks(f.ids)+")")
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY " + upd + ", s.id LIMIT ?"
	return q, args
}

func (s Source) coverExpr() string {
	if strings.TrimSpace(s.Cover) == "" {
		return s.Result
	}
	return s.Cover
}

func (y *Syncer) queryRows(ctx context.Context, dst any, q string, args ...any) error {
	return y.conn.QueryRowsPartialCtx(ctx, dst, q, args...)
}

func (y *Syncer) fetchSrc(ctx context.Context, s Source, f scanFilter, extra ...any) ([]srcRow, error) {
	q, args := s.newRowsSQL(f)
	// 参数顺序：游标 / 窗口参数在前，IN 列表居中，limit 最后（见 newRowsSQL 中条件的拼接顺序）
	args = append(args, extra...)
	args = append(args, f.limit)
	var rows []srcRow
	if err := y.queryRows(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	return rows, nil
}

// normKey 与 MySQL utf8mb4_unicode_ci 比较（大小写不敏感、忽略尾随空格）近似的键归一化，用于 Go 侧 map 对键。
func normKey(k string) string { return strings.ToLower(strings.TrimRight(k, " ")) }

// resvByKeysSQL 按计费键批量取预留单（普通读）。与 legacy 的相关子查询（LIMIT 1，走 uk_biz(biz_type,biz_id,attempt_no)）
// 取同一行：attempt_no 最小者；同键只取第一行。
func resvByKeysSQL(n int) string {
	return "SELECT biz_id, status, price_points, feature_code, model_id, reservation_no FROM afv_billing_reservation " +
		"WHERE biz_type = 'LOCALLIFE' AND deleted = 0 AND biz_id IN (" + marks(n) + ") ORDER BY biz_id, attempt_no, id"
}

func (y *Syncer) loadResv(ctx context.Context, keys []string) (map[string]resvRow, error) {
	out := map[string]resvRow{}
	for len(keys) > 0 {
		n := min(len(keys), keyChunk)
		chunk := keys[:n]
		keys = keys[n:]
		var rs []resvRow
		args := make([]any, 0, n)
		for _, k := range chunk {
			args = append(args, k)
		}
		if err := y.queryRows(ctx, &rs, resvByKeysSQL(n), args...); err != nil {
			return nil, err
		}
		for _, r := range rs {
			k := normKey(r.BizID)
			if _, dup := out[k]; !dup {
				out[k] = r
			}
		}
	}
	return out, nil
}

func (y *Syncer) loadSubs(ctx context.Context, ids []string) (map[string]sql.NullString, error) {
	out := map[string]sql.NullString{}
	for len(ids) > 0 {
		n := min(len(ids), keyChunk)
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, 0, n)
		for _, k := range chunk {
			args = append(args, k)
		}
		var rs []subRow
		q := "SELECT exec_task_id, input FROM " + SubmissionTable + " WHERE exec_task_id IN (" + marks(n) + ")"
		if err := y.queryRows(ctx, &rs, q, args...); err != nil {
			return nil, err
		}
		for _, r := range rs {
			out[normKey(r.ExecID)] = r.Input
		}
	}
	return out, nil
}

// ---------- 算 ----------

// targetRow afv_creation_task 要写的一行（列与 upsertColumns 一一对应）。
type targetRow struct {
	TaskID, Type, EntryCode, OriginService, OriginRef string
	UserID                                            int64
	Status                                            sql.NullInt64
	Request, Ext, ExecParams                          sql.NullString
	Billing                                           string
	Points                                            sql.NullInt64
	Feature                                           sql.NullString
	ModelID                                           sql.NullInt64
	ResNo                                             sql.NullString
	Result, Results, Cover, Fail                      sql.NullString
	Deleted                                           int64
	Created, Updated                                  string
}

type textRow struct {
	TaskID, Content, Created, Updated string
}

func (t targetRow) args() []any {
	return []any{t.TaskID, t.UserID, t.Type, t.Status, t.EntryCode, t.Request, t.Ext, t.ExecParams,
		t.Billing, t.Points, t.Feature, t.ModelID, t.ResNo,
		t.Result, t.Results, t.Cover, t.Fail, 1, t.Deleted, t.Created, t.Updated, t.OriginService, t.OriginRef}
}

func (t targetRow) digest(tx *textRow) uint64 {
	h := fnv.New64a()
	for _, a := range t.args() {
		fmt.Fprintf(h, "%v\x00", a)
	}
	if tx != nil {
		h.Write([]byte(tx.Content))
	}
	return h.Sum64()
}

// billingFields 预留单 → (billing_status, price_points, feature_code, model_id, reservation_no)。
// 没配计费 / 没有预留单：NONE + 全 NULL。EXPIRED 是预扣过期释放，点数已回到余额，等同退款。
func (s Source) billingFields(r *resvRow) (string, sql.NullInt64, sql.NullString, sql.NullInt64, sql.NullString) {
	if strings.TrimSpace(s.BillingRequest) == "" || r == nil {
		return "NONE", sql.NullInt64{}, sql.NullString{}, sql.NullInt64{}, sql.NullString{}
	}
	var st string
	switch strings.ToUpper(strings.TrimRight(r.Status.String, " ")) {
	case "HELD":
		st = "HELD"
	case "CONFIRMED":
		st = "CONFIRMED"
	case "PARTIALLY_REFUNDED":
		st = "PARTIALLY_REFUNDED"
	case "REFUNDED", "CANCELLED", "EXPIRED":
		st = "REFUNDED"
	default:
		st = "BILLING_PENDING_REVIEW"
	}
	return st, r.Points, r.Feature, r.ModelID, r.No
}

// buildTarget 一行源记录 → 任务中心行（+ 有正文时的文本行）。
func (s Source) buildTarget(r srcRow, resv map[string]resvRow, subs map[string]sql.NullString) (targetRow, *textRow) {
	var rv *resvRow
	if r.BKey.Valid && resv != nil {
		if v, ok := resv[normKey(r.BKey.String)]; ok {
			rv = &v
		}
	}
	var execParams sql.NullString
	if r.Ext.Valid && subs != nil {
		execParams = subs[normKey(r.Ext.String)]
	}
	bs, pts, feat, mid, no := s.billingFields(rv)
	ref := Ref(s.Prefix, r.ID)
	t := targetRow{
		TaskID: ref, UserID: r.UserID, Type: s.Type, Status: r.Status, EntryCode: s.EntryCode,
		Request: r.Request, Ext: r.Ext, ExecParams: execParams,
		Billing: bs, Points: pts, Feature: feat, ModelID: mid, ResNo: no,
		Result: r.Result, Results: r.Results, Cover: r.Cover, Fail: r.Fail,
		Deleted: r.Deleted, Created: r.Created, Updated: r.Updated,
		OriginService: OriginLocalLife, OriginRef: OriginRef(s.Prefix, r.ID),
	}
	var tx *textRow
	if strings.TrimSpace(s.ResultText) != "" && r.Text.Valid && r.Text.String != "" {
		tx = &textRow{TaskID: ref, Content: r.Text.String, Created: r.Created, Updated: r.Updated}
	}
	return t, tx
}

// ---------- 写 ----------

func (y *Syncer) sleepFn() func(time.Duration) {
	if y.nw.sleep != nil {
		return y.nw.sleep
	}
	return time.Sleep
}

// retryable 死锁（1213）/ 锁等待超时（1205）可以整批重来。
func retryable(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

var retryBackoff = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond, 400 * time.Millisecond}

func upsertSQL(n int) string {
	one := "(" + marks(upsertColumnCount) + ")"
	return "INSERT INTO afv_creation_task (" + upsertColumns + ") VALUES " +
		strings.TrimSuffix(strings.Repeat(one+",", n), ",") + " " + upsertUpdateClause
}

func upsertTextSQL(n int) string {
	return "INSERT INTO " + TextTable + " (task_id, content, create_time, update_time) VALUES " +
		strings.TrimSuffix(strings.Repeat("(?,?,?,?),", n), ",") +
		" ON DUPLICATE KEY UPDATE content = VALUES(content), update_time = VALUES(update_time)"
}

// upsertBatch 一批行一个短事务（主表 + 正文表）；按 task_id 排序统一加锁顺序，死锁 / 锁超时退避重试。
func (y *Syncer) upsertBatch(ctx context.Context, rows []targetRow, texts []textRow) error {
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })
	sort.Slice(texts, func(i, j int) bool { return texts[i].TaskID < texts[j].TaskID })
	args := make([]any, 0, len(rows)*upsertColumnCount)
	for _, r := range rows {
		args = append(args, r.args()...)
	}
	targs := make([]any, 0, len(texts)*4)
	for _, t := range texts {
		targs = append(targs, t.TaskID, t.Content, t.Created, t.Updated)
	}
	var err error
	for attempt := 0; ; attempt++ {
		err = y.conn.TransactCtx(ctx, func(ctx context.Context, sess sqlx.Session) error {
			if _, e := sess.ExecCtx(ctx, upsertSQL(len(rows)), args...); e != nil {
				return e
			}
			if len(texts) > 0 {
				if _, e := sess.ExecCtx(ctx, upsertTextSQL(len(texts)), targs...); e != nil {
					return e
				}
			}
			return nil
		})
		if err == nil || !retryable(err) || attempt >= len(retryBackoff) {
			return err
		}
		d := retryBackoff[attempt] + time.Duration(rand.Int63n(int64(retryBackoff[attempt]/2)+1))
		logx.WithContext(ctx).Infof("taskcenter: upsert 遇到死锁/锁等待超时，%v 后重试（第 %d 次） 判据=TASKCENTER_UPSERT_RETRY: %v", d, attempt+1, err)
		y.sleepFn()(d)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// processRows 一页源行：补预留单 / 提交快照 → 拼行 → 摘要去重 → 分批 upsert。
// covered 记录本轮已处理的计费键，供预留单驱动那一步跳过。
func (y *Syncer) processRows(ctx context.Context, i int, s Source, rows []srcRow, covered map[string]struct{}) error {
	if len(rows) == 0 {
		return nil
	}
	var bkeys, exts []string
	for _, r := range rows {
		if r.BKey.Valid {
			bkeys = append(bkeys, r.BKey.String)
		}
		if r.Ext.Valid {
			exts = append(exts, r.Ext.String)
		}
	}
	var resv map[string]resvRow
	var subs map[string]sql.NullString
	var err error
	if len(bkeys) > 0 {
		if resv, err = y.loadResv(ctx, bkeys); err != nil {
			return fmt.Errorf("读预留单: %w", err)
		}
	}
	if len(exts) > 0 {
		if subs, err = y.loadSubs(ctx, exts); err != nil {
			return fmt.Errorf("读提交快照: %w", err)
		}
	}
	type item struct {
		t  targetRow
		tx *textRow
		d  uint64
	}
	var todo []item
	sent := y.nw.sent[i]
	for _, r := range rows {
		t, tx := s.buildTarget(r, resv, subs)
		d := t.digest(tx)
		if prev, ok := sent[t.TaskID]; ok && prev == d {
			continue
		}
		todo = append(todo, item{t, tx, d})
	}
	for len(todo) > 0 {
		n := min(len(todo), y.nw.batchRows)
		batch := todo[:n]
		todo = todo[n:]
		rs := make([]targetRow, 0, n)
		var ts []textRow
		for _, it := range batch {
			rs = append(rs, it.t)
			if it.tx != nil {
				ts = append(ts, *it.tx)
			}
		}
		if err := y.upsertBatch(ctx, rs, ts); err != nil {
			return fmt.Errorf("写任务中心: %w", err)
		}
		if len(sent) > sentMax {
			sent = map[string]uint64{}
			y.nw.sent[i] = sent
		}
		for _, it := range batch {
			sent[it.t.TaskID] = it.d
		}
	}
	for _, r := range rows {
		if r.BKey.Valid {
			covered[normKey(r.BKey.String)] = struct{}{}
		}
	}
	return nil
}

// ---------- 源表分页 ----------

// scanSource 追平一个源：按 cursor 状态决定起点，分页读 → 处理；每轮最多 pageCap 页，没读完转 catch-up 下轮续读。
func (y *Syncer) scanSource(ctx context.Context, i int, s Source, covered map[string]struct{}) error {
	c := y.nw.cur[i]
	f := scanFilter{limit: y.nw.pageRows}
	switch c.st {
	case stCatchup:
		a := c.after
		f.after = &a
	case stSteady:
		if c.wm != "" {
			f.from = c.wm
			f.overlap = int64(y.overlapDur() / time.Second)
		}
	}
	maxT := c.wm
	for page := 0; ; page++ {
		rows, err := y.fetchSrc(ctx, s, f)
		if err != nil {
			return fmt.Errorf("读源表: %w", err)
		}
		if err := y.processRows(ctx, i, s, rows, covered); err != nil {
			return err
		}
		if n := len(rows); n > 0 {
			if rows[n-1].Updated > maxT {
				maxT = rows[n-1].Updated
			}
			last := pos{t: rows[n-1].Updated, id: rows[n-1].ID}
			if n >= f.limit {
				if page+1 >= y.nw.pageCap {
					// 本轮读不完：记下续读位置，下轮从这里继续（不回看，保证向前推进）
					y.nw.cur[i] = cursor{st: stCatchup, wm: c.wm, after: last}
					logx.WithContext(ctx).Infof("taskcenter: 源 %s/%s 本轮达到 %d 页上限，下轮从 %s#%d 续读", s.Table, s.EntryCode, y.nw.pageCap, last.t, last.id)
					return nil
				}
				f = scanFilter{limit: f.limit, after: &last}
				continue
			}
		}
		y.nw.cur[i] = cursor{st: stSteady, wm: maxT}
		return nil
	}
}

// ---------- 预留单驱动 ----------

type resvBatch struct {
	changes  []resvChange
	next     cursor
	complete bool
}

// fetchResvChanges 读 biz_type=LOCALLIFE 预留单窗口内变过的 (biz_id, update_time)（所有源共用，一轮一次，普通读）。
// 首轮不回溯全表：首轮各源全量扫描时按键现读预留单，已覆盖；这里只从「当前库时间 - overlap」起跟踪。
func (y *Syncer) fetchResvChanges(ctx context.Context) (*resvBatch, error) {
	if !y.nw.rok {
		var now []struct {
			T string `db:"t"`
		}
		if err := y.queryRows(ctx, &now, "SELECT DATE_FORMAT(NOW(6), "+tsLayout+") AS t"); err != nil || len(now) == 0 {
			if err == nil {
				err = errors.New("空结果")
			}
			return nil, fmt.Errorf("读库时间: %w", err)
		}
		y.nw.rcur = cursor{st: stSteady, wm: now[0].T}
		y.nw.rok = true
	}
	c := y.nw.rcur
	b := &resvBatch{}
	var after *pos
	if c.st == stCatchup {
		a := c.after
		after = &a
	}
	maxT := c.wm
	for page := 0; ; page++ {
		var cond string
		var args []any
		switch {
		case after != nil:
			cond = "(update_time > ? OR (update_time = ? AND id > ?))"
			args = []any{after.t, after.t, after.id}
		default:
			cond = "update_time >= DATE_SUB(CAST(? AS DATETIME(6)), INTERVAL ? SECOND)"
			args = []any{c.wm, int64(y.overlapDur() / time.Second)}
		}
		q := "SELECT biz_id, DATE_FORMAT(update_time, " + tsLayout + ") AS t, id FROM afv_billing_reservation " +
			"WHERE biz_type = 'LOCALLIFE' AND deleted = 0 AND " + cond + " ORDER BY update_time, id LIMIT ?"
		args = append(args, resvPageRows)
		var rs []resvChange
		if err := y.queryRows(ctx, &rs, q, args...); err != nil {
			return nil, fmt.Errorf("读预留单窗口: %w", err)
		}
		b.changes = append(b.changes, rs...)
		if n := len(rs); n > 0 {
			last := pos{t: rs[n-1].Updated, id: rs[n-1].ID}
			if last.t > maxT {
				maxT = last.t
			}
			if n >= resvPageRows {
				if page+1 >= resvPageCap {
					b.next = cursor{st: stCatchup, wm: c.wm, after: last}
					return b, nil
				}
				after = &last
				continue
			}
		}
		b.complete = true
		b.next = cursor{st: stSteady, wm: maxT}
		return b, nil
	}
}

// resvDriven 预留单变了、源行没动的行（如过期回收退款）：按计费键反查源行重新同步。
// 已在本轮源行扫描里处理过的键、以及已处理过的 (源, biz_id, 时间) 都跳过；没有对应源行的（别的服务的预留单）也记下，免得每轮白查。
func (y *Syncer) resvDriven(ctx context.Context, i int, s Source, rb *resvBatch, covered map[string]struct{}) error {
	if rb == nil || strings.TrimSpace(s.BillingRequest) == "" {
		return nil
	}
	if len(y.nw.seenResv) > seenMax {
		y.nw.seenResv = map[string]struct{}{}
	}
	var keys, mks []string
	flush := func() error {
		if len(keys) == 0 {
			return nil
		}
		args := make([]any, 0, len(keys))
		for _, k := range keys {
			args = append(args, k)
		}
		rows, err := y.fetchSrc(ctx, s, scanFilter{bkeys: len(keys), limit: 10 * keyChunk}, args...)
		if err != nil {
			return fmt.Errorf("按预留单读源表: %w", err)
		}
		if err := y.processRows(ctx, i, s, rows, covered); err != nil {
			return err
		}
		for _, m := range mks {
			y.nw.seenResv[m] = struct{}{}
		}
		keys, mks = nil, nil
		return nil
	}
	for _, ch := range rb.changes {
		mk := fmt.Sprintf("%d|%s|%s", i, normKey(ch.BizID), ch.Updated)
		if _, ok := y.nw.seenResv[mk]; ok {
			continue
		}
		if _, ok := covered[normKey(ch.BizID)]; ok {
			y.nw.seenResv[mk] = struct{}{}
			continue
		}
		keys = append(keys, ch.BizID)
		mks = append(mks, mk)
		if len(keys) >= keyChunk {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// syncNewRound 新路径的一轮（y.mu 已持有）。
func (y *Syncer) syncNewRound(ctx context.Context) (errs []error, jobs []traceJob) {
	var rb *resvBatch
	for _, s := range y.sources {
		if strings.TrimSpace(s.BillingRequest) != "" {
			b, err := y.fetchResvChanges(ctx)
			if err != nil {
				logx.WithContext(ctx).Errorf("taskcenter sync: %v", err)
				errs = append(errs, err)
			} else {
				rb = b
			}
			break
		}
	}
	allOK := rb != nil || len(errs) == 0
	window := int64(y.window / time.Second)
	for i, s := range y.sources {
		secs := window
		if !y.synced[i] {
			secs = 0
		}
		covered := map[string]struct{}{}
		err := y.scanSource(ctx, i, s, covered)
		if err == nil {
			err = y.resvDriven(ctx, i, s, rb, covered)
		}
		if err != nil {
			err = fmt.Errorf("%s/%s: %w", s.Table, s.EntryCode, err)
			logx.WithContext(ctx).Errorf("taskcenter sync: %v", err)
			errs = append(errs, err)
			allOK = false
			continue
		}
		if strings.TrimSpace(s.Trace) != "" {
			jobs = append(jobs, traceJob{idx: i, src: s, secs: secs})
		}
		y.synced[i] = true
	}
	if rb != nil && allOK {
		y.nw.rcur = rb.next
	}
	return errs, jobs
}
