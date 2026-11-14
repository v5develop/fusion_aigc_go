package algo

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func intp(v int) *int           { return &v }
func floatp(v float64) *float64 { return &v }

func TestParamsValidate(t *testing.T) {
	c := Config{Global: DefaultParams()}
	if errs := c.Validate(nil); len(errs) != 0 {
		t.Fatalf("默认值应合法: %v", errs)
	}
	bad := c
	bad.Global.TopN = 2
	bad.Global.HysteresisDelta = 0.9
	bad.Global.ScoreNormalize = "weird"
	errs := bad.Validate(nil)
	fields := map[string]bool{}
	for _, e := range errs {
		fields[e.Field] = true
	}
	for _, f := range []string{"global.top-n", "global.hysteresis-delta", "global.score-normalize"} {
		if !fields[f] {
			t.Fatalf("缺少字段错误 %s: %v", f, errs)
		}
	}
	// 跨字段：max-visible ≥ top-n
	x := c
	x.Global.MaxVisible = 8
	if errs := x.Validate(nil); len(errs) != 1 || errs[0].Field != "global.max-visible" {
		t.Fatalf("max-visible 跨字段: %v", errs)
	}
	// 覆盖：未知行业、保留范围、覆盖后的 top-n 越过 max-visible / 小于 max-per-media
	y := c
	y.Overrides = map[string]ScopeOverride{
		"ghost":     {TopN: intp(5)},
		ScopeAll:    {TopN: intp(5)},
		"foot_bath": {TopN: intp(20), MinDecayed: floatp(-1)},
		"apparel":   {TopN: intp(5), MinUniqueUsers: intp(1)},
	}
	errs = y.Validate(map[string]bool{"foot_bath": true, "apparel": true})
	got := map[string]bool{}
	for _, e := range errs {
		got[e.Field] = true
	}
	want := []string{"overrides.ghost", "overrides.ALL", "overrides.foot_bath.top-n", "overrides.foot_bath.min-decayed", "overrides.apparel.top-n"}
	for _, f := range want {
		if !got[f] {
			t.Fatalf("缺少 %s: %v", f, errs)
		}
	}
	if !strings.Contains(errs.Error(), "overrides.ghost") {
		t.Fatal("Error() 应列出字段")
	}
}

func TestConfigForAndHash(t *testing.T) {
	c := Config{Global: DefaultParams(), Overrides: map[string]ScopeOverride{"foot_bath": {TopN: intp(5), MinDecayed: floatp(1), MinUniqueUsers: intp(2)}}}
	p := c.For("foot_bath")
	if p.TopN != 5 || p.MinDecayed != 1 || p.MinUniqueUsers != 2 || p.HalfLifeHours != 24 {
		t.Fatalf("覆盖: %+v", p)
	}
	if c.For(ScopeAll).TopN != 10 || c.For("apparel").TopN != 10 {
		t.Fatal("ALL / 未覆盖的范围应用全局值")
	}
	h1 := c.Hash()
	c2 := Config{Global: DefaultParams(), Overrides: map[string]ScopeOverride{"foot_bath": {TopN: intp(5), MinDecayed: floatp(1), MinUniqueUsers: intp(2)}, "empty": {}}}
	if c2.Hash() != h1 {
		t.Fatal("空覆盖不应影响 hash")
	}
	c2.Global.TopN = 11
	if c2.Hash() == h1 || len(h1) != 64 {
		t.Fatal("参数变化 hash 应变化")
	}
}

