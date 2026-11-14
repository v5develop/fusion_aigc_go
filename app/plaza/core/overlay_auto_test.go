package core

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// 冻结进意图的文字位快照：与 ovSlotsJSON 里 shop 位同一版式。
const ovSnapJSON = `[{"key":"shop","layout":{"box":{"x":0.1,"y":0.8,"w":0.8,"h":0.1},"size":0.05,"color":"#FFFFFF","fontRid":"` + goodRid + `"},"maxLength":10}]`

const ovReqID = "req-0123456789abcdef"

var ovIntentColNames = []string{"id", "user_id", "request_id", "run_id", "template_id", "enabled", "values_json", "slots_json", "create_time"}

func autoOverlayService(t *testing.T, textOverlay int8, m *fakeOverlayMedia, a *fakeOverlayAssets) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	s, mock := overlayService(t, textOverlay, m, a)
	s.cfg.Overlay.Auto.Enabled = true
	return s, mock
}

func autoJob(attempts int) *overlayJobRow {
	return &overlayJobRow{ID: 3, UserID: 9, IntentID: 5, TemplateID: 101, SourceURL: "https://cdn/p.png", SourceHash: sourceHash("https://cdn/p.png"),
		Attempts: attempts, ValuesJSON: []byte(`{"shop":"某某足浴"}`), SlotsJSON: []byte(ovSnapJSON)}
}

func ovRow(status, synced int, url any) *sqlmock.Rows {
	var reason any
	if status != overlayStatusOK {
		reason = "x"
	}
	return sqlmock.NewRows(ovCols).AddRow(7, 9, 101, "https://cdn/p.png", "h", `{"shop":"某某足浴"}`, ovPlainRid, nil, url, status, synced, reason, t0)
}

func TestOverlayAutoBackoffAndScanInterval(t *testing.T) {
	for attempt, want := range map[int]time.Duration{0: 30 * time.Second, 1: 30 * time.Second, 2: 2 * time.Minute, 3: 10 * time.Minute, 4: 30 * time.Minute, 9: 30 * time.Minute} {
		if got := overlayBackoffAfter(attempt); got != want {
			t.Errorf("第 %d 次失败后应等 %v，实际 %v", attempt, want, got)
		}
	}
	for age, want := range map[time.Duration]time.Duration{time.Minute: 15 * time.Second, time.Hour: time.Minute, 5 * time.Hour: 5 * time.Minute} {
		if got := overlayScanInterval(age); got != want {
			t.Errorf("登记 %v 后扫描间隔应为 %v，实际 %v", age, want, got)
		}
	}
	var c OverlayAutoConfig
	if c.maxAttempts() != 5 || c.batch() != 10 || c.matchWindow() != 24*time.Hour || len(c.entryCodes()) != 1 || c.entryCodes()[0] != "agentImage" {
		t.Fatalf("零值配置应退回默认: %d %d %v %v", c.maxAttempts(), c.batch(), c.matchWindow(), c.entryCodes())
	}
}

func TestPickOverlaySource(t *testing.T) {
	cases := []struct {
		name, result, wm, want string
		ok                     bool
	}{
		{"免费档用水印图", "https://cdn/r.png", "https://cdn/w.png", "https://cdn/w.png", true},
		{"付费档用原图", "https://cdn/r.png", "", "https://cdn/r.png", true},
		{"用户看到的是私有引用：跳过，不拿原图顶替", "https://cdn/r.png", "mr:abc", "", false},
		{"都没有", "", "", "", false},
	}
	for _, c := range cases {
		got, ok := pickOverlaySource(c.result, c.wm)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v", c.name, got, ok)
		}
	}
}

