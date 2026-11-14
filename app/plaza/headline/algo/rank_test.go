package algo

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// fixture 构造一个范围的输入：模板默认可见、图片、30 天前创建（无新鲜度加成）。
type fixture struct {
	in ScopeInput
}

func newFixture(scope string) *fixture {
	return &fixture{in: ScopeInput{
		Scope: scope, Templates: map[int64]Template{}, Stats: map[int64]Stats{},
		Params: DefaultParams(), Now: now0,
	}}
}

func (f *fixture) tpl(id int64, opts ...func(*Template)) *fixture {
	t := Template{ID: id, MediaType: "image", CreatedAt: now0.AddDate(0, 0, -30), Visible: true}
	for _, o := range opts {
		o(&t)
	}
	f.in.Templates[id] = t
	f.in.Members = append(f.in.Members, id)
	return f
}

// hot 给模板一份信号：uses 次全在 2 小时前、users 个去重用户。
func (f *fixture) hot(id int64, uses, users int) *fixture {
	f.in.Stats[id] = ComputeStats(Signal{TemplateID: id, UniqueUsers: users, Hours: hoursAgo(now0, map[int]int{2: uses})},
		f.in.Templates[id].CreatedAt, now0, f.in.Params)
	return f
}

func video(t *Template)  { t.MediaType = "video" }
func hidden(t *Template) { t.Visible = false }
func createdDaysAgo(d int) func(*Template) {
	return func(t *Template) { t.CreatedAt = now0.AddDate(0, 0, -d) }
}

func ids(items []Item) []int64 {
	out := make([]int64, len(items))
	for i, it := range items {
		out[i] = it.TemplateID
	}
	return out
}

func sources(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Source
	}
	return out
}

func TestRankByScoreAndTieBreak(t *testing.T) {
	f := newFixture("foot_bath").tpl(1).tpl(2).tpl(3).tpl(4)
	f.hot(1, 50, 20).hot(2, 200, 50).hot(3, 100, 30).hot(4, 100, 30)
	f.in.Params.ColdStartFill = false
	r := ComputeScope(f.in)
	// 3 与 4 同分同次数：模板 id 降序
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{2, 4, 3, 1}) {
		t.Fatalf("排序错误: %v", got)
	}
	for i, it := range r.Items {
		if it.Rank != i+1 || it.Source != SourceAuto || it.Score == nil || it.Detail == nil {
			t.Fatalf("条目字段错误: %+v", it)
		}
	}
	if r.Stats.Auto != 4 || r.Stats.Candidates != 4 || r.Stats.Eligible != 4 {
		t.Fatalf("摘要错误: %+v", r.Stats)
	}
}

func TestEligibilityOrderAndRelativeThresholds(t *testing.T) {
	f := newFixture("foot_bath").tpl(1).tpl(2, hidden).tpl(3).tpl(4).tpl(5).tpl(6)
	f.in.Params.ColdStartFill = false
	f.hot(1, 100, 40).hot(2, 100, 40).hot(3, 100, 40).hot(4, 100, 40).hot(5, 1, 1).hot(6, 100, 1)
	f.in.Stats[99] = Stats{Uses7d: 500, Decayed: 400, UniqueUsers: 100} // 不在范围（别的行业的模板）
	f.in.Blocks = []Block{{ID: 1, TemplateID: 3, Scope: "foot_bath", Kind: BlockBlock}}
	f.in.Rules = []ManualRule{{ID: 7, TemplateID: 4, Scope: ScopeAny, StartDate: now0, EndDate: now0, Enabled: true, Why: "运营精选"}}
	r := ComputeScope(f.in)
	want := map[string]int{SkipNotInScope: 1, SkipDisabled: 1, SkipSuppressed: 1, SkipManualExists: 1, SkipBelowMinDecayed: 1, SkipBelowMinUsers: 1}
	if !reflect.DeepEqual(r.Stats.Skip, want) {
		t.Fatalf("排除原因: %v", r.Stats.Skip)
	}
	// 绝对阈值 min-decayed=3 / min-users=3，候选最大 ~93 / 40 → 相对阈值 9.3 / 4 均大于绝对值，取绝对值
	if r.Stats.Thresholds.EffMinDecayed != 3 || r.Stats.Thresholds.EffMinUsers != 3 {
		t.Fatalf("大范围应受绝对阈值约束: %+v", r.Stats.Thresholds)
	}
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{4, 1}) {
		t.Fatalf("榜单应为 人工4 + 算法1: %v %v", got, sources(r.Items))
	}
	// 别的行业的模板（99）绝不进入
	for _, it := range r.Items {
		if it.TemplateID == 99 {
			t.Fatal("跨行业模板进入了榜单")
		}
	}

	// 小行业：最大 decayed≈1.9、用户 2 → 相对阈值 0.19 / 1，模板都能上榜
	g := newFixture("bluev").tpl(1).tpl(2)
	g.in.Params.ColdStartFill = false
	g.hot(1, 2, 2).hot(2, 1, 1)
	r = ComputeScope(g.in)
	if len(r.Items) != 2 || r.Stats.Thresholds.EffMinUsers != 1 || r.Stats.Thresholds.EffMinDecayed >= 1 {
		t.Fatalf("小行业应按相对阈值出榜: %v %+v", ids(r.Items), r.Stats.Thresholds)
	}
	// 行业覆盖可以单独放宽/收紧
	g.in.Params.RelMinRatio = 1
	g.in.Params.MinUniqueUsers = 2
	r = ComputeScope(g.in)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("rel=1 时相对阈值等于范围最大值: %v", got)
	}
}