func TestParseConfigFallsBack(t *testing.T) {
	def := DefaultParams()
	cfg, warn := ParseConfig(nil, def)
	if warn != nil || cfg.Global != def {
		t.Fatal("空参数应全用默认")
	}
	cfg, warn = ParseConfig([]byte(`{"global":{"top-n":12,"weight":{"fresh":0.3}},"overrides":{"foot_bath":{"top-n":8}}}`), def)
	if warn != nil || cfg.Global.TopN != 12 || cfg.Global.Weight.Fresh != 0.3 || cfg.Global.Weight.Decay != 1 || *cfg.Overrides["foot_bath"].TopN != 8 {
		t.Fatalf("部分键应叠加默认: %+v warn=%v", cfg, warn)
	}
	if cfg, warn = ParseConfig([]byte(`{bad`), def); warn == nil || cfg.Global != def {
		t.Fatal("非法 JSON 应沿用默认并告警")
	}
	if cfg, warn = ParseConfig([]byte(`{"global":{"top-n":99}}`), def); warn == nil || cfg.Global != def {
		t.Fatal("越界应沿用默认并告警")
	}
}

func TestFormatCountAndWhy(t *testing.T) {
	cases := map[int]string{0: "0", 320: "320", 999: "999", 1000: "1k", 1234: "1.2k", 1299: "1.2k", 9999: "9.9k", 10000: "1w", 12345: "1.2w", 150000: "15w"}
	for n, want := range cases {
		if got := FormatCount(n); got != want {
			t.Fatalf("FormatCount(%d)=%s want %s", n, got, want)
		}
	}
	p := DefaultParams()
	type tc struct {
		st   Stats
		want string
	}
	for _, c := range []tc{
		{Stats{Uses7d: 320, AgeDays: 3}, "新上架 · 近 7 天 320 次复刻"},
		{Stats{Uses7d: 1234, PrevUses: 10, Velocity: 0.6, AgeDays: 30}, "今日热度上升 · 近 7 天 1.2k 次复刻"},
		{Stats{Uses7d: 1234, PrevUses: 663, AgeDays: 30}, "近 7 天复刻 1.2k 次，较前 7 天 +86%"},
		{Stats{Uses7d: 320, AgeDays: 30}, "近 7 天复刻 320 次，新上榜"},
		{Stats{Uses7d: 320, PrevUses: 300, AgeDays: 30}, "近 7 天复刻 320 次"},
		// 不足展示门槛（20 次）：不带数字
		{Stats{Uses7d: 5, AgeDays: 2}, "新上架 · 近期热门"},
		{Stats{Uses7d: 19, PrevUses: 2, Velocity: 0.9, AgeDays: 30}, "今日热度上升"},
		{Stats{Uses7d: 3, PrevUses: 3, AgeDays: 30}, "近 7 天热门"},
	} {
		got := AutoWhy(c.st, p)
		if got != c.want {
			t.Fatalf("AutoWhy(%+v)=%q want %q", c.st, got, c.want)
		}
		if len([]rune(got)) > 60 {
			t.Fatalf("推荐语超过 60 字: %q", got)
		}
	}
	if UsageVisible(19, p) || !UsageVisible(20, p) {
		t.Fatal("展示门槛应为 20")
	}
}

