package taskcenter

// SyncMode=shadow：旧路径照常写（锁行为与 legacy 完全一致，所以仍是「旧」），新路径只读、只算、不写，
// 把「新路径会写的行」与「旧路径刚写进 afv_creation_task / afv_creation_task_text 的行」逐列比对。
// 没有双写：新路径在此模式下不对任何表执行写语句。
//
// 比对只看窗口内的行（update_time 在同步窗口内，每源每轮最多一页），首轮不做全表比对。
// 瞬态误报的处理：
//   - 目标行 update_time ≠ 新算出的 update_time（旧路径还没把源表最新状态同步过去）→ 跳过，计入 stale；
//   - 预留单刚变、旧路径尚未因 OR EXISTS 追上 → 首次发现的不一致只登记，下一轮仍不一致才报 TASKCENTER_SHADOW_DIFF。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/zeromicro/go-zero/core/logx"
)

// ShadowStats shadow 比对计数（测试 / 排障）。
type ShadowStats struct {
	Compared   int64 // 比对过的任务中心行
	Stale      int64 // 目标行比源行旧，跳过
	Suspected  int64 // 首次发现不一致（待下一轮确认）
	Persistent int64 // 连续两轮不一致，已按 判据=TASKCENTER_SHADOW_DIFF 记日志
}

type shadowState struct {
	compared, stale, suspected, persistent atomic.Int64
	pending                                map[string]map[string]struct{} // 源索引键 → 待确认的 task_id
	onDiff                                 func(taskID string, diffs []string)
}

func newShadowState() *shadowState {
	return &shadowState{pending: map[string]map[string]struct{}{}}
}

// ShadowStats 读取计数。
func (y *Syncer) ShadowStats() ShadowStats {
	if y == nil {
		return ShadowStats{}
	}
	sh := y.nw.shadow
	return ShadowStats{sh.compared.Load(), sh.stale.Load(), sh.suspected.Load(), sh.persistent.Load()}
}

// OnShadowDiff 注册持续不一致的回调（测试用）。
func (y *Syncer) OnShadowDiff(fn func(taskID string, diffs []string)) *Syncer {
	if y != nil {
		y.nw.shadow.onDiff = fn
	}
	return y
}

// gotRow afv_creation_task 里的实际行。
type gotRow struct {
	TaskID    string         `db:"task_id"`
	UserID    int64          `db:"user_id"`
	Type      string         `db:"type"`
	Status    int64          `db:"status"`
	EntryCode string         `db:"entry_code"`
	Request   sql.NullString `db:"request_params"`
	Ext       sql.NullString `db:"execution_task_id"`
	Exec      sql.NullString `db:"execution_params"`
	Billing   sql.NullString `db:"billing_status"`
	Points    sql.NullInt64  `db:"price_points"`
	Feature   sql.NullString `db:"feature_code"`
	ModelID   sql.NullInt64  `db:"model_id"`
	ResNo     sql.NullString `db:"reservation_no"`
	Result    sql.NullString `db:"result_url"`
	Results   sql.NullString `db:"result_urls"`
	Cover     sql.NullString `db:"cover_url"`
	Fail      sql.NullString `db:"error_msg"`
	Deleted   int64          `db:"deleted"`
	Updated   string         `db:"upd_time"`
	Origin    sql.NullString `db:"origin_service"`
	OriginRef sql.NullString `db:"origin_ref"`
	Transfer  int64          `db:"is_transferred"`
}

type gotText struct {
	TaskID  string `db:"task_id"`
	Content string `db:"content"`
}

const gotRowSQL = "SELECT task_id, user_id, type, status, entry_code, CAST(request_params AS CHAR) AS request_params, execution_task_id, " +
	"CAST(execution_params AS CHAR) AS execution_params, billing_status, price_points, feature_code, model_id, reservation_no, " +
	"result_url, CAST(result_urls AS CHAR) AS result_urls, cover_url, error_msg, deleted, " +
	"DATE_FORMAT(update_time, " + tsLayout + ") AS upd_time, origin_service, origin_ref, is_transferred FROM afv_creation_task WHERE task_id IN ("

func nsEq(a, b sql.NullString) bool { return a.Valid == b.Valid && a.String == b.String }

func niEq(a, b sql.NullInt64) bool { return a.Valid == b.Valid && a.Int64 == b.Int64 }