func TestOverlaySlotSnapshotRoundTrip(t *testing.T) {
	s, _ := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	slots, err := s.overlaySlots(ctx0, 101)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snapSlots(slots))
	back, err := slotsFromSnap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].key != "shop" || back[0].maxLen != 10 || back[0].layout == nil {
		t.Fatalf("快照往返不一致: %+v", back)
	}
	for _, bad := range []string{`not json`, `[]`, `[{"key":"x","layout":{"bad":1}}]`} {
		if _, err := slotsFromSnap([]byte(bad)); err == nil {
			t.Errorf("坏快照 %s 应报错", bad)
		}
	}
}

func TestOverlayIntentRejects(t *testing.T) {
	s, _ := overlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil) // Auto.Enabled 零值 = 关
	if _, err := s.CreateOverlayIntent(ctx0, 9, OverlayIntentInput{RequestID: ovReqID, TemplateID: 101}); xerr.ReasonOf(err) != ReasonOverlayAutoOff {
		t.Fatalf("自动叠字没开应回 %s: %v", ReasonOverlayAutoOff, err)
	}
	s, _ = autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	for name, in := range map[string]OverlayIntentInput{
		"缺 requestId":     {TemplateID: 101},
		"requestId 有非法字符": {RequestID: "req 'or 1=1", TemplateID: 101},
		"requestId 太短":    {RequestID: "abc", TemplateID: 101},
		"缺模板":             {RequestID: ovReqID},
	} {
		if _, err := s.CreateOverlayIntent(ctx0, 9, in); err == nil {
			t.Errorf("%s: 应被拒", name)
		}
	}
	s.ovMedia = nil
	if _, err := s.CreateOverlayIntent(ctx0, 9, OverlayIntentInput{RequestID: ovReqID, TemplateID: 101}); xerr.ReasonOf(err) != ReasonOverlayNotReady {
		t.Fatalf("媒体服务没配应回 %s: %v", ReasonOverlayNotReady, err)
	}
}