func TestSuppressUntil(t *testing.T) {
	wed := time.Date(2026, 11, 11, 15, 0, 0, 0, CST) // 周三
	kind, until, err := SuppressUntil(Duration24h, time.Time{}, wed)
	if err != nil || kind != BlockCancel || !until.Equal(wed.Add(24*time.Hour)) {
		t.Fatalf("24h: %v %v %v", kind, until, err)
	}
	_, until, _ = SuppressUntil(DurationSunday, time.Time{}, wed)
	if !until.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, CST)) {
		t.Fatalf("到本周日结束（下周一 0 点）: %v", until)
	}
	sun := time.Date(2026, 11, 15, 23, 0, 0, 0, CST)
	_, until, _ = SuppressUntil(DurationSunday, time.Time{}, sun)
	if !until.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, CST)) {
		t.Fatalf("周日当天应为今晚 24 点: %v", until)
	}
	mon := time.Date(2026, 11, 16, 0, 30, 0, 0, CST)
	_, until, _ = SuppressUntil(DurationSunday, time.Time{}, mon)
	if !until.Equal(time.Date(2026, 11, 23, 0, 0, 0, 0, CST)) {
		t.Fatalf("周一应到本周日: %v", until)
	}
	_, until, err = SuppressUntil(DurationCustom, time.Date(2026, 11, 20, 9, 0, 0, 0, CST), wed)
	if err != nil || !until.Equal(time.Date(2026, 11, 21, 0, 0, 0, 0, CST)) {
		t.Fatalf("自定义日期当日 24:00: %v %v", until, err)
	}
	// 自定义今天：今天 24:00 仍晚于现在，允许
	if _, _, err = SuppressUntil(DurationCustom, wed, wed); err != nil {
		t.Fatalf("自定义今天应允许: %v", err)
	}
	if _, _, err = SuppressUntil(DurationCustom, wed.AddDate(0, 0, -1), wed); err != ErrUntilNotFuture {
		t.Fatalf("过去的日期应拒绝: %v", err)
	}
	kind, until, err = SuppressUntil(DurationPermanent, time.Time{}, wed)
	if err != nil || kind != BlockBlock || until != nil {
		t.Fatalf("永久: %v %v %v", kind, until, err)
	}
	if _, _, err = SuppressUntil("forever", time.Time{}, wed); err != ErrUnknownDuration {
		t.Fatal("未知时长应报错")
	}
}

func TestExpiredSnapshots(t *testing.T) {
	var metas []SnapshotMeta
	v := int64(0)
	// 10 天前起每 10 分钟一版
	for at := now0.AddDate(0, 0, -10); !at.After(now0); at = at.Add(10 * time.Minute) {
		v++
		metas = append(metas, SnapshotMeta{Version: v, ComputedAt: at})
	}
	del := ExpiredSnapshots(metas, now0, 7, 20)
	deleted := map[int64]bool{}
	for _, d := range del {
		deleted[d] = true
	}
	kept := 0
	perHour := map[int64]int{}
	for _, m := range metas {
		if deleted[m.Version] {
			continue
		}
		kept++
		age := now0.Sub(m.ComputedAt)
		if age > 7*24*time.Hour {
			t.Fatalf("超期版本未删: %v", m)
		}
		if age > 24*time.Hour {
			perHour[HourKey(m.ComputedAt)]++
		}
	}
	for h, n := range perHour {
		if n != 1 {
			t.Fatalf("24h 外每小时只留第一版: hour=%d n=%d", h, n)
		}
	}
	// 24 小时内 ≈145 版 + 6 天 × 24 小时
	if kept < 140+6*24-2 || kept > 146+6*24+1 {
		t.Fatalf("保留数量异常: %d", kept)
	}
	// 至少保留最近 20 版（全都超期也不删光）
	old := []SnapshotMeta{}
	for i := int64(1); i <= 25; i++ {
		old = append(old, SnapshotMeta{Version: i, ComputedAt: now0.AddDate(0, 0, -30).Add(time.Duration(i) * time.Minute)})
	}
	if got := ExpiredSnapshots(old, now0, 7, 20); !reflect.DeepEqual(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("至少保留 20 版: %v", got)
	}
}

