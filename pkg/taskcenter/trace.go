package taskcenter

// 任务轨迹（afv_task_event）的派生事件：taskcenter 是所有「源表 → 任务中心」同步的共用收口，
// 对没有自己埋点的服务（图片 / 视频 / 笔记 / 文案 / 数字人，它们走执行面，不直接调网关），
// 在主表 upsert 之后按源表行状态把生命周期事件交给异步写入器（pkg/taskevent），管理端按创作任务号就能看到
// 创建 → 提交 → 完成 / 失败 → 扣费 / 退款。画布与 studio 有自己更细的埋点，不开这个（Source.Trace 为空）。
//
// 读写约束（追踪永远不能拖慢业务、不能锁资金表）：
//   - 只用普通 SELECT（一致性非锁定读），绝不 INSERT … SELECT：不对源表和 afv_billing_reservation 加任何锁；
//   - 只读与本轮同步相同的窗口行（update 在窗口内），并受创建时间下限（TraceFloor，默认 3 天）与每轮行数上限（TraceRowCap）约束；
//     首轮不做全量回填，只覆盖下限窗口；
//   - 事件在 Go 里拼装（脱敏也只有 taskevent.Sanitize 一处），经异步队列写库，不等库；
//   - 派生在 y.mu 之外、单独的 goroutine + 自己的超时里跑，不会拖慢 Sync。
//
// 契约见 pkg/taskevent：task_id = trace_id = Ref(prefix, id)，source='go'，dedupe_key 唯一，INSERT IGNORE。
// 事件由源表当前状态推出，时间取源表时间列（创建 = create 列；提交 / 终态 = update 列；扣费 / 退款 = 预留单 update_time，
// 是首次看到该状态那一轮的值，故为近似，attrs 标 approx_ts）。dedupe 带上状态，后续的状态变化（failed → 重试 → ok）不会被吞：
//
//	go.<Trace>/created     mark  任务创建                      mark:…/created:0
//	go.<Trace>/run         span  创建 → 终态                    span_start:…/run:0、span_end:…/run:<ok|failed|cancelled>
//	go.<Trace>/submitted   mark  已提交执行面                    mark:…/submitted:0
//	go.<Trace>/settle      mark  预留单已确认扣费                mark:…/settle:<预留状态>
//	go.<Trace>/refund      mark  预留单已退回                    mark:…/refund:<REFUNDED|CANCELLED|EXPIRED>

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/pkg/taskevent"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// DefaultTraceFloor 派生事件只看创建时间在这个范围内的行。
	DefaultTraceFloor = 72 * time.Hour
	// DefaultTraceRowCap 每源每轮最多处理的源表行数（预留单查询为其 4 倍）。
	DefaultTraceRowCap = 500
	// traceTimeout 一轮派生（所有源）的总超时。
	traceTimeout = 20 * time.Second
	// seenResvMax 已处理预留单记忆的上限，超过就清空（INSERT IGNORE 兜底，不影响正确性）。
	seenResvMax = 20000

	tsFmt = "'%Y-%m-%d %H:%i:%s.%f'"
)

// cst 与 Java / taskevent 一致的库时区（datetime 列不带时区）。
var cst = time.FixedZone("CST", 8*3600)

// rowReader 一致性读（sqlx.SqlConn 满足）。
type rowReader interface {
	QueryRowsPartialCtx(ctx context.Context, v any, query string, args ...any) error
}

// traceRow 源表的一行（派生事件要用的列）。
type traceRow struct {
	ID      int64          `db:"id"`
	Created sql.NullString `db:"created"`
	Updated sql.NullString `db:"updated"`
	Status  sql.NullInt64  `db:"st"`
	Ext     sql.NullString `db:"ext"`
	Fail    sql.NullString `db:"fail"`
	BKey    sql.NullString `db:"bkey"`
}

// traceResv 预留单（只读状态）。
type traceResv struct {
	BizID   string         `db:"biz_id"`
	Status  sql.NullString `db:"status"`
	Points  sql.NullString `db:"points"`
	Updated sql.NullString `db:"updated"`
}