func TestOverlayIntentRejectsTemplateWithoutOverlayOrValues(t *testing.T) {
	s, mock := autoOverlayService(t, 0, &fakeOverlayMedia{height: 2000}, nil)
	mock.ExpectQuery(`FROM afv_plaza_overlay_intent WHERE user_id = 9 AND request_id`).WillReturnRows(sqlmock.NewRows(ovIntentColNames))
	if _, err := s.CreateOverlayIntent(ctx0, 9, OverlayIntentInput{RequestID: ovReqID, TemplateID: 101, Values: map[string]string{"shop": "x"}}); err == nil {
		t.Fatal("模板没开 text_overlay 应拒绝登记")
	}
	s, mock = autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	mock.ExpectQuery(`FROM afv_plaza_overlay_intent WHERE user_id = 9 AND request_id`).WillReturnRows(sqlmock.NewRows(ovIntentColNames))
	if _, err := s.CreateOverlayIntent(ctx0, 9, OverlayIntentInput{RequestID: ovReqID, TemplateID: 101, Values: map[string]string{"shop": "  "}}); err == nil {
		t.Fatal("开着加字却没有任何文字应拒绝")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayIntentFreezesValuesAndSlots(t *testing.T) {
	s, mock := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	mock.ExpectQuery(`FROM afv_plaza_overlay_intent WHERE user_id = 9 AND request_id`).WillReturnRows(sqlmock.NewRows(ovIntentColNames))
	// 冻结：文字值（规整后，没有版式的位丢弃）+ 文字位版式 + runId = run- + requestId
	mock.ExpectExec(`INSERT IGNORE INTO afv_plaza_overlay_intent .*'run-` + ovReqID + `', 101, 1, '\{"shop":"某某足浴"\}', '\[\{"key":"shop","layout":.*"maxLength":10\}\]'`).
		WillReturnResult(sqlmock.NewResult(5, 1))
	mock.ExpectQuery(`FROM afv_plaza_overlay_intent WHERE user_id = 9 AND request_id`).WillReturnRows(sqlmock.NewRows(ovIntentColNames).
		AddRow(5, 9, ovReqID, "run-"+ovReqID, 101, 1, `{"shop":"某某足浴"}`, ovSnapJSON, t0))
	out, err := s.CreateOverlayIntent(ctx0, 9, OverlayIntentInput{RequestID: ovReqID, TemplateID: 101, Values: map[string]string{"shop": " 某某足浴 ", "note": "无版式"}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || out.Values["shop"] != "某某足浴" || len(out.Values) != 1 || out.RequestID != ovReqID {
		t.Fatalf("意图不对: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayIntentReplayReturnsFrozenOne(t *testing.T) {
	s, mock := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	mock.ExpectQuery(`FROM afv_plaza_overlay_intent WHERE user_id = 9 AND request_id`).WillReturnRows(sqlmock.NewRows(ovIntentColNames).
		AddRow(5, 9, ovReqID, "run-"+ovReqID, 101, 1, `{"shop":"旧字"}`, ovSnapJSON, t0))
	off := false
	out, err := s.CreateOverlayIntent(ctx0, 9, OverlayIntentInput{RequestID: ovReqID, TemplateID: 101, Enabled: &off, Values: map[string]string{"shop": "新字"}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || out.Values["shop"] != "旧字" {
		t.Fatalf("同一 requestId 重发应原样返回先冻结的意图: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有 INSERT
		t.Fatal(err)
	}
}

func TestOverlayIntentDisabledStoresNoValues(t *testing.T) {
	s, mock := autoOverlayService(t, 0, &fakeOverlayMedia{height: 2000}, nil) // 关闭时不校验模板与文字
	mock.ExpectQuery(`FROM afv_plaza_overlay_intent WHERE user_id = 9 AND request_id`).WillReturnRows(sqlmock.NewRows(ovIntentColNames))
	mock.ExpectExec(`INSERT IGNORE INTO afv_plaza_overlay_intent .*, 101, 0, NULL, NULL, NOW\(\)`).WillReturnResult(sqlmock.NewResult(6, 1))
	mock.ExpectQuery(`FROM afv_plaza_overlay_intent WHERE user_id = 9 AND request_id`).WillReturnRows(sqlmock.NewRows(ovIntentColNames).
		AddRow(6, 9, ovReqID, "run-"+ovReqID, 101, 0, nil, nil, t0))
	off := false
	out, err := s.CreateOverlayIntent(ctx0, 9, OverlayIntentInput{RequestID: ovReqID, TemplateID: 101, Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if out.Enabled || len(out.Values) != 0 {
		t.Fatalf("本次不加字: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestScanOverlayIntentEnqueuesAssetsOfRun(t *testing.T) {
	s, mock := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, &fakeOverlayAssets{saved: true})
	r := &overlayIntentRow{ID: 5, UserID: 9, RequestID: ovReqID, RunID: "run-" + ovReqID, TemplateID: 101, Enabled: 1, CreateTime: t0}
	mock.ExpectQuery(`SELECT plaza_template_id FROM afv_agent_turn WHERE user_id = 9 AND run_id = 'run-` + ovReqID + `'`).
		WillReturnRows(sqlmock.NewRows([]string{"plaza_template_id"}).AddRow(101))
	mock.ExpectQuery(`FROM afv_creation_task .*\$\.source\.runId'\)\) = 'run-` + ovReqID + `'`).
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "parent_task_id"}).AddRow("AiTask_c1", "AiTask_g1").AddRow("AiTask_g1", nil))
	mock.ExpectQuery(`FROM afv_creation_task .*\$\.retryOf'\)\) IN \('AiTask_c1', 'AiTask_g1'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "parent_task_id"}).AddRow("AiTask_r1", "AiTask_rg"))
	// 素材行按任务 + 分组父任务 + 重试任务找；排除叠字版与视频，只认配置的入口
	mock.ExpectQuery(`FROM afv_asset WHERE user_id = 9 .*source_task_id IN \('AiTask_c1', 'AiTask_g1', 'AiTask_r1', 'AiTask_rg'\) AND entry_code IN \('agentImage'\) AND type <> 'video' .*<> 'overlay'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "result_url", "watermark_url"}).
			AddRow(11, "https://cdn/r1.png", "https://cdn/w1.png"). // 免费档：叠水印图
			AddRow(12, "https://cdn/r2.png", nil).                  // 付费档：叠原图
			AddRow(13, "mr:private", nil))                          // 不可摄入：跳过
	mock.ExpectExec(`INSERT IGNORE INTO afv_plaza_overlay_job .*VALUES \(9, 5, 101, 11, 'https://cdn/w1.png', '` + sourceHash("https://cdn/w1.png") + `', 0, 0\)`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT IGNORE INTO afv_plaza_overlay_job .*VALUES \(9, 5, 101, 12, 'https://cdn/r2.png'`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // 已有作业：忽略
	n, err := s.scanOverlayIntent(ctx0, r)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只新建 1 条作业（另一条已存在），实际 %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestScanOverlayIntentTemplateMismatchDisables(t *testing.T) {
	s, mock := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	r := &overlayIntentRow{ID: 5, UserID: 9, RunID: "run-" + ovReqID, TemplateID: 101, Enabled: 1, CreateTime: t0}
	mock.ExpectQuery(`FROM afv_agent_turn`).WillReturnRows(sqlmock.NewRows([]string{"plaza_template_id"}).AddRow(999))
	mock.ExpectExec(`UPDATE afv_plaza_overlay_intent SET enabled = 0 WHERE id = 5`).WillReturnResult(sqlmock.NewResult(0, 1))
	if n, err := s.scanOverlayIntent(ctx0, r); err != nil || n != 0 {
		t.Fatalf("轮次模板对不上应作废意图、不建作业: %d %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有查任务 / 素材
		t.Fatal(err)
	}
}

func TestScanOverlayIntentNothingYet(t *testing.T) {
	s, mock := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	r := &overlayIntentRow{ID: 5, UserID: 9, RunID: "run-" + ovReqID, TemplateID: 101, Enabled: 1, CreateTime: t0}
	mock.ExpectQuery(`FROM afv_agent_turn`).WillReturnRows(sqlmock.NewRows([]string{"plaza_template_id"})) // 轮次还没受理
	mock.ExpectQuery(`FROM afv_creation_task`).WillReturnRows(sqlmock.NewRows([]string{"task_id", "parent_task_id"}))
	if n, err := s.scanOverlayIntent(ctx0, r); err != nil || n != 0 {
		t.Fatalf("还没有产物: %d %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessOverlayJobRendersFrozenSnapshotWithoutQuota(t *testing.T) {
	m, a := &fakeOverlayMedia{height: 2000}, &fakeOverlayAssets{saved: true}
	// 模板此刻已关掉 text_overlay：自动叠字仍按提交时冻结的版式与文字做
	s, mock := autoOverlayService(t, 0, m, a)
	s.ovLim = nil // 不占用户叠字配额：限频器拿掉也不影响（作业路径根本不调它）
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols))
	expectAutoInsert(mock, "")
	mock.ExpectExec(`UPDATE afv_plaza_overlay SET asset_synced = 1`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE id = 7 AND user_id = 9`).WillReturnRows(ovRow(overlayStatusOK, 1, "https://cdn/x.png"))
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE id = 7 AND user_id = 9`).WillReturnRows(ovRow(overlayStatusOK, 1, "https://cdn/x.png"))
	out := s.processOverlayJob(ctx0, autoJob(1), 5)
	if out.state != overlayJobDone || out.overlayID != 7 || out.err != "" {
		t.Fatalf("应完成: %+v", out)
	}
	if len(m.slots) != 1 || m.slots[0].Text != "某某足浴" || m.slots[0].Size != 100 {
		t.Fatalf("应按冻结快照渲染: %+v", m.slots)
	}
	if a.calls != 1 {
		t.Fatalf("叠字版应登记素材库一次（pkg/asset）: %d", a.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessOverlayJobTransientFailureBacksOffWithoutSavingRow(t *testing.T) {
	m := &fakeOverlayMedia{height: 2000, textErr: errors.New("boom")}
	s, mock := autoOverlayService(t, 1, m, &fakeOverlayAssets{saved: true})
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols))
	out := s.processOverlayJob(ctx0, autoJob(2), 5)
	if out.state != overlayJobPending || out.retryIn != 2*time.Minute || out.err != "叠字渲染失败" {
		t.Fatalf("中间失败应退避重试: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 不落 failed 叠加记录
		t.Fatal(err)
	}
}

func TestProcessOverlayJobFinalFailureSavesFailedRowForManualRetry(t *testing.T) {
	m := &fakeOverlayMedia{height: 2000, ingestErr: errors.New("boom")}
	s, mock := autoOverlayService(t, 1, m, &fakeOverlayAssets{saved: true})
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols))
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols))
	expectAutoInsert(mock, `.*'无字原图读取失败'`)
	out := s.processOverlayJob(ctx0, autoJob(5), 5)
	if out.state != overlayJobFailed || out.overlayID != 7 || out.err != "无字原图读取失败" {
		t.Fatalf("最后一次失败应记最终失败并落 failed 记录: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessOverlayJobPermanentFailureStopsImmediately(t *testing.T) {
	m := &fakeOverlayMedia{height: 2000}
	s, mock := autoOverlayService(t, 1, m, nil)
	j := autoJob(1)
	j.SlotsJSON = []byte(`[]`) // 快照坏了
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols))
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols))
	expectAutoInsert(mock, "")
	out := s.processOverlayJob(ctx0, j, 5)
	if out.state != overlayJobFailed || len(m.calls) != 0 {
		t.Fatalf("永久失败第 1 次就终止、不碰媒体服务: %+v %v", out, m.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessOverlayJobKeepsExistingOK(t *testing.T) {
	m := &fakeOverlayMedia{height: 2000}
	s, mock := autoOverlayService(t, 1, m, &fakeOverlayAssets{saved: true})
	// 前端已叠过 / 用户已改字：成功记录且已登记 → 直接收尾，不重渲染、不覆盖
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(ovRow(overlayStatusOK, 1, "https://cdn/x.png"))
	out := s.processOverlayJob(ctx0, autoJob(1), 5)
	if out.state != overlayJobDone || out.overlayID != 7 || len(m.calls) != 0 {
		t.Fatalf("已有成功记录应直接完成: %+v %v", out, m.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessOverlayJobAssetSyncPendingRetriesThenCompletes(t *testing.T) {
	m, a := &fakeOverlayMedia{height: 2000}, &fakeOverlayAssets{saved: false} // 无字版素材行还没同步
	s, mock := autoOverlayService(t, 1, m, a)
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(ovRow(overlayStatusOK, 0, "https://cdn/x.png"))
	out := s.processOverlayJob(ctx0, autoJob(1), 5)
	if out.state != overlayJobPending || out.overlayID != 7 || out.retryIn != 30*time.Second {
		t.Fatalf("没登记上应退避再补: %+v", out)
	}
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(ovRow(overlayStatusOK, 0, "https://cdn/x.png"))
	out = s.processOverlayJob(ctx0, autoJob(5), 5)
	if out.state != overlayJobDone || out.err == "" {
		t.Fatalf("次数用完叠字已成功按完成收尾（读取时补登记）: %+v", out)
	}
	if len(m.calls) != 0 {
		t.Fatalf("只补登记不重渲染: %v", m.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFinishOverlayJobFencedByLeaseToken(t *testing.T) {
	s, mock := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	mock.ExpectExec(`UPDATE afv_plaza_overlay_job SET state = 0, .*next_at = DATE_ADD\(NOW\(\), INTERVAL 120 SECOND\) WHERE id = 3 AND lease_token = 'tok'`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // 租约已丢：结果作废（不报错）
	s.finishOverlayJob(ctx0, autoJob(2), "tok", overlayJobOutcome{state: overlayJobPending, err: "叠字渲染失败", retryIn: 2 * time.Minute})
	mock.ExpectExec(`UPDATE afv_plaza_overlay_job SET state = 1, overlay_id = COALESCE\(7, overlay_id\), last_error = NULL, .*next_at = NULL WHERE id = 3 AND lease_token = 'tok'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	s.finishOverlayJob(ctx0, autoJob(1), "tok", overlayJobOutcome{state: overlayJobDone, overlayID: 7})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayAutoDisabledIsNoop(t *testing.T) {
	s, mock := overlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil) // Auto.Enabled=false
	if s.OverlayAutoReady() {
		t.Fatal("开关关闭不应就绪")
	}
	if n, err := s.ScanOverlayIntents(ctx0); n != 0 || err != nil {
		t.Fatalf("关闭时扫描应空转: %d %v", n, err)
	}
	if n, err := s.RunOverlayJobs(ctx0); n != 0 || err != nil {
		t.Fatalf("关闭时作业应空转: %d %v", n, err)
	}
	var nilSvc *Service
	if nilSvc.OverlayAutoReady() {
		t.Fatal("nil Service 不应就绪")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// expectAutoInsert 自动叠字写回：事务内 FOR UPDATE 复查（没有记录）→ 普通 INSERT（不带 ON DUPLICATE KEY）→ 提交。
func expectAutoInsert(mock sqlmock.Sqlmock, insertTail string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101 .* FOR UPDATE`).WillReturnRows(sqlmock.NewRows(ovCols))
	// 以 VALUES (...) 的右括号结尾：不带 ON DUPLICATE KEY UPDATE（那种写法以 update_time = CURRENT_TIMESTAMP 结尾）
	mock.ExpectExec(`INSERT INTO afv_plaza_overlay \(.*\) VALUES \(.*` + insertTail + `.*\)$`).WillReturnResult(sqlmock.NewResult(7, 1))
	mock.ExpectCommit()
}

// 中：作业渲染期间用户手动叠了字（成功记录已落库）→ 写回前事务内复查到成功记录，放弃本次渲染结果，不覆盖。
func TestProcessOverlayJobAbandonsWhenManualOverlayLandsDuringRender(t *testing.T) {
	m, a := &fakeOverlayMedia{height: 2000}, &fakeOverlayAssets{saved: true}
	s, mock := autoOverlayService(t, 1, m, a)
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols)) // 作业开始时还没有
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101 .* FOR UPDATE`).WillReturnRows(ovRow(overlayStatusOK, 1, "https://cdn/manual.png"))
	mock.ExpectCommit()
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(ovRow(overlayStatusOK, 1, "https://cdn/manual.png"))
	out := s.processOverlayJob(ctx0, autoJob(1), 5)
	if out.state != overlayJobDone || out.overlayID != 7 || out.err != "" {
		t.Fatalf("应按用户的成功记录收尾: %+v", out)
	}
	if a.calls != 0 {
		t.Fatalf("放弃的渲染结果不得登记素材库: %d", a.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // 没有 INSERT / UPDATE 覆盖
		t.Fatal(err)
	}
}

// 最终失败落失败记录时，用户已手动叠成功：保留成功记录，不用失败覆盖。
func TestSaveAutoFailureKeepsManualOK(t *testing.T) {
	s, mock := autoOverlayService(t, 1, &fakeOverlayMedia{height: 2000}, nil)
	mock.ExpectQuery(`FROM afv_plaza_overlay WHERE user_id = 9 AND template_id = 101`).WillReturnRows(sqlmock.NewRows(ovCols))
	mock.ExpectBegin()
	mock.ExpectQuery(`FOR UPDATE`).WillReturnRows(ovRow(overlayStatusOK, 1, "https://cdn/manual.png"))
	mock.ExpectCommit()
	if id := s.saveAutoFailure(ctx0, autoJob(5), "叠字渲染失败"); id != 7 {
		t.Fatalf("应返回用户的成功记录: %d", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
