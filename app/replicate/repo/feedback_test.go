package repo

import (
	"database/sql/driver"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"

	"github.com/DATA-DOG/go-sqlmock"
)

// 反馈覆盖写：新插入影响 1 行 = 第一次；ON DUPLICATE 改写影响 2 行 = 覆盖；值没变影响 0 行也不算第一次。
func TestUpsertFeedbackCreatedFlag(t *testing.T) {
	for affected, want := range map[int64]bool{1: true, 2: false, 0: false} {
		r, mock := newMock(t)
		mock.ExpectExec(`INSERT INTO .afv_replicate_feedback. .*VALUES \(7, 3, 2, 'down', CAST\('\["seam_jump"\]' AS JSON\), '跳了', 'smooth'\) ON DUPLICATE KEY UPDATE`).
			WillReturnResult(sqlmock.NewResult(0, affected))
		created, err := r.UpsertFeedback(ctx, &model.Feedback{UserID: 7, JobID: 3, SegIndex: 2, Rating: "down", Reasons: []byte(`["seam_jump"]`), Note: "跳了", Continuity: "smooth"})
		if err != nil || created != want {
			t.Fatalf("影响 %d 行：created=%v err=%v", affected, created, err)
		}
	}
	// up 没有原因：写 JSON null
	r, mock := newMock(t)
	mock.ExpectExec(`VALUES \(7, 3, 0, 'up', CAST\('null' AS JSON\), '', 'fast'\)`).WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := r.UpsertFeedback(ctx, &model.Feedback{UserID: 7, JobID: 3, Rating: "up", Continuity: "fast"}); err != nil {
		t.Fatal(err)
	}
}

// ---- 后台汇总 / 列表（CONTRACT §AB）----

// 汇总是单条聚合 SQL：每个原因一组 JSON_CONTAINS 条件聚合，时间窗口按 created_at 左闭右开；结果按列顺序回填。
func TestAdminFeedbackSummaryScan(t *testing.T) {
	r, mock := newMock(t)
	cols := []string{"total", "up", "down", "us", "ds", "uf", "df"}
	vals := []driver.Value{int64(10), int64(6), int64(4), int64(3), int64(3), int64(3), int64(1)}
	for range core.FeedbackReasons {
		cols = append(cols, "n", "s", "f")
		vals = append(vals, int64(2), int64(1), int64(1))
	}
	mock.ExpectQuery("SELECT COUNT\\(\\*\\).*JSON_CONTAINS\\(reasons, '\"face_mismatch\"'\\).*JSON_CONTAINS\\(reasons, '\"other\"'\\).*FROM `afv_replicate_feedback` WHERE created_at >= FROM_UNIXTIME\\(1000 / 1000\\) AND created_at < FROM_UNIXTIME\\(2000 / 1000\\)").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(vals...))
	st, err := r.AdminFeedbackSummary(ctx, time.UnixMilli(1000), time.UnixMilli(2000))
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 10 || st.Up != 6 || st.Down != 4 || st.UpSmooth != 3 || st.DownFast != 1 || len(st.Reasons) != len(core.FeedbackReasons) ||
		st.Reasons[0].Reason != "face_mismatch" || st.Reasons[0].Count != 2 || st.Reasons[0].Smooth != 1 {
		t.Fatalf("%+v", st)
	}
}

// ---- 真库集成（REPLICATE_TEST_DSN，库名须含 scratch / test；表 afv_replicate_feedback 来自 Go_V1.0.60；未设置时跳过）----

const fbITUser1, fbITUser2 = 990091, 990092

func fbITSeed(t *testing.T) *Repo {
	t.Helper()
	r, db := integrationRepo(t)
	clean := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM afv_replicate_feedback WHERE user_id IN (?, ?)", fbITUser1, fbITUser2)
	}
	clean()
	t.Cleanup(clean)
	ins := func(user, job int64, seg int, rating, reasons, note, cont, created string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "INSERT INTO afv_replicate_feedback (user_id, job_id, seg_index, rating, reasons, note, continuity, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, CAST(? AS JSON), ?, ?, ?, ?)", user, job, seg, rating, reasons, note, cont, created, created); err != nil {
			t.Fatal(err)
		}
	}
	ins(fbITUser1, 880001, 0, "up", "null", "", "fast", "2026-11-10 10:00:00")
	ins(fbITUser1, 880002, 0, "down", `["seam_jump","other"]`, "接缝跳", "smooth", "2026-11-11 10:00:00")
	ins(fbITUser1, 880002, 2, "down", `["seam_jump"]`, "", "smooth", "2026-11-12 10:00:00")
	ins(fbITUser2, 880003, 0, "down", `["face_mismatch"]`, "不像", "fast", "2026-11-13 10:00:00")
	ins(fbITUser2, 880004, 0, "up", "null", "", "smooth", "2026-11-14 10:00:00")
	ins(fbITUser2, 880005, 0, "up", "null", "", "fast", "2026-10-01 10:00:00") // 窗口外
	return r
}