// WithTrace 调整派生事件的创建时间下限与每轮行数上限（≤0 保持默认）。
func (y *Syncer) WithTrace(floor time.Duration, rowCap int) *Syncer {
	if y == nil {
		return y
	}
	if floor > 0 {
		y.traceFloor = floor
	}
	if rowCap > 0 {
		y.traceRowCap = rowCap
	}
	return y
}

// WithEvents 指定异步事件写入器（默认用同库连接新建一个）。
func (y *Syncer) WithEvents(w *taskevent.Writer) *Syncer {
	if y != nil {
		y.events = w
	}
	return y
}

// WaitEvents 等进行中的派生轮结束（测试 / 关停用）。
func (y *Syncer) WaitEvents() {
	if y != nil {
		y.evWG.Wait()
	}
}

// filters 源的静态筛选（kind / include），不含窗口。
func (s Source) filters() []string {
	var out []string
	if s.Kind != "" {
		out = append(out, "kind = '"+s.Kind+"'")
	}
	if s.Include != "" {
		out = append(out, "("+s.Include+")")
	}
	return out
}

// traceRowsSQL 读源表行：创建时间 ≥ 下限；windowed 时还要 update 在窗口内；keys>0 时改按预留单 biz_id 取（IN）。
// 参数顺序：floorSecs, [windowSecs | keys...], limit。
func (s Source) traceRowsSQL(windowed bool, keys int) string {
	bkey := "NULL"
	if strings.TrimSpace(s.BillingRequest) != "" {
		bkey = s.billingKeySQL()
	}
	ext := "NULL"
	if strings.TrimSpace(s.ExecTaskID) != "" {
		ext = "LEFT((" + s.ExecTaskID + "), 128)"
	}
	conds := append([]string{s.createCol() + " >= NOW() - INTERVAL ? SECOND"}, s.filters()...)
	switch {
	case keys > 0:
		conds = append(conds, "("+bkey+") IN ("+strings.TrimSuffix(strings.Repeat("?,", keys), ",")+")")
	case windowed:
		conds = append(conds, s.updateCol()+" >= NOW() - INTERVAL ? SECOND")
	}
	return "SELECT s.id AS id, DATE_FORMAT(" + s.createCol() + ", " + tsFmt + ") AS created, DATE_FORMAT(" + s.updateCol() + ", " + tsFmt + ") AS updated, " +
		"(" + s.Status + ") AS st, " + ext + " AS ext, " + orNull(s.Fail) + " AS fail, " + bkey + " AS bkey " +
		"FROM " + s.Table + " s WHERE " + strings.Join(conds, " AND ") + " ORDER BY " + s.updateCol() + " DESC LIMIT ?"
}

const traceResvSQL = "SELECT r.biz_id AS biz_id, r.status AS status, CAST(r.price_points AS CHAR) AS points, " +
	"DATE_FORMAT(r.update_time, " + tsFmt + ") AS updated FROM afv_billing_reservation r " +
	"WHERE r.biz_type = 'LOCALLIFE' AND r.deleted = 0 AND r.update_time >= NOW() - INTERVAL ? SECOND " +
	"ORDER BY r.update_time DESC LIMIT ?"

func parseTS(ns sql.NullString) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05.000000", ns.String, cst)
	if err != nil {
		return time.Time{}
	}
	return t
}

type traceJob struct {
	idx  int
	src  Source
	secs int64
}

// deriveAsync 在 Sync 持锁区之外异步派生一轮事件：同一时刻只跑一轮（后到的直接跳过，下一轮的窗口会补上，写入幂等）。
func (y *Syncer) deriveAsync(ctx context.Context, jobs []traceJob) {
	if y.events == nil || len(jobs) == 0 {
		return
	}
	y.evWG.Add(1)
	go func() {
		defer y.evWG.Done()
		defer func() {
			if r := recover(); r != nil {
				logx.Errorf("taskcenter: 派生任务轨迹 panic（不影响同步） 判据=TASK_EVENT_WRITE: %v", r)
			}
		}()
		if !y.evMu.TryLock() {
			return
		}
		defer y.evMu.Unlock()
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), traceTimeout)
		defer cancel()
		y.derive(dctx, jobs)
	}()
}