func jsonEq(a, b sql.NullString) bool {
	if a.Valid != b.Valid {
		return false
	}
	if !a.Valid {
		return true
	}
	var x, y any
	if json.Unmarshal([]byte(a.String), &x) != nil || json.Unmarshal([]byte(b.String), &y) != nil {
		return a.String == b.String
	}
	return reflect.DeepEqual(x, y)
}

// diffTarget 期望行 vs 实际行，返回不一致的列描述。stale=true 表示实际行比期望行旧（update_time 不同），不是不一致。
// 规则同 upsertUpdateClause：execution_params 期望为 NULL 时不覆盖（不比）；deleted 取 GREATEST（实际 ≥ 期望）；
// create_time / user_id / type / entry_code / is_transferred 只在插入时写，仍比对（行是我们建的）。
func diffTarget(w targetRow, g gotRow, wt *textRow, gt *gotText) (diffs []string, stale bool) {
	if w.Updated != g.Updated {
		return nil, true
	}
	add := func(col string, want, got any) {
		diffs = append(diffs, fmt.Sprintf("%s: want=%v got=%v", col, want, got))
	}
	if w.UserID != g.UserID {
		add("user_id", w.UserID, g.UserID)
	}
	if w.Type != g.Type {
		add("type", w.Type, g.Type)
	}
	if w.Status.Int64 != g.Status || !w.Status.Valid {
		add("status", w.Status, g.Status)
	}
	if w.EntryCode != g.EntryCode {
		add("entry_code", w.EntryCode, g.EntryCode)
	}
	if !jsonEq(w.Request, g.Request) {
		add("request_params", w.Request.String, g.Request.String)
	}
	if !nsEq(w.Ext, g.Ext) {
		add("execution_task_id", w.Ext, g.Ext)
	}
	if w.ExecParams.Valid && !jsonEq(w.ExecParams, g.Exec) {
		add("execution_params", w.ExecParams.String, g.Exec.String)
	}
	if w.Billing != g.Billing.String {
		add("billing_status", w.Billing, g.Billing.String)
	}
	if !niEq(w.Points, g.Points) {
		add("price_points", w.Points, g.Points)
	}
	if !nsEq(w.Feature, g.Feature) {
		add("feature_code", w.Feature, g.Feature)
	}
	if !niEq(w.ModelID, g.ModelID) {
		add("model_id", w.ModelID, g.ModelID)
	}
	if !nsEq(w.ResNo, g.ResNo) {
		add("reservation_no", w.ResNo, g.ResNo)
	}
	if !nsEq(w.Result, g.Result) {
		add("result_url", w.Result, g.Result)
	}
	if !jsonEq(w.Results, g.Results) {
		add("result_urls", w.Results.String, g.Results.String)
	}
	if !nsEq(w.Cover, g.Cover) {
		add("cover_url", w.Cover, g.Cover)
	}
	if !nsEq(w.Fail, g.Fail) {
		add("error_msg", w.Fail, g.Fail)
	}
	if g.Deleted < w.Deleted {
		add("deleted", w.Deleted, g.Deleted)
	}
	if g.Origin.String != w.OriginService {
		add("origin_service", w.OriginService, g.Origin.String)
	}
	if g.OriginRef.String != w.OriginRef {
		add("origin_ref", w.OriginRef, g.OriginRef.String)
	}
	if g.Transfer != 1 {
		add("is_transferred", 1, g.Transfer)
	}
	switch {
	case wt != nil && gt == nil:
		add("text.content", "(有正文)", "(无行)")
	case wt != nil && gt != nil && wt.Content != gt.Content:
		add("text.content", len(wt.Content), len(gt.Content))
	}
	return diffs, false
}