func TestHysteresisKeepsIncumbent(t *testing.T) {
	// 1 个名额：在榜项 A(1) 分数略低于挑战者 B(2)，在 15% 迟滞内保住；B 明显更高时被挤掉
	build := func(bUses int) ScopeResult {
		f := newFixture("s").tpl(1).tpl(2)
		f.in.Params.TopN, f.in.Params.MaxVisible, f.in.Params.MaxPerMedia, f.in.Params.MaxRankMove = 1, 1, 1, 1
		f.in.Params.ColdStartFill = false
		f.in.Params.MinDwellMinutes = 0
		f.hot(1, 100, 30).hot(2, bUses, 30)
		f.in.Prev = []PrevItem{{TemplateID: 1, Rank: 1, Source: SourceAuto, EnteredAt: now0.Add(-10 * time.Hour)}}
		return ComputeScope(f.in)
	}
	r := build(110)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("迟滞内应保住在榜项: %v", got)
	}
	if !r.Items[0].Detail.Stability.Incumbent || r.Items[0].EnteredAt != now0.Add(-10*time.Hour) {
		t.Fatalf("在榜项痕迹/起点结转错误: %+v", r.Items[0].Detail.Stability)
	}
	if len(r.Stats.NearMiss) != 1 || r.Stats.NearMiss[0].TemplateID != 2 || r.Stats.NearMiss[0].Reason != SkipCutBySlots {
		t.Fatalf("近邻候选: %+v", r.Stats.NearMiss)
	}
	r = build(100000)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("明显更高应挤掉在榜项: %v", got)
	}
	if r.Items[0].EnteredAt != now0 {
		t.Fatal("新进者起点应为 now")
	}
}

func TestMinDwellProtectsUnlessSuppressed(t *testing.T) {
	build := func(entered time.Duration, block bool) ScopeResult {
		f := newFixture("s").tpl(1).tpl(2)
		f.in.Params.ColdStartFill = false
		f.hot(2, 100, 30)
		// 1 号已经没有热度（低于阈值），但刚入榜 30 分钟
		f.in.Prev = []PrevItem{{TemplateID: 1, Rank: 1, Source: SourceAuto, EnteredAt: now0.Add(-entered)}}
		if block {
			f.in.Blocks = []Block{{TemplateID: 1, Scope: ScopeAny, Kind: BlockCancel, Until: ptrTime(now0.Add(time.Hour))}}
		}
		return ComputeScope(f.in)
	}
	r := build(30*time.Minute, false)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{2, 1}) {
		t.Fatalf("驻留期内应保留: %v", got)
	}
	if r.Items[1].Detail.PickedBy != "DWELL" || !r.Items[1].Detail.Stability.DwellProtected {
		t.Fatalf("应标记驻留保护: %+v", r.Items[1].Detail)
	}
	if got := ids(build(3*time.Hour, false).Items); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("驻留期过后应按资格掉榜: %v", got)
	}
	if got := ids(build(30*time.Minute, true).Items); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("被运营取消的项不受驻留保护: %v", got)
	}
}