// derive 一轮派生：每个源一次窗口行查询 + 共享的一次预留单查询（只有带计费的源才查），拼事件入异步队列。
func (y *Syncer) derive(ctx context.Context, jobs []traceJob) {
	rd, ok := any(y.conn).(rowReader)
	if !ok {
		return
	}
	floor := int64(y.traceFloor / time.Second)
	var resvByKey map[string]traceResv
	loadResv := func(secs int64) map[string]traceResv {
		if resvByKey != nil {
			return resvByKey
		}
		resvByKey = map[string]traceResv{}
		var rs []traceResv
		if err := rd.QueryRowsPartialCtx(ctx, &rs, traceResvSQL, secs, y.traceRowCap*4); err != nil {
			logx.WithContext(ctx).Errorf("taskcenter: 读预留单失败（不影响同步） 判据=TASK_EVENT_WRITE: %v", err)
			return resvByKey
		}
		for _, r := range rs {
			if _, dup := resvByKey[r.BizID]; !dup { // 按 update_time 倒序，先到的最新
				resvByKey[r.BizID] = r
			}
		}
		return resvByKey
	}
	for _, j := range jobs {
		s := j.src
		secs := j.secs
		windowed := secs > 0
		if !windowed || secs > floor {
			secs = floor // 首轮（全量）只覆盖下限窗口
		}
		var rows []traceRow
		var args []any
		args = append(args, floor)
		if windowed {
			args = append(args, secs)
		}
		args = append(args, y.traceRowCap)
		if err := rd.QueryRowsPartialCtx(ctx, &rows, s.traceRowsSQL(windowed, 0), args...); err != nil {
			logx.WithContext(ctx).Errorf("taskcenter: 读源表行失败（不影响同步） 判据=TASK_EVENT_WRITE source=%s/%s: %v", s.Table, s.EntryCode, err)
			continue
		}
		if len(rows) >= y.traceRowCap {
			logx.WithContext(ctx).Infof("taskcenter: 派生事件达到每轮行数上限 %d，较旧的行留给下一轮 source=%s/%s", y.traceRowCap, s.Table, s.EntryCode)
		}
		var resv map[string]traceResv
		if strings.TrimSpace(s.BillingRequest) != "" {
			resv = loadResv(secs)
			rows = append(rows, y.resvDrivenRows(ctx, rd, j, rows, resv, floor)...)
		}
		for _, r := range rows {
			y.emitRow(ctx, s, r, resv)
		}
	}
}

// resvDrivenRows 预留单在窗口内变过、但源行本身没动的那些行（如到期回收退款）：按预留单 biz_id 用 IN 取回源行。
// 已处理过的（biz_id, 状态, 时间）不再重复查，没有对应源行的（别的服务的预留单）也记下，避免每轮白查。
func (y *Syncer) resvDrivenRows(ctx context.Context, rd rowReader, j traceJob, have []traceRow, resv map[string]traceResv, floor int64) []traceRow {
	covered := map[string]bool{}
	for _, r := range have {
		if r.BKey.Valid {
			covered[r.BKey.String] = true
		}
	}
	if y.seenResv == nil || len(y.seenResv) > seenResvMax {
		y.seenResv = map[string]struct{}{}
	}
	var keys []string
	var marks []string
	for biz, r := range resv {
		if covered[biz] {
			continue
		}
		mk := strconv.Itoa(j.idx) + "|" + biz + "|" + r.Status.String + "|" + r.Updated.String
		if _, seen := y.seenResv[mk]; seen {
			continue
		}
		keys = append(keys, biz)
		marks = append(marks, mk)
		if len(keys) >= y.traceRowCap {
			break
		}
	}
	if len(keys) == 0 {
		return nil
	}
	args := []any{floor}
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, y.traceRowCap)
	var extra []traceRow
	if err := rd.QueryRowsPartialCtx(ctx, &extra, j.src.traceRowsSQL(false, len(keys)), args...); err != nil {
		logx.WithContext(ctx).Errorf("taskcenter: 按预留单读源表行失败（不影响同步） 判据=TASK_EVENT_WRITE source=%s/%s: %v", j.src.Table, j.src.EntryCode, err)
		return nil
	}
	for _, m := range marks {
		y.seenResv[m] = struct{}{}
	}
	return extra
}

