package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/signals"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

var now0 = time.Date(2026, 11, 13, 14, 30, 0, 0, algo.CST)

type fakeCatalog struct{ c *store.Catalog }

func (f fakeCatalog) LoadCatalog(context.Context, time.Time) (*store.Catalog, error) { return f.c, nil }

func tpl(id int64, inds ...int64) *store.CatalogTemplate {
	return &store.CatalogTemplate{ID: id, Title: "模板" + string(rune('A'+id)), State: store.StatePublished, MediaType: "image",
		Data:      json.RawMessage(`{"result":["https://x/a.webp"],"prompt":"secret","agentSkillId":1870000000000000001,"description":"足浴海报"}`),
		CreatedAt: now0.AddDate(0, 0, -30), IndustryIDs: inds, Tags: []string{"足浴"}}
}

func testCatalog() *store.Catalog {
	inds := []algo.Industry{
		{ID: 1, Code: "foot_bath", Name: "足浴", Enabled: true},
		{ID: 2, Code: "apparel", Name: "服装", Enabled: true},
		{ID: 3, Code: "foot_bath_bluev", Name: "蓝V号", ParentID: 1, Enabled: true},
	}
	t4 := tpl(4, 1)
	t4.State = "OFFLINE"
	tpls := map[int64]*store.CatalogTemplate{1: tpl(1, 1), 2: tpl(2, 2), 3: tpl(3, 1), 4: t4, 5: tpl(5, 3), 6: tpl(6, 1)}
	return store.NewCatalog(inds, tpls, now0, true)
}

func TestResolveScope(t *testing.T) {
	cat := testCatalog()
	cases := []struct {
		ind, sub, want string
		code           int
	}{
		{"", "", "", xerr.ParamMissing},
		{"", "foot_bath_bluev", "", xerr.ParamMissing},
		{"ALL", "", "", xerr.NotFound},
		{"*", "", "", xerr.NotFound},
		{"ghost", "", "", xerr.NotFound},
		{"apparel", "foot_bath_bluev", "", xerr.NotFound}, // 二级不属于它
		{"foot_bath", "", "foot_bath", 0},
		{"foot_bath", "foot_bath_bluev", "foot_bath_bluev", 0},
	}
	for _, c := range cases {
		got, err := ResolveScope(cat, c.ind, c.sub)
		code, _, _ := xerr.Parse(err)
		if got != c.want || code != c.code {
			t.Fatalf("ResolveScope(%q,%q)=%q code=%d want %q/%d", c.ind, c.sub, got, code, c.want, c.code)
		}
		if c.code == xerr.NotFound && xerr.ReasonOf(err) != ReasonIndustryNotFound {
			t.Fatalf("reason=%s", xerr.ReasonOf(err))
		}
	}
	// 行业表还没建：用户侧没有任何榜
	noInd := store.NewCatalog(nil, map[int64]*store.CatalogTemplate{}, now0, false)
	if _, err := ResolveScope(noInd, "foot_bath", ""); xerr.ReasonOf(err) != ReasonIndustryNotFound {
		t.Fatal("行业未就绪应为 INDUSTRY_NOT_FOUND")
	}
}