func TestRankMoveClamp(t *testing.T) {
	f := newFixture("s")
	for id := int64(1); id <= 8; id++ {
		f.tpl(id)
	}
	f.in.Params.MaxPerMedia = 10
	f.in.Params.ColdStartFill = false
	f.in.Params.MinDwellMinutes = 0
	// 上一版 1 号第 1；这一版它的热度垫底（目标第 8），名次限动 3 → 最多掉到第 4
	f.hot(1, 40, 30)
	for id := int64(2); id <= 8; id++ {
		f.hot(id, 1000+int(id)*100, 30)
	}
	f.in.Prev = []PrevItem{{TemplateID: 1, Rank: 1, Source: SourceAuto, EnteredAt: now0.Add(-5 * time.Hour)}}
	r := ComputeScope(f.in)
	if r.Items[3].TemplateID != 1 {
		t.Fatalf("应夹到第 4 名: %v", ids(r.Items))
	}
	if d := r.Items[3].Detail.Stability; d.RankMovedFrom == nil || *d.RankMovedFrom != 1 || d.RankClampedBy != 4 {
		t.Fatalf("限动痕迹: %+v", d)
	}
	// 其余按分数降序
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{8, 7, 6, 1, 5, 4, 3, 2}) {
		t.Fatalf("顺序: %v", got)
	}
}

func TestMediaCapNoBackfill(t *testing.T) {
	f := newFixture("s").tpl(1).tpl(2).tpl(3).tpl(4).tpl(5, video)
	f.in.Params.MaxPerMedia = 2
	f.in.Params.ColdStartFill = false
	f.hot(1, 400, 40).hot(2, 300, 40).hot(3, 200, 40).hot(4, 150, 40).hot(5, 100, 40)
	r := ComputeScope(f.in)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{1, 2, 5}) {
		t.Fatalf("每种媒体最多 2 个: %v", got)
	}
	if r.Stats.Skip[SkipDiversityMedia] != 2 {
		t.Fatalf("DIVERSITY_MEDIA 计数: %v", r.Stats.Skip)
	}
}

func TestColdStartFillOnlyFromSameScope(t *testing.T) {
	// 新行业：3 个合格 + 若干无热度模板；top-n=10 → 3 AUTO + 最新上架补位
	f := newFixture("new_ind")
	for id := int64(1); id <= 8; id++ {
		f.tpl(id, createdDaysAgo(int(id))) // id 越小越新
	}
	f.hot(6, 100, 30).hot(7, 90, 30).hot(8, 80, 30)
	f.in.Params.MaxPerMedia = 10
	f.in.Templates[100] = Template{ID: 100, MediaType: "image", CreatedAt: now0, Visible: true} // 别的行业最新模板
	f.in.Params.MinDwellMinutes = 0
	r := ComputeScope(f.in)
	if got := sources(r.Items); !reflect.DeepEqual(got, []string{"AUTO", "AUTO", "AUTO", "FILL", "FILL", "FILL", "FILL", "FILL"}) {
		t.Fatalf("来源: %v", got)
	}
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{6, 7, 8, 1, 2, 3, 4, 5}) {
		t.Fatalf("补位应按上架时间降序且只来自本范围: %v", got)
	}
	for _, it := range r.Items[3:] {
		if it.Why != WhyFill || it.Score != nil || it.Uses7d != 0 || it.Trend != nil || !it.IsNew {
			t.Fatalf("FILL 字段: %+v", it)
		}
	}
	if r.Stats.Fill != 5 {
		t.Fatalf("fill=%d", r.Stats.Fill)
	}
	// 关掉补位
	f.in.Params.ColdStartFill = false
	if r = ComputeScope(f.in); len(r.Items) != 3 {
		t.Fatalf("cold-start-fill=false 时不补: %v", ids(r.Items))
	}
	// 有合格算法项出现时 FILL 被顶掉；FILL 不是在榜项
	f.in.Params.ColdStartFill = true
	f.in.Params.TopN, f.in.Params.MaxPerMedia, f.in.Params.MaxRankMove = 4, 4, 3
	f.in.Prev = ToPrev(ComputeScope(f.in).Items) // 6,7,8 + FILL 1
	f.hot(2, 120, 30)
	r = ComputeScope(f.in)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{6, 7, 8, 2}) || !reflect.DeepEqual(sources(r.Items), []string{"AUTO", "AUTO", "AUTO", "AUTO"}) {
		t.Fatalf("FILL 应被新合格项顶掉（在榜项靠迟滞保住前三）: %v %v", got, sources(r.Items))
	}
}