func TestCountTurnsCapAndRapidDedupe(t *testing.T) {
	base := time.Date(2026, 11, 13, 9, 0, 0, 0, CST)
	var turns []Turn
	// 用户 1 对模板 7 同一天 5 次（间隔 10 分钟）→ 只计 3 次
	for i := 0; i < 5; i++ {
		turns = append(turns, Turn{RowID: int64(i + 1), UserID: 1, TemplateID: 7, CreatedAt: base.Add(time.Duration(i) * 10 * time.Minute)})
	}
	// 用户 2：0s、50s、100s → 50s 与 100s 都距上一次尝试不足 60 秒（被去重的尝试也刷新窗口）→ 只计 1 次
	for i, s := range []int{0, 50, 100} {
		turns = append(turns, Turn{RowID: int64(100 + i), UserID: 2, TemplateID: 7, CreatedAt: base.Add(time.Duration(s) * time.Second)})
	}
	// 用户 2 第二天再来 → 新的一天新的封顶
	turns = append(turns, Turn{RowID: 200, UserID: 2, TemplateID: 7, CreatedAt: base.AddDate(0, 0, 1)})
	// 重复的行 id 只算一次；模板 id 0 忽略
	turns = append(turns, Turn{RowID: 1, UserID: 1, TemplateID: 7, CreatedAt: base}, Turn{RowID: 300, UserID: 3, TemplateID: 0, CreatedAt: base})
	tc := CountTurns(turns, 3, 60)
	total := 0
	for _, c := range tc.Hours[7] {
		total += c
	}
	if total != 3+1+1 {
		t.Fatalf("计入总数=%d，want 5（封顶 3 + 去重 1 + 次日 1）", total)
	}
	if tc.Hours[7][HourKey(base)] != 4 || tc.Hours[7][HourKey(base.AddDate(0, 0, 1))] != 1 {
		t.Fatalf("小时桶: %v", tc.Hours[7])
	}
	sig := tc.Signals(base.AddDate(0, 0, 1))
	if sig[7].UniqueUsers != 2 {
		t.Fatalf("去重用户=%d", sig[7].UniqueUsers)
	}
	// 输入乱序结果相同
	rev := append([]Turn(nil), turns...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if !reflect.DeepEqual(CountTurns(rev, 3, 60), tc) {
		t.Fatal("乱序输入结果应一致")
	}
	// rapid=0 不去重
	if n := CountTurns(turns[5:8], 3, 0).Hours[7][HourKey(base)]; n != 3 {
		t.Fatalf("rapid=0 时应计 3 次, got %d", n)
	}
	// 去重用户只看近 7 个自然日
	if s := tc.Signals(base.AddDate(0, 0, 7)); s[7].UniqueUsers != 1 {
		t.Fatalf("7 天后只剩次日那次的用户: %d", s[7].UniqueUsers)
	}
}

func TestBuildScopes(t *testing.T) {
	inds := []Industry{
		{ID: 1, Code: "foot_bath", Enabled: true},
		{ID: 2, Code: "foot_bath_bluev", ParentID: 1, Enabled: true},
		{ID: 3, Code: "foot_bath_creator", ParentID: 1, Enabled: false}, // 停用二级：模板仍归一级
		{ID: 4, Code: "apparel", Enabled: false},                        // 停用一级：整棵不可见
		{ID: 5, Code: "apparel_shop", ParentID: 4, Enabled: true},
		{ID: 6, Code: "general", Enabled: true},
	}
	rel := map[int64][]int64{
		10: {1},    // 一级直挂
		11: {2},    // 蓝V
		12: {3},    // 停用二级
		13: {5},    // 停用一级下的二级
		14: {2, 6}, // 多行业
		15: {99},   // 未知行业
	}
	scopes := BuildScopes(inds, rel)
	got := map[string][]int64{}
	for _, s := range scopes {
		got[s.Code] = s.Members
	}
	want := map[string][]int64{
		"foot_bath":       {10, 11, 12, 14},
		"foot_bath_bluev": {11, 14},
		"general":         {14},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("范围成员: %v", got)
	}
	if s := scopes[1]; s.Code != "foot_bath_bluev" || s.ParentCode != "foot_bath" {
		t.Fatalf("二级父 code: %+v", s)
	}
	if of := ScopesOfTemplate(scopes, 14); !reflect.DeepEqual(of, []string{"foot_bath", "foot_bath_bluev", "general"}) {
		t.Fatalf("模板所属范围: %v", of)
	}
	if !scopes[0].HasMember(12) || scopes[0].HasMember(13) {
		t.Fatal("HasMember")
	}
}