func TestCleanPayloadKeepsBigIntsAndHidesKeys(t *testing.T) {
	p, skill := cleanPayload(json.RawMessage(`{"prompt":"p","agentSkillId":1870000000000000001,"v1PlazaTemplateId":3,"result":["u"],"n":12345678901234567890}`))
	if skill == nil || *skill != "1870000000000000001" {
		t.Fatalf("agentSkillId 应原样转字符串: %v", skill)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(p, &m)
	if _, ok := m["prompt"]; ok || m["n"] == nil || string(m["n"]) != "12345678901234567890" {
		t.Fatalf("payload: %s", p)
	}
	if _, ok := m["v1PlazaTemplateId"]; ok {
		t.Fatal("元数据键应摘除")
	}
	if p, s := cleanPayload(nil); string(p) != "{}" || s != nil {
		t.Fatal("空 payload")
	}
}

func TestResolveScopesForWrites(t *testing.T) {
	cat := testCatalog()
	if _, err := resolveScopes(cat, 1, []string{"ALL"}, false); xerr.ReasonOf(err) != ReasonScopeNotEditable {
		t.Fatalf("ALL 应拒绝: %v", err)
	}
	if _, err := resolveScopes(cat, 1, []string{"apparel"}, false); xerr.ReasonOf(err) != ReasonTemplateNotInScope {
		t.Fatalf("不含该模板的行业应拒绝: %v", err)
	}
	got, err := resolveScopes(cat, 5, []string{"foot_bath_bluev", "foot_bath_bluev"}, true)
	if err != nil || !reflect.DeepEqual(got, []string{"foot_bath_bluev", "foot_bath"}) {
		t.Fatalf("alsoParent 应补一级并去重: %v %v", got, err)
	}
	if got, err = resolveScopes(cat, 1, []string{"*"}, false); err != nil || !reflect.DeepEqual(got, []string{"*"}) {
		t.Fatalf("* 原样保留: %v %v", got, err)
	}
	if _, err = resolveScopes(cat, 1, nil, false); err == nil {
		t.Fatal("空范围应拒绝")
	}
}

func TestSuppressWindowAndDates(t *testing.T) {
	kind, until, err := suppressWindow(SuppressReq{Duration: "sunday"}, now0) // 周五
	if err != nil || kind != algo.BlockCancel || !until.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, algo.CST)) {
		t.Fatalf("sunday: %v %v %v", kind, until, err)
	}
	if _, until, _ = suppressWindow(SuppressReq{Duration: "custom", CustomDate: "2026-11-20"}, now0); !until.Equal(time.Date(2026, 11, 21, 0, 0, 0, 0, algo.CST)) {
		t.Fatalf("custom: %v", until)
	}
	if kind, until, _ = suppressWindow(SuppressReq{Duration: "permanent"}, now0); kind != algo.BlockBlock || until != nil {
		t.Fatal("permanent")
	}
	if _, _, err = suppressWindow(SuppressReq{Kind: "CANCEL", UntilAt: "2026-11-13T10:00:00"}, now0); err == nil {
		t.Fatal("过去的 untilAt 应拒绝")
	}
	if kind, until, err = suppressWindow(SuppressReq{Kind: "cancel", UntilAt: "2026-11-14T10:00:00"}, now0); err != nil || kind != algo.BlockCancel || until.Hour() != 10 {
		t.Fatalf("显式 untilAt: %v %v %v", kind, until, err)
	}
	if _, _, err = suppressWindow(SuppressReq{}, now0); err == nil {
		t.Fatal("没给时长应拒绝")
	}
	sd, ed, err := dateWindow("", "", now0)
	if err != nil || fmtDate(sd) != "2026-11-13" || fmtDate(ed) != "2026-11-19" {
		t.Fatalf("缺省今天起 7 天: %v %v %v", sd, ed, err)
	}
	if _, _, err = dateWindow("2026-11-20", "2026-11-19", now0); err == nil {
		t.Fatal("结束早于开始应拒绝")
	}
	rules := []store.HeadlineRule{{ID: 1, PlazaTemplateID: 1, ScopeCode: "foot_bath", Enabled: 1,
		StartDate: store.CSTTime{Time: sd}, EndDate: store.CSTTime{Time: ed}}}
	if !overlaps(rules, 1, "foot_bath", ed, ed.AddDate(0, 0, 3), nil) {
		t.Fatal("首尾相接（含首尾日）算重叠")
	}
	if overlaps(rules, 1, "foot_bath", ed.AddDate(0, 0, 1), ed.AddDate(0, 0, 3), nil) || overlaps(rules, 1, "apparel", sd, ed, nil) ||
		overlaps(rules, 1, "foot_bath", sd, ed, map[int64]bool{1: true}) {
		t.Fatal("不重叠 / 别的范围 / 排除自身")
	}
}