func TestColdStartShortListWhenScopeSmall(t *testing.T) {
	f := newFixture("tiny").tpl(1).tpl(2).tpl(3, hidden)
	r := ComputeScope(f.in)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{2, 1}) {
		t.Fatalf("范围内只有 2 个可见模板，榜单就是 2 条: %v", got)
	}
	empty := newFixture("empty")
	if r = ComputeScope(empty.in); len(r.Items) != 0 || r.Items == nil {
		t.Fatalf("空范围应返回空列表（非 nil）: %#v", r.Items)
	}
}

func TestManualPlacementPinsAndSlots(t *testing.T) {
	f := newFixture("s")
	for id := int64(1); id <= 12; id++ {
		f.tpl(id)
		f.hot(id, 1000-int(id)*10, 40)
	}
	f.tpl(20).tpl(21).tpl(22)
	f.in.Params.MaxVisible = 12
	f.in.Params.MaxPerMedia = 10
	f.in.Rules = []ManualRule{
		{ID: 1, TemplateID: 20, Scope: "s", StartDate: now0, EndDate: now0, Enabled: true, Why: "定位第 3", PinPosition: 3},
		{ID: 2, TemplateID: 21, Scope: ScopeAny, StartDate: now0.AddDate(0, 0, -1), EndDate: now0.AddDate(0, 0, 3), Enabled: true, Why: "无定位"},
		{ID: 3, TemplateID: 22, Scope: "s", StartDate: now0, EndDate: now0, Enabled: true, Why: "定位第 3（冲突顺延）", PinPosition: 3},
		{ID: 4, TemplateID: 5, Scope: "other", StartDate: now0, EndDate: now0, Enabled: true, Why: "别的范围"},
		{ID: 5, TemplateID: 6, Scope: "s", StartDate: now0.AddDate(0, 0, 1), EndDate: now0.AddDate(0, 0, 2), Enabled: true, Why: "明天才生效"},
		{ID: 6, TemplateID: 7, Scope: "s", StartDate: now0, EndDate: now0, Enabled: false, Why: "停用"},
	}
	r := ComputeScope(f.in)
	// 人工 3 条 → autoSlots = min(10, 12−3) = 9；总 12 条
	if r.Stats.AutoSlots != 9 || len(r.Items) != 12 || r.Stats.Manual != 3 {
		t.Fatalf("名额: slots=%d len=%d manual=%d", r.Stats.AutoSlots, len(r.Items), r.Stats.Manual)
	}
	if r.Items[0].TemplateID != 21 || r.Items[0].Source != SourceManual || r.Items[0].HeadlineID != 2 {
		t.Fatalf("无定位人工项应排在算法项之前: %+v", r.Items[0])
	}
	if r.Items[2].TemplateID != 20 || r.Items[3].TemplateID != 22 {
		t.Fatalf("定位: %v", ids(r.Items))
	}
	if r.Items[1].Source != SourceAuto || r.Items[1].TemplateID != 1 {
		t.Fatalf("第 2 名应为最热算法项: %+v", r.Items[1])
	}
	// 别的范围 / 未到期 / 停用的规则不生效，对应模板走算法
	for _, it := range r.Items {
		if (it.TemplateID == 5 || it.TemplateID == 6 || it.TemplateID == 7) && it.Source != SourceAuto {
			t.Fatalf("不生效的规则被套用: %+v", it)
		}
	}
	// 人工项永不被截断：人工数超过 max-visible 时算法名额为 0
	f.in.Params.MaxVisible = 10
	f.in.Params.TopN = 10
	for i := int64(0); i < 12; i++ {
		f.in.Rules = append(f.in.Rules, ManualRule{ID: 100 + i, TemplateID: i + 1, Scope: "s", StartDate: now0, EndDate: now0, Enabled: true, Why: "x"})
	}
	r = ComputeScope(f.in)
	if r.Stats.AutoSlots != 0 || r.Stats.Auto != 0 || r.Stats.Manual != 15 {
		t.Fatalf("人工项不截断: %+v", r.Stats)
	}
}

func TestPinBeyondLengthGoesLast(t *testing.T) {
	items := Place(
		[]Item{{TemplateID: 9, HeadlineID: 1, PinPosition: 50}},
		[]ManualRule{{ID: 1}},
		[]Item{{TemplateID: 1}, {TemplateID: 2}}, nil)
	if got := ids(items); !reflect.DeepEqual(got, []int64{1, 2, 9}) || items[2].Rank != 3 {
		t.Fatalf("越界定位应放末位: %v", got)
	}
}