// emitRow 一行源表记录 → 事件（入异步队列）。
func (y *Syncer) emitRow(ctx context.Context, s Source, r traceRow, resv map[string]traceResv) {
	c := strings.TrimSpace(s.Trace)
	ref := Ref(s.Prefix, r.ID)
	step := func(name string) string { return "go." + c + "/" + name }
	base := func(extra taskevent.Attrs) taskevent.Attrs {
		a := taskevent.Attrs{"entry_code": s.EntryCode, "type": s.Type, "origin": c, "approx_ts": true}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	created, updated := parseTS(r.Created), parseTS(r.Updated)
	w := y.events
	w.Write(ctx, taskevent.Event{TaskID: ref, TS: created, StepPath: step("created"), Kind: taskevent.KindMark, Message: "任务创建",
		Dedupe: taskevent.DedupeKey(taskevent.KindMark, step("created"), 0), Attrs: base(nil)})
	w.Write(ctx, taskevent.Event{TaskID: ref, TS: created, StepPath: step("run"), Kind: taskevent.KindSpanStart,
		Dedupe: taskevent.DedupeKey(taskevent.KindSpanStart, step("run"), 0), Attrs: base(nil)})
	if r.Ext.Valid && strings.TrimSpace(r.Ext.String) != "" {
		w.Write(ctx, taskevent.Event{TaskID: ref, TS: updated, StepPath: step("submitted"), Kind: taskevent.KindMark, Message: "已提交执行面",
			ExternalType: "exec_task", ExternalRef: r.Ext.String,
			Dedupe: taskevent.DedupeKey(taskevent.KindMark, step("submitted"), 0), Attrs: base(nil)})
	}
	// 任务中心状态：1 处理中 2 完成 3 失败 4 取消 5 部分成功；只有终态才收尾 run 区间
	if st := r.Status.Int64; r.Status.Valid && (st == 2 || st == 3 || st == 4 || st == 5) {
		status, f, extra := taskevent.StatusOK, taskevent.Fail{}, taskevent.Attrs(nil)
		switch st {
		case 3:
			status = taskevent.StatusFailed
			f = taskevent.Fail{Class: "EXEC", Code: "TASK_FAILED", Check: "taskcenter.derived_failure", Message: r.Fail.String}
		case 4:
			status = taskevent.StatusCancelled
		case 5:
			extra = taskevent.Attrs{"partial": true}
		}
		w.Write(ctx, taskevent.Event{TaskID: ref, TS: updated, StepPath: step("run"), Kind: taskevent.KindSpanEnd, Status: status,
			FailureClass: f.Class, FailureCode: f.Code, CheckCode: f.Check, Message: f.Message,
			Dedupe: "span_end:" + step("run") + ":" + status, Attrs: base(extra)})
	}
	if !r.BKey.Valid || resv == nil {
		return
	}
	rv, ok := resv[r.BKey.String]
	if !ok {
		return
	}
	var name, msg, status string
	switch rv.Status.String {
	case "CONFIRMED", "PARTIALLY_REFUNDED":
		name, msg, status = "settle", "预留单已确认扣费", "confirm"
	case "REFUNDED", "CANCELLED", "EXPIRED":
		name, msg, status = "refund", "预留单已退回", "refund"
	default:
		return
	}
	a := base(nil)
	if p, err := strconv.ParseFloat(strings.TrimSpace(rv.Points.String), 64); err == nil {
		a["points"] = p
	}
	w.Write(ctx, taskevent.Event{TaskID: ref, TS: parseTS(rv.Updated), StepPath: step(name), Kind: taskevent.KindMark, Status: status, Message: msg,
		Dedupe: "mark:" + step(name) + ":" + rv.Status.String, Attrs: a})
}