func TestVisibleAt(t *testing.T) {
	t1 := tpl(1)
	if !t1.VisibleAt(now0) {
		t.Fatal("已发布有封面应可见")
	}
	future := now0.Add(time.Hour)
	t1.OnlineAt = &future
	if t1.VisibleAt(now0) {
		t.Fatal("未到上线时间不可见")
	}
	t1.OnlineAt, t1.OfflineAt = nil, &now0
	if t1.VisibleAt(now0) {
		t.Fatal("下线时刻起不可见（左闭右开）")
	}
	t2 := tpl(2)
	t2.Data = json.RawMessage(`{}`)
	if t2.VisibleAt(now0) {
		t.Fatal("无封面不可见")
	}
	t3 := tpl(3)
	t3.State, t3.Enabled = "", true // 生命周期列未上线：按 enabled
	if !t3.VisibleAt(now0) {
		t.Fatal("旧口径 enabled=1 可见")
	}
	// H1：Java 仍是写者（RequireEnabled）时 state=PUBLISHED 也须 enabled=1；Go 为写者时不看 enabled
	t4 := tpl(4)
	t4.Enabled = false
	if !t4.VisibleAt(now0) {
		t.Fatal("Go 为写者：PUBLISHED 不看 enabled")
	}
	t4.RequireEnabled = true
	if t4.VisibleAt(now0) {
		t.Fatal("Java 为写者：enabled=0 不可见")
	}
	t4.Enabled = true
	if !t4.VisibleAt(now0) {
		t.Fatal("Java 为写者：enabled=1 且 PUBLISHED 可见")
	}
	t4.State = "OFFLINE"
	if t4.VisibleAt(now0) {
		t.Fatal("Java 为写者：enabled=1 但非 PUBLISHED 仍不可见")
	}
	var nilT *store.CatalogTemplate
	if nilT.VisibleAt(now0) {
		t.Fatal("nil 不可见")
	}
}

// newSvc sqlmock + miniredis 组装的服务。
func newSvc(t *testing.T) (*Service, sqlmock.Sqlmock, *miniredis.Miniredis) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	mock.MatchExpectationsInOrder(false)
	db := bun.NewDB(sdb, mysqldialect.New())
	mr := miniredis.RunT(t)
	sig := signals.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	s := New(store.New(db), sig, fakeCatalog{testCatalog()}, Conf{}).WithClock(func() time.Time { return now0 })
	return s, mock, mr
}