func TestScopeSemanticsOfRulesAndBlocks(t *testing.T) {
	members := map[int64]bool{1: true, 2: true}
	tpls := map[int64]Template{1: {ID: 1, Visible: true}, 2: {ID: 2, Visible: true}, 3: {ID: 3, Visible: true}}
	rules := []ManualRule{
		{ID: 1, TemplateID: 1, Scope: ScopeAny, StartDate: now0.AddDate(0, 0, -1), EndDate: now0, Enabled: true, Why: "通配"},
		{ID: 2, TemplateID: 1, Scope: "s", StartDate: now0.AddDate(0, 0, -3), EndDate: now0, Enabled: true, Why: "显式"},
		{ID: 3, TemplateID: 3, Scope: ScopeAny, StartDate: now0, EndDate: now0, Enabled: true, Why: "不属于本范围"},
		{ID: 4, TemplateID: 2, Scope: "s", StartDate: now0.AddDate(0, 0, -1), EndDate: now0, Enabled: true, SortOrder: 5},
		{ID: 5, TemplateID: 2, Scope: "s", StartDate: now0.AddDate(0, 0, -1), EndDate: now0, Enabled: true, SortOrder: 1},
	}
	got := EffectiveManual("s", members, tpls, rules, nil, now0)
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 5 {
		t.Fatalf("显式范围优先于 *、同起始日 sort_order 小者优先、非成员不生效: %+v", got)
	}
	// ALL 不接受规则
	if len(EffectiveManual(ScopeAll, members, tpls, rules, nil, now0)) != 0 {
		t.Fatal("ALL 不应套用规则")
	}
	// 抑制精确匹配：对二级的取消不影响一级
	blocks := []Block{
		{TemplateID: 1, Scope: "foot_bath_bluev", Kind: BlockCancel, Until: ptrTime(now0.Add(time.Hour))},
		{TemplateID: 2, Scope: ScopeAny, Kind: BlockBlock},
		{TemplateID: 3, Scope: "foot_bath", Kind: BlockCancel, Until: ptrTime(now0.Add(-time.Minute))}, // 已过期
	}
	parent := SuppressedSet("foot_bath", blocks, now0)
	if parent[1] || !parent[2] || parent[3] {
		t.Fatalf("一级抑制集合: %v", parent)
	}
	if child := SuppressedSet("foot_bath_bluev", blocks, now0); !child[1] || !child[2] {
		t.Fatalf("二级抑制集合: %v", child)
	}
	if len(SuppressedSet(ScopeAll, blocks, now0)) != 0 {
		t.Fatal("ALL 不受抑制")
	}
	// 被抑制的模板的人工推荐也不生效
	if got := EffectiveManual("s", members, tpls, rules, map[int64]bool{1: true}, now0); len(got) != 1 || got[0].TemplateID != 2 {
		t.Fatalf("抑制优先于人工: %+v", got)
	}
}

func TestDeterministicOutput(t *testing.T) {
	build := func() RefreshInput {
		in := RefreshInput{Config: Config{Global: DefaultParams()}, Templates: map[int64]Template{}, Signals: map[int64]Signal{}, Now: now0, IncludeGlobal: true}
		var members []int64
		for id := int64(1); id <= 30; id++ {
			in.Templates[id] = Template{ID: id, MediaType: []string{"image", "video"}[id%2], CreatedAt: now0.AddDate(0, 0, -int(id)), Visible: true}
			members = append(members, id)
			if id%3 != 0 {
				in.Signals[id] = Signal{TemplateID: id, UniqueUsers: int(id), Hours: hoursAgo(now0, map[int]int{int(id): int(id) * 3, int(id) * 5: 7})}
			}
		}
		in.Scopes = []Scope{{Code: "b", Members: members[:20]}, {Code: "a", Members: members[10:]}}
		in.Prev = map[string][]PrevItem{"a": {{TemplateID: 11, Rank: 1, Source: SourceAuto, EnteredAt: now0.Add(-time.Hour)}}}
		return in
	}
	a, _ := json.Marshal(Refresh(build()))
	b, _ := json.Marshal(Refresh(build()))
	if string(a) != string(b) {
		t.Fatal("同输入两次输出不一致")
	}
	r := Refresh(build())
	if len(r.Scopes) != 3 || r.Scopes[0].Scope != "a" || r.Scopes[1].Scope != "b" || r.Scopes[2].Scope != ScopeAll {
		t.Fatalf("范围顺序: %v", r.Scopes)
	}
}