// shadowCompare 旧路径刚同步完源 i 之后：新路径只读只算，与旧路径写入的结果比对。任何失败只记日志，不影响同步。
func (y *Syncer) shadowCompare(ctx context.Context, i int, s Source, secs int64) {
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("taskcenter: shadow 比对 panic（不影响同步） 判据=TASKCENTER_SHADOW_DIFF: %v", r)
		}
	}()
	win := secs
	if win <= 0 {
		win = int64(y.window / 1e9) // 首轮旧路径是全量，这里只抽样窗口内的行
	}
	rows, err := y.fetchSrc(ctx, s, scanFilter{window: win, limit: y.nw.pageRows})
	if err != nil {
		logx.WithContext(ctx).Errorf("taskcenter: shadow 读源表失败（不影响同步） 判据=TASKCENTER_SHADOW_DIFF source=%s/%s: %v", s.Table, s.EntryCode, err)
		return
	}
	key := fmt.Sprintf("%d", i)
	pend := y.nw.shadow.pending[key]
	// 上一轮待确认的行一并重读（可能已不在窗口里）
	have := map[int64]bool{}
	for _, r := range rows {
		have[r.ID] = true
	}
	if len(pend) > 0 {
		for _, r := range y.pendingRows(ctx, s, pend) {
			if !have[r.ID] {
				rows = append(rows, r)
				have[r.ID] = true
			}
		}
	}
	diffs := y.shadowDiff(ctx, s, rows)
	next := map[string]struct{}{}
	sh := y.nw.shadow
	for tid, d := range diffs {
		if _, was := pend[tid]; was {
			sh.persistent.Add(1)
			logx.WithContext(ctx).Errorf("taskcenter: 新旧同步结果不一致 判据=TASKCENTER_SHADOW_DIFF source=%s/%s task_id=%s 差异=%s",
				s.Table, s.EntryCode, tid, strings.Join(d, "; "))
			if sh.onDiff != nil {
				sh.onDiff(tid, d)
			}
			continue
		}
		sh.suspected.Add(1)
		next[tid] = struct{}{}
	}
	sh.pending[key] = next
}

// pendingRows 按 task_id 反查源行：task_id 是 md5，不可逆；这里用 Ref 对窗口外的待确认行不做回查，
// 而是借助 shadowDiff 对这些行的目标 origin_ref（<prefix>:<id>）取回主键。
func (y *Syncer) pendingRows(ctx context.Context, s Source, pend map[string]struct{}) []srcRow {
	args := make([]any, 0, len(pend))
	for tid := range pend {
		args = append(args, tid)
	}
	var refs []struct {
		R sql.NullString `db:"origin_ref"`
	}
	if err := y.queryRows(ctx, &refs, "SELECT origin_ref FROM afv_creation_task WHERE task_id IN ("+marks(len(args))+")", args...); err != nil {
		return nil
	}
	var ids []any
	for _, r := range refs {
		var id int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.R.String, s.Prefix+":"), "%d", &id); err == nil && strings.HasPrefix(r.R.String, s.Prefix+":") {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := y.fetchSrc(ctx, s, scanFilter{ids: len(ids), limit: len(ids)}, ids...)
	if err != nil {
		return nil
	}
	return rows
}

// shadowDiff 对一批源行：算期望行，读实际行，返回 task_id → 不一致列。stale 的行已跳过并计数。
func (y *Syncer) shadowDiff(ctx context.Context, s Source, rows []srcRow) map[string][]string {
	out := map[string][]string{}
	if len(rows) == 0 {
		return out
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
			return out
		}
	}
	if len(exts) > 0 {
		if subs, err = y.loadSubs(ctx, exts); err != nil {
			return out
		}
	}
	want := map[string]targetRow{}
	wantText := map[string]*textRow{}
	var ids []string
	for _, r := range rows {
		t, tx := s.buildTarget(r, resv, subs)
		want[t.TaskID] = t
		wantText[t.TaskID] = tx
		ids = append(ids, t.TaskID)
	}
	got := map[string]gotRow{}
	gotT := map[string]*gotText{}
	for len(ids) > 0 {
		n := min(len(ids), keyChunk)
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, 0, n)
		for _, id := range chunk {
			args = append(args, id)
		}
		var gs []gotRow
		if err := y.queryRows(ctx, &gs, gotRowSQL+marks(n)+")", args...); err != nil {
			logx.WithContext(ctx).Errorf("taskcenter: shadow 读任务中心失败（不影响同步） 判据=TASKCENTER_SHADOW_DIFF: %v", err)
			return out
		}
		for _, g := range gs {
			got[g.TaskID] = g
		}
		var ts []gotText
		if err := y.queryRows(ctx, &ts, "SELECT task_id, content FROM "+TextTable+" WHERE task_id IN ("+marks(n)+")", args...); err == nil {
			for _, g := range ts {
				g := g
				gotT[g.TaskID] = &g
			}
		}
	}
	sh := y.nw.shadow
	for tid, w := range want {
		g, ok := got[tid]
		if !ok {
			sh.compared.Add(1)
			out[tid] = []string{"任务中心里没有这一行（旧路径未写入）"}
			continue
		}
		d, stale := diffTarget(w, g, wantText[tid], gotT[tid])
		if stale {
			sh.stale.Add(1)
			continue
		}
		sh.compared.Add(1)
		if len(d) > 0 {
			out[tid] = d
		}
	}
	return out
}