func fbITWin() (*time.Time, *time.Time) {
	f := time.Date(2026, 11, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	to := time.Date(2026, 12, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	return &f, &to
}

func TestIntegrationAdminListFeedback(t *testing.T) {
	r := fbITSeed(t)
	from, to := fbITWin()
	list := func(f FeedbackFilter, off, lim int) ([]*model.Feedback, int64) {
		t.Helper()
		rows, n, err := r.AdminListFeedback(ctx, f, off, lim)
		if err != nil {
			t.Fatal(err)
		}
		return rows, n
	}
	// 只数本测试种的用户：用 userId 筛两次对账
	if rows, n := list(FeedbackFilter{UserID: fbITUser1, From: from, To: to}, 0, 10); n != 3 || len(rows) != 3 || rows[0].ID < rows[1].ID {
		t.Fatalf("用户 1：n=%d rows=%d（应 3 行、id 倒序）", n, len(rows))
	}
	if _, n := list(FeedbackFilter{UserID: fbITUser2, From: from, To: to}, 0, 10); n != 2 {
		t.Fatalf("窗口外那条不该算：n=%d", n)
	}
	if rows, n := list(FeedbackFilter{UserID: fbITUser1, Rating: "down", Reason: "seam_jump", From: from, To: to}, 0, 10); n != 2 || len(rows) != 2 {
		t.Fatalf("原因筛选 seam_jump：n=%d", n)
	}
	if _, n := list(FeedbackFilter{UserID: fbITUser1, Reason: "other", From: from, To: to}, 0, 10); n != 1 {
		t.Fatalf("原因筛选 other：n=%d", n)
	}
	if _, n := list(FeedbackFilter{UserID: fbITUser2, Continuity: "smooth", From: from, To: to}, 0, 10); n != 1 {
		t.Fatalf("连贯度筛选：n=%d", n)
	}
	if rows, n := list(FeedbackFilter{JobID: 880002, From: from, To: to}, 0, 10); n != 2 || len(rows) != 2 {
		t.Fatalf("任务筛选：n=%d", n)
	}
	// 分页：total 不受 limit 影响
	if rows, n := list(FeedbackFilter{UserID: fbITUser1, From: from, To: to}, 1, 1); n != 3 || len(rows) != 1 {
		t.Fatalf("分页：n=%d rows=%d", n, len(rows))
	}
	// 窗口右开：to 恰为 11-12 10:00 时不含该行
	end := time.Date(2026, 11, 12, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if _, n := list(FeedbackFilter{UserID: fbITUser1, From: from, To: &end}, 0, 10); n != 2 {
		t.Fatalf("右开区间：n=%d", n)
	}
}

func TestIntegrationListJobFeedbackAndSummary(t *testing.T) {
	r := fbITSeed(t)
	rows, err := r.ListJobFeedback(ctx, 880002)
	if err != nil || len(rows) != 2 || rows[0].SegIndex != 0 || rows[1].SegIndex != 2 {
		t.Fatalf("任务反馈按段号升序：%v %+v", err, rows)
	}
	from, to := fbITWin()
	st, err := r.AdminFeedbackSummary(ctx, *from, *to)
	if err != nil {
		t.Fatal(err)
	}
	// 库里可能有别的数据：至少含本测试种的 5 条；断言用差量不可行，这里要求专用库，直接比对精确值
	if st.Total < 5 || st.Up < 2 || st.Down < 3 {
		t.Fatalf("汇总偏小：%+v", st)
	}
	get := func(code string) ReasonStat {
		for _, x := range st.Reasons {
			if x.Reason == code {
				return x
			}
		}
		t.Fatalf("缺原因 %s", code)
		return ReasonStat{}
	}
	if s := get("seam_jump"); s.Count < 2 || s.Smooth < 2 {
		t.Fatalf("seam_jump：%+v", s)
	}
	if s := get("face_mismatch"); s.Count < 1 || s.Fast < 1 {
		t.Fatalf("face_mismatch：%+v", s)
	}
	if st.Up+st.Down != st.Total || st.UpSmooth+st.UpFast != st.Up || st.DownSmooth+st.DownFast != st.Down {
		t.Fatalf("各口径应自洽：%+v", st)
	}
}