func TestSubIndustryComputedIndependently(t *testing.T) {
	// 12.5：一级榜前 10 里蓝V号只占 1 个，蓝V号自身有 6 个合格模板 → 蓝V号榜有 6 个（不是一级榜的过滤结果）
	inds := []Industry{
		{ID: 1, Code: "foot_bath", Enabled: true},
		{ID: 2, Code: "foot_bath_bluev", ParentID: 1, Enabled: true},
	}
	rel := map[int64][]int64{}
	in := RefreshInput{Config: Config{Global: DefaultParams()}, Templates: map[int64]Template{}, Signals: map[int64]Signal{}, Now: now0}
	in.Config.Global.ColdStartFill = false
	add := func(id int64, ind int64, uses int) {
		rel[id] = []int64{ind}
		in.Templates[id] = Template{ID: id, MediaType: "image", CreatedAt: now0.AddDate(0, 0, -30), Visible: true}
		in.Signals[id] = Signal{TemplateID: id, UniqueUsers: 30, Hours: hoursAgo(now0, map[int]int{3: uses})}
	}
	for id := int64(1); id <= 12; id++ {
		add(id, 1, 1000+int(id)) // 一级直挂，热
	}
	add(100, 2, 2000) // 蓝V 最热的一个
	for id := int64(101); id <= 105; id++ {
		add(id, 2, 50) // 蓝V 其余，偏冷
	}
	in.Config.Global.MaxPerMedia = 10
	in.Scopes = BuildScopes(inds, rel)
	r := Refresh(in)
	parent, _ := r.Scope("foot_bath")
	child, _ := r.Scope("foot_bath_bluev")
	bluevInParent := 0
	for _, it := range parent.Items {
		if it.TemplateID >= 100 {
			bluevInParent++
		}
	}
	if bluevInParent != 1 || len(parent.Items) != 10 {
		t.Fatalf("一级榜: %v", ids(parent.Items))
	}
	if len(child.Items) != 6 {
		t.Fatalf("蓝V号榜应独立算出 6 个: %v", ids(child.Items))
	}
	for _, it := range child.Items {
		if it.TemplateID < 100 {
			t.Fatalf("二级榜混入了一级直挂模板: %v", ids(child.Items))
		}
	}
}

func TestGlobalScopeIsRawHeat(t *testing.T) {
	in := GlobalInput{Templates: map[int64]Template{}, Stats: map[int64]Stats{}, Params: DefaultParams(), Now: now0}
	in.Params.GlobalTopN = 3
	for id := int64(1); id <= 6; id++ {
		in.Templates[id] = Template{ID: id, MediaType: "image", CreatedAt: now0.AddDate(0, 0, -30), Visible: id != 6}
		in.Stats[id] = ComputeStats(Signal{UniqueUsers: 30, Hours: hoursAgo(now0, map[int]int{1: int(id) * 100})}, in.Templates[id].CreatedAt, now0, in.Params)
	}
	in.Stats[7] = Stats{Uses7d: 1, Decayed: 0.5, UniqueUsers: 1}
	in.Templates[7] = Template{ID: 7, Visible: true}
	r := ComputeGlobal(in)
	if got := ids(r.Items); !reflect.DeepEqual(got, []int64{5, 4, 3}) {
		t.Fatalf("全局榜: %v", got)
	}
	if r.Stats.Skip[SkipDisabled] != 1 || r.Stats.Skip[SkipBelowMinDecayed] != 1 || r.Stats.Skip[SkipCutBySlots] != 2 {
		t.Fatalf("全局榜排除: %v", r.Stats.Skip)
	}
}

func TestDiff(t *testing.T) {
	prev := []PrevItem{{TemplateID: 1, Rank: 1}, {TemplateID: 2, Rank: 2}, {TemplateID: 3, Rank: 3}}
	next := []Item{{TemplateID: 2, Rank: 1}, {TemplateID: 1, Rank: 2}, {TemplateID: 4, Rank: 3}}
	d := Diff(prev, next)
	if !reflect.DeepEqual(d.Enter, []int64{4}) || !reflect.DeepEqual(d.Exit, []int64{3}) || len(d.Moved) != 2 || d.Moved[0] != (RankMoved{1, 1, 2}) {
		t.Fatalf("diff: %+v", d)
	}
	if !Diff(prev, []Item{{TemplateID: 1, Rank: 1}, {TemplateID: 2, Rank: 2}, {TemplateID: 3, Rank: 3}}).Empty() {
		t.Fatal("相同应为空 diff")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