func expectCtlRulesBlocks(mock sqlmock.Sqlmock, blocks *sqlmock.Rows) {
	mock.ExpectQuery("FROM `afv_plaza_headline_ctl`").WillReturnRows(
		sqlmock.NewRows([]string{"id", "params_json", "params_version", "updated_by", "ingest_last_id", "calibrated_at"}).
			AddRow(1, nil, 0, nil, 0, nil))
	mock.ExpectQuery("FROM `afv_plaza_headline` AS").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_plaza_headline_block`").WillReturnRows(blocks)
}

func TestHeadlinesRequestTimeChecks(t *testing.T) {
	s, mock, mr := newSvc(t)
	ctx := context.Background()
	payload := latestPayload{Version: 9, ComputedAt: now0.Add(-3 * time.Minute), Scope: "foot_bath", Items: []algo.Item{
		{TemplateID: 1, Rank: 1, Source: algo.SourceAuto, Why: "近 7 天复刻 25 次", Uses7d: 25, PrevUses: 10},
		{TemplateID: 2, Rank: 2, Source: algo.SourceAuto, Why: "x", Uses7d: 30}, // 已改到服装：请求时丢弃
		{TemplateID: 3, Rank: 3, Source: algo.SourceAuto, Why: "x", Uses7d: 30}, // 刚被屏蔽
		{TemplateID: 4, Rank: 4, Source: algo.SourceAuto, Why: "x", Uses7d: 30}, // 已下线
		{TemplateID: 5, Rank: 5, Source: algo.SourceAuto, Why: "近 7 天热门", Uses7d: 5, PrevUses: 1},
		{TemplateID: 6, Rank: 6, Source: algo.SourceFill, Why: algo.WhyFill, IsNew: true},
	}}
	b, _ := json.Marshal(payload)
	mr.Set(signals.LatestKey("foot_bath"), string(b))
	expectCtlRulesBlocks(mock, sqlmock.NewRows([]string{"id", "plaza_template_id", "scope_code", "kind", "until_at", "reason", "batch_id", "created_by", "create_time", "update_time"}).
		AddRow(1, 3, "*", "BLOCK", nil, "侵权", nil, nil, now0, now0))
	out, err := s.Headlines(ctx, "foot_bath", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	var ranks []int
	for _, it := range out.Items {
		ids = append(ids, it.PlazaTemplateID)
		ranks = append(ranks, it.Rank)
	}
	if !reflect.DeepEqual(ids, []int64{1, 5, 6}) || !reflect.DeepEqual(ranks, []int{1, 5, 6}) {
		t.Fatalf("请求时校验应丢弃改行业 / 屏蔽 / 下线的项且不重排: %v %v", ids, ranks)
	}
	a, low, fill := out.Items[0], out.Items[1], out.Items[2]
	if a.Kind != "HEAT" || !a.UsageVisible || a.Uses != 25 || a.Trend == nil && a.PreviousUses != 10 {
		t.Fatalf("热度项: %+v", a)
	}
	if low.UsageVisible || low.Uses != 0 || low.PreviousUses != 0 || low.Trend != nil {
		t.Fatalf("不足 20 次不下发数字: %+v", low)
	}
	if fill.Kind != "NEW" || fill.Uses != 0 || !fill.IsNew || fill.Why != algo.WhyFill {
		t.Fatalf("补位项: %+v", fill)
	}
	if a.AgentSkillID == nil || *a.AgentSkillID != "1870000000000000001" || len(a.Industries) != 1 || a.Industries[0].Code != "foot_bath" {
		t.Fatalf("模板字段: %+v", a)
	}
	var pl map[string]any
	_ = json.Unmarshal(a.Payload, &pl)
	if _, ok := pl["prompt"]; ok {
		t.Fatal("payload 不能带 prompt")
	}
	if out.Scope != "foot_bath" || out.Version != 9 || out.StatsTo != "2026-11-13" || out.StatsFrom != "2026-11-07" {
		t.Fatalf("顶层: %+v", out)
	}
	raw, _ := json.Marshal(out)
	for _, bad := range []string{`"source"`, `"score"`, `"scoreDetail"`} {
		if json.Valid(raw) && contains(string(raw), bad) {
			t.Fatalf("用户端不得下发 %s: %s", bad, raw)
		}
	}
}

func TestHeadlinesKeywordAndEmptyScopeTriggersRefresh(t *testing.T) {
	s, mock, mr := newSvc(t)
	ctx := context.Background()
	payload := latestPayload{Version: 3, ComputedAt: now0, Scope: "foot_bath", Items: []algo.Item{
		{TemplateID: 1, Rank: 1, Source: algo.SourceAuto, Why: "近 7 天复刻 25 次", Uses7d: 25},
		{TemplateID: 6, Rank: 2, Source: algo.SourceFill, Why: algo.WhyFill},
	}}
	b, _ := json.Marshal(payload)
	mr.Set(signals.LatestKey("foot_bath"), string(b))
	s.catalog.(fakeCatalog).c.Templates[6].Title = "团购海报"
	expectCtlRulesBlocks(mock, sqlmock.NewRows([]string{"id"}))
	out, err := s.Headlines(ctx, "foot_bath", "", "团购")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].PlazaTemplateID != 6 || out.Items[0].Rank != 2 {
		t.Fatalf("keyword 只在该榜内过滤、名次保持: %+v", out.Items)
	}

	// 新行业还没进最新版本：返回空榜（非错误）并置刷新标记
	mock.ExpectQuery("FROM `afv_plaza_headline_snapshot` AS").WillReturnRows(
		sqlmock.NewRows([]string{"id", "computed_at", "scope_stats"}).AddRow(3, now0, `{"foot_bath":{}}`))
	mock.ExpectQuery("FROM `afv_plaza_headline_snapshot_item`").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_plaza_headline_ctl`").WillReturnRows(
		sqlmock.NewRows([]string{"id", "params_json", "params_version", "updated_by", "ingest_last_id", "calibrated_at"}).AddRow(1, nil, 0, nil, 0, nil))
	out, err = s.Headlines(ctx, "apparel", "", "")
	if err != nil || len(out.Items) != 0 || out.Items == nil {
		t.Fatalf("新范围应返回空榜: %+v %v", out, err)
	}
	if !mr.Exists(signals.DirtyKey) {
		t.Fatal("应置刷新标记，由租约主下一轮刷新")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRefreshNowFallsBackToDBAndWritesSnapshot(t *testing.T) {
	s, mock, mr := newSvc(t)
	ctx := context.Background()
	mock.ExpectQuery("FROM `afv_plaza_headline_ctl`").WillReturnRows(
		sqlmock.NewRows([]string{"id", "params_json", "params_version", "updated_by", "ingest_last_id", "calibrated_at"}).AddRow(1, nil, 0, nil, 0, nil))
	turns := sqlmock.NewRows([]string{"id", "user_id", "plaza_template_id", "create_time"})
	for i := 0; i < 30; i++ { // 模板 1：30 个用户各一次 → 合格
		turns.AddRow(i+1, 100+i, 1, now0.Add(-time.Duration(i)*time.Hour))
	}
	for i := 0; i < 6; i++ { // 同一用户刷模板 5：一天封顶 3 次
		turns.AddRow(100+i, 7, 5, now0.Add(-time.Duration(i)*10*time.Minute))
	}
	mock.ExpectQuery("FROM afv_agent_turn").WillReturnRows(turns)
	mock.ExpectQuery("FROM `afv_plaza_headline` AS").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_plaza_headline_block`").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_plaza_headline_snapshot` AS").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO `afv_plaza_headline_snapshot` .*'2026-11-13 14:30:00.000', 'SCHEDULED'.*'DB'").WillReturnResult(sqlmock.NewResult(42, 1))
	mock.ExpectExec("INSERT INTO `afv_plaza_headline_snapshot_item`").WillReturnResult(sqlmock.NewResult(1, 10))
	mock.ExpectCommit()

	out, err := s.RefreshNow(ctx, TriggerScheduled, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != 42 || !out.Refreshed {
		t.Fatalf("outcome: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	b, err := mr.Get(signals.LatestKey("foot_bath"))
	if err != nil {
		t.Fatal(err)
	}
	var p latestPayload
	_ = json.Unmarshal([]byte(b), &p)
	if p.Version != 42 || len(p.Items) == 0 || p.Items[0].TemplateID != 1 || p.Items[0].Source != algo.SourceAuto || p.Items[0].Score != nil {
		t.Fatalf("足浴榜缓存: %+v", p.Items)
	}
	for _, it := range p.Items {
		if it.TemplateID == 2 {
			t.Fatal("服装模板进入了足浴榜")
		}
	}
	if mr.Exists(signals.LatestKey(algo.ScopeAll)) {
		t.Fatal("ALL 不进用户侧缓存")
	}
	if !mr.Exists(signals.LatestKey("foot_bath_bluev")) || !mr.Exists(signals.LatestKey("apparel")) {
		t.Fatal("每个行业范围都应有缓存（含空榜 / 补位）")
	}
	// 锁已释放
	if mr.Exists(signals.LockKey) {
		t.Fatal("刷新后应释放锁")
	}
}
