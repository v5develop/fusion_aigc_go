package core

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/xerr"
)

func reasonOf(err error) string { return xerr.ReasonOf(err) }

func codeOf(err error) int {
	c, _, _ := xerr.Parse(err)
	return c
}

func catalogOf(t *testing.T, src *fakeSource) *Catalog {
	t.Helper()
	c, err := BuildCatalog(context.Background(), src, t0, false)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func idsOf(recs []*tplRec) []int64 {
	out := []int64{}
	for _, r := range recs {
		out = append(out, r.t.ID)
	}
	return out
}

func eq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestResolveStrictVertical(t *testing.T) {
	c := catalogOf(t, stdFixture())
	// 缺 industry（含只给 subIndustry）→ 400 口径的 PARAM_MISSING
	for _, in := range [][2]string{{"", ""}, {"", "foot_bath_bluev"}, {"  ", ""}} {
		_, err := c.Resolve(in[0], in[1])
		if codeOf(err) != xerr.ParamMissing || err.Error() == "" || !strings.Contains(err.Error(), "请先选择行业") {
			t.Errorf("%v 应为 PARAM_MISSING「请先选择行业」: %v", in, err)
		}
	}
	// ALL / 不存在 / 停用 / 父级停用 / 二级不属于它 → INDUSTRY_NOT_FOUND
	for _, in := range [][2]string{
		{"ALL", ""}, {"all", ""}, {"nope", ""}, {"dead", ""}, {"dead_sub", ""},
		{"foot_bath_creator", ""},                // 停用的二级
		{"spa", "foot_bath_bluev"},               // 二级不属于所给一级
		{"foot_bath", "foot_bath"},               // sub 不能是一级
		{"foot_bath_bluev", "foot_bath_creator"}, // 二级下不能再选二级
		{"foot_bath", "ALL"},
	} {
		_, err := c.Resolve(in[0], in[1])
		if reasonOf(err) != ReasonIndustryNotFound {
			t.Errorf("%v 应为 INDUSTRY_NOT_FOUND: %v", in, err)
		}
	}
	sc, err := c.Resolve("foot_bath", "")
	if err != nil || sc.Code() != "foot_bath" || len(sc.ids) != 3 { // 一级含全部子级（含停用的）
		t.Fatalf("一级范围: %+v %v", sc, err)
	}
	if sc, err = c.Resolve("foot_bath", "foot_bath_bluev"); err != nil || sc.Code() != "foot_bath_bluev" || len(sc.ids) != 1 {
		t.Fatalf("二级范围: %+v %v", sc, err)
	}
	// industry 直接给二级 code 也行
	if sc, err = c.Resolve("foot_bath_bluev", ""); err != nil || sc.Sub == nil || sc.Root.Code != "foot_bath" {
		t.Fatalf("二级作 industry: %+v %v", sc, err)
	}
}

func TestMembershipParentIncludesChildren(t *testing.T) {
	c := catalogOf(t, stdFixture())
	sc, _ := c.Resolve("foot_bath", "")
	// 101、102 直挂；103 挂二级 bluev；105 挂停用二级 creator —— 停用子级其模板仍归父级
	got := idsOf(c.List(sc, ListQuery{Page: 1, Size: 50}, t0).Recs)
	// 排序：minSort 升序（103:5, 102:10, 101:20, 105:0 → 105 在最前）
	if !eq(got, []int64{105, 103, 102, 101}) {
		t.Fatalf("一级 = 直挂 ∪ 子级，行业内顺序: %v", got)
	}
	sub, _ := c.Resolve("foot_bath", "foot_bath_bluev")
	if got := idsOf(c.List(sub, ListQuery{Page: 1, Size: 50}, t0).Recs); !eq(got, []int64{103}) {
		t.Fatalf("二级独立: %v", got)
	}
	// 其它行业的模板绝不出现
	spa, _ := c.Resolve("spa", "")
	if got := idsOf(c.List(spa, ListQuery{Page: 1, Size: 50}, t0).Recs); !eq(got, []int64{104}) {
		t.Fatalf("spa: %v", got)
	}
}

func TestListOrderTieBreakers(t *testing.T) {
	src := stdFixture()
	// 三个模板同一 sort_order：update_time 降序，再 id 降序
	src.tpls = []model.Template{
		tpl(1, "x", "image", t0.Add(-3*time.Hour)),
		tpl(2, "y", "image", t0.Add(-1*time.Hour)),
		tpl(3, "z", "image", t0.Add(-1*time.Hour)),
	}
	src.rels = []model.TemplateIndustry{rel(1, 4, 10), rel(2, 4, 10), rel(3, 4, 10)}
	c := catalogOf(t, src)
	sc, _ := c.Resolve("spa", "")
	if got := idsOf(c.List(sc, ListQuery{Page: 1, Size: 10}, t0).Recs); !eq(got, []int64{3, 2, 1}) {
		t.Fatalf("同序时 update_time 降序再 id 降序: %v", got)
	}
}

func TestKeywordSearchOnlyWithinIndustry(t *testing.T) {
	c := catalogOf(t, stdFixture())
	foot, _ := c.Resolve("foot_bath", "")
	// 标题 / 描述 / 标签都能命中，不区分大小写
	for _, kw := range []string{"a", "A", "d-a", "标签-A"} {
		got := idsOf(c.List(foot, ListQuery{Keyword: kw, Page: 1, Size: 10}, t0).Recs)
		if len(got) == 0 || got[len(got)-1] != 101 && !contains(got, 101) {
			t.Errorf("keyword=%q 应命中 101: %v", kw, got)
		}
	}
	// D 属于 spa：在 foot_bath 里搜 D 搜不到（没有跨行业搜索）；到 spa 里搜才有
	if got := c.List(foot, ListQuery{Keyword: "标签-D", Page: 1, Size: 10}, t0); got.Total != 0 {
		t.Fatalf("不得跨行业搜索: %v", idsOf(got.Recs))
	}
	spa, _ := c.Resolve("spa", "")
	if got := c.List(spa, ListQuery{Keyword: "标签-D", Page: 1, Size: 10}, t0); got.Total != 1 {
		t.Fatalf("同行业能搜到: %+v", got)
	}
}

func contains(s []int64, v int64) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestMediaFilterPagingAndCounts(t *testing.T) {
	c := catalogOf(t, stdFixture())
	foot, _ := c.Resolve("foot_bath", "")
	vid := c.List(foot, ListQuery{MediaType: "video", Page: 1, Size: 10}, t0)
	if vid.Total != 1 || vid.Recs[0].t.ID != 103 {
		t.Fatalf("video: %+v", vid)
	}
	p2 := c.List(foot, ListQuery{Page: 2, Size: 3}, t0)
	if p2.Total != 4 || len(p2.Recs) != 1 {
		t.Fatalf("分页: %+v", p2)
	}
	if over := c.List(foot, ListQuery{Page: 9, Size: 3}, t0); over.Total != 4 || len(over.Recs) != 0 {
		t.Fatalf("越界页: %+v", over)
	}
	n := c.Counts(foot, t0)
	// 图片 3（101/102/105）视频 1；近 7 天上新：103(1h) 105(2h) 102(24h)；101 是 48h 前也算；D 不在范围
	if n.Image != 3 || n.Video != 1 || n.NewCount7d != 4 {
		t.Fatalf("counts: %+v", n)
	}
	// 计数与列表条数一致
	if all := c.List(foot, ListQuery{Page: 1, Size: 100}, t0); all.Total != n.Image+n.Video {
		t.Fatalf("counts 与列表条数不一致: %d vs %d", all.Total, n.Image+n.Video)
	}
}

func TestLifecycleWindowIsSecondExact(t *testing.T) {
	src := stdFixture()
	on := t0.Add(10 * time.Minute)
	off := t0.Add(20 * time.Minute)
	a := tpl(201, "win", "image", t0.Add(-time.Hour))
	a.OnlineAt = sql.NullTime{Time: on, Valid: true}
	a.OfflineAt = sql.NullTime{Time: off, Valid: true}
	draft := tpl(202, "draft", "image", t0)
	draft.State = model.StateDraft
	offline := tpl(203, "offline", "image", t0)
	offline.State = model.StateOffline
	deleted := tpl(204, "deleted", "image", t0)
	deleted.Deleted = 1
	src.tpls = []model.Template{a, draft, offline, deleted}
	src.rels = []model.TemplateIndustry{rel(201, 4, 0), rel(202, 4, 0), rel(203, 4, 0), rel(204, 4, 0)}
	c := catalogOf(t, src)
	spa, _ := c.Resolve("spa", "")
	count := func(at time.Time) int { return c.List(spa, ListQuery{Page: 1, Size: 10}, at).Total }
	if count(on.Add(-time.Second)) != 0 {
		t.Fatal("上线前一秒不可见")
	}
	if count(on) != 1 {
		t.Fatal("上线时刻（含）可见")
	}
	if count(off.Add(-time.Second)) != 1 {
		t.Fatal("下线前一秒可见")
	}
	if count(off) != 0 {
		t.Fatal("下线时刻（不含）不可见")
	}
	// 草稿 / 下线 / 回收站任何时刻都不可见
	for _, id := range []int64{202, 203, 204} {
		if _, err := c.Get(id, on); reasonOf(err) == "" && err == nil {
			t.Errorf("%d 不应可见", id)
		}
	}
	if _, err := c.Get(201, on); err != nil {
		t.Fatalf("窗口内可按 id 取: %v", err)
	}
	if _, err := c.Get(201, off); codeOf(err) != xerr.NotFound || !strings.Contains(err.Error(), "刚刚下架") {
		t.Fatalf("窗口外 RESOURCE_NOT_FOUND + E-13 文案: %v", err)
	}
}

func TestDirectoryRules(t *testing.T) {
	c := catalogOf(t, stdFixture())
	dir := c.Directory(t0)
	var codes []string
	for _, d := range dir {
		codes = append(codes, d.Code)
	}
	// spa(10) foot_bath(20) forced(80)；empty 无模板且未开 show_when_empty 不出现；dead 停用整棵不可见
	if strings.Join(codes, ",") != "spa,foot_bath,forced" {
		t.Fatalf("目录顺序与可见性: %v", codes)
	}
	foot := dir[1]
	if len(foot.Children) != 1 || foot.Children[0].Code != "foot_bath_bluev" {
		t.Fatalf("停用的二级不出现在 children: %+v", foot.Children)
	}
	if foot.Hint == "" || foot.Examples == nil || foot.NextActions == nil || foot.Children == nil {
		t.Fatalf("hint 必有，数组字段是 [] 而不是 null: %+v", foot)
	}
	// 目录里没有任何数量 / 模板内容字段
	b, _ := jsonMarshal(dir)
	for _, bad := range []string{"count", "template", "total"} {
		if strings.Contains(strings.ToLower(string(b)), bad) {
			t.Errorf("目录不得含 %s: %s", bad, b)
		}
	}
	// 窗口外：把所有模板的可见窗口移到未来，行业按「无可见模板」隐藏，show_when_empty 的仍在
	src := stdFixture()
	for i := range src.tpls {
		src.tpls[i].OnlineAt = sql.NullTime{Time: t0.Add(time.Hour), Valid: true}
	}
	c2 := catalogOf(t, src)
	dir2 := c2.Directory(t0)
	if len(dir2) != 1 || dir2[0].Code != "forced" {
		t.Fatalf("无可见模板的行业不出现: %+v", dir2)
	}
}

func TestDirectoryParentKeptWhenOnlyChildHasTemplates(t *testing.T) {
	src := stdFixture()
	src.rels = []model.TemplateIndustry{rel(103, 2, 0)} // 只有二级有模板
	c := catalogOf(t, src)
	dir := c.Directory(t0)
	if len(dir) != 2 || dir[0].Code != "foot_bath" || len(dir[0].Children) != 1 || dir[1].Code != "forced" {
		t.Fatalf("一级自身没有模板但有非空子级则保留（forced 是 show_when_empty）: %+v", dir)
	}
}

func TestServiceIndustriesGate(t *testing.T) {
	s := newTestService(stdFixture())
	d, err := s.Industries(context.Background())
	if err != nil || !d.GateEnabled || len(d.Industries) != 3 {
		t.Fatalf("%+v %v", d, err)
	}
	s.cfg.Industry.Enabled = false
	s.cat = nil
	if d, _ := s.Industries(context.Background()); d.GateEnabled || d.Industries == nil || len(d.Industries) != 0 {
		t.Fatalf("开关关闭：gateEnabled=false 且 industries=[]（不降级成全量）: %+v", d)
	}
	// 没有任何可展示行业 → gateEnabled=false
	s2 := newTestService(&fakeSource{})
	if d, _ := s2.Industries(context.Background()); d.GateEnabled || len(d.Industries) != 0 {
		t.Fatalf("0 行业: %+v", d)
	}
}

func TestParseListParams(t *testing.T) {
	q := url.Values{"industry": {"spa"}, "page": {"2"}, "size": {"30"}}
	p, err := ParseListParams(q)
	if err != nil || p.Page != 2 || p.Size != 30 || p.Industry != "spa" {
		t.Fatalf("%+v %v", p, err)
	}
	p, _ = ParseListParams(url.Values{})
	if p.Page != 1 || p.Size != 24 {
		t.Fatalf("默认 page=1 size=24: %+v", p)
	}
	for _, bad := range []url.Values{{"page": {"0"}}, {"size": {"-1"}}, {"page": {"x"}}, {"size": {"0"}}} {
		if _, err := ParseListParams(bad); codeOf(err) != xerr.ParamError {
			t.Errorf("%v 应为 PARAM_INVALID: %v", bad, err)
		}
	}
}

func TestServiceTemplatesValidationOrderAndShape(t *testing.T) {
	s := newTestService(stdFixture())
	ctx := context.Background()
	if _, err := s.Templates(ctx, ListParams{Page: 1, Size: 10}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("缺 industry: %v", err)
	}
	if _, err := s.Templates(ctx, ListParams{Industry: "spa", Sub: "", MediaType: "gif", Page: 1, Size: 10}); codeOf(err) != xerr.ParamError {
		t.Fatalf("mediaType 非法: %v", err)
	}
	if _, err := s.Templates(ctx, ListParams{Industry: "spa", Page: 0, Size: 10}); codeOf(err) != xerr.ParamError {
		t.Fatalf("page<1: %v", err)
	}
	if _, err := s.Templates(ctx, ListParams{Industry: "spa", Keyword: strings.Repeat("字", 51), Page: 1, Size: 10}); codeOf(err) != xerr.ParamError {
		t.Fatalf("keyword >50: %v", err)
	}
	if _, err := s.Templates(ctx, ListParams{Industry: "ALL", Page: 1, Size: 10}); reasonOf(err) != ReasonIndustryNotFound {
		t.Fatalf("ALL: %v", err)
	}
	pg, err := s.Templates(ctx, ListParams{Industry: "foot_bath", Page: 1, Size: 500})
	if err != nil || pg.Total != 4 || pg.Pages != 1 || pg.Current != 1 {
		t.Fatalf("size>100 夹到 100: %+v %v", pg, err)
	}
	it := pg.Data[0]
	if it.Industries == nil || len(pg.Data[3].Industries) == 0 {
		t.Fatalf("industries 只含启用的行业，且不为 null: %+v", it.Industries)
	}
	if it.AgentSkillID == nil || *it.AgentSkillID != "1876543210987654321" {
		t.Fatalf("agentSkillId 以字符串下发且大整数不丢精度: %v", it.AgentSkillID)
	}
	if strings.Contains(string(it.Payload), "SECRET") || strings.Contains(string(it.Payload), "agentSkillId") {
		t.Fatalf("payload 不得含 prompt 与元数据键: %s", it.Payload)
	}
	if it.TargetChannels == nil || it.Tags == nil {
		t.Fatalf("字段齐全: %+v", it)
	}
}

func TestTemplateByIDOnlyReturnsOne(t *testing.T) {
	s := newTestService(stdFixture())
	it, err := s.TemplateByID(context.Background(), 104)
	if err != nil || it.PlazaTemplateID != 104 || len(it.Industries) != 1 || it.Industries[0].Code != "spa" {
		t.Fatalf("%+v %v", it, err)
	}
	if _, err := s.TemplateByID(context.Background(), 99999); codeOf(err) != xerr.NotFound {
		t.Fatalf("不存在: %v", err)
	}
}

func TestUsageVisibleThreshold(t *testing.T) {
	s := newTestService(stdFixture())
	c := catalogOf(t, stdFixture())
	rec := c.tpls[101]
	low := s.item(c, rec, map[int64]int64{101: 19}, t0)
	if low.UsageVisible || low.UsageCount != nil {
		t.Fatalf("低于阈值不下发具体数字: %+v", low)
	}
	hi := s.item(c, rec, map[int64]int64{101: 20}, t0)
	if !hi.UsageVisible || hi.UsageCount == nil || *hi.UsageCount != 20 {
		t.Fatalf("达到阈值下发: %+v", hi)
	}
	b, _ := jsonMarshal(low)
	if !strings.Contains(string(b), `"usageCount":null`) {
		t.Fatalf("低于阈值 usageCount 为 null: %s", b)
	}
}

func TestIsNewAndAuthor(t *testing.T) {
	s := newTestService(stdFixture())
	c := catalogOf(t, stdFixture())
	if !s.item(c, c.tpls[103], nil, t0).IsNew {
		t.Fatal("1 小时前创建算新上架")
	}
	if s.item(c, c.tpls[104], nil, t0).IsNew {
		t.Fatal("100 天前不算")
	}
	if got := s.item(c, c.tpls[101], nil, t0).AuthorName; got != "官方" {
		t.Fatalf("默认作者: %q", got)
	}
	r := *c.tpls[101]
	r.t.DataJSON = []byte(`{"authorName":" 小店 ","result":[]}`)
	if got := s.item(c, &r, nil, t0).AuthorName; got != "小店" {
		t.Fatalf("数据里的 authorName: %q", got)
	}
}

func TestChannelsResolveOverrideAndValidate(t *testing.T) {
	c := catalogOf(t, stdFixture())
	got := c.resolveChannels([]byte(`["douyin_9_16","off","ghost",{"code":"douyin_9_16","width":540,"height":960}]`))
	if len(got) != 2 || got[0].Width != 1080 || got[1].Width != 540 || got[1].Ratio != "9:16" || got[1].Name != "抖音 9:16" {
		t.Fatalf("停用 / 未知渠道跳过，覆盖优先于字典但比例来自字典: %+v", got)
	}
	dict := c.channels
	if err := validateChannels([]byte(`["douyin_9_16",{"code":"douyin_9_16","width":540,"height":960}]`), dict); err != nil {
		t.Fatalf("合法: %v", err)
	}
	for _, bad := range []string{`["ghost"]`, `["off"]`, `[{"code":"douyin_9_16","width":500,"height":500}]`, `[{"code":"douyin_9_16","width":540}]`, `{"a":1}`, `[1]`} {
		if err := validateChannels([]byte(bad), dict); codeOf(err) != xerr.ParamError {
			t.Errorf("%s 应被拒: %v", bad, err)
		}
	}
}

func TestThumbURL(t *testing.T) {
	rule := "?x-oss-process=image/resize,w_640/format,webp"
	oss := "https://bkt.oss-cn-hangzhou.aliyuncs.com/a/b.webp"
	if got := thumbURL(oss, rule); got != oss+rule {
		t.Fatal(got)
	}
	if got := thumbURL(oss, rule[1:]); got != oss+rule {
		t.Fatal("规则不带 ? 也行:", got)
	}
	for _, u := range []string{"https://cdn.example.com/a.webp", oss + "?x=1", ""} {
		if got := thumbURL(u, rule); got != u {
			t.Errorf("非 OSS 地址 / 已带参数 / 空 → 退回原图: %q -> %q", u, got)
		}
	}
	if got := thumbURL(oss, ""); got != oss {
		t.Fatal("规则缺失退回原图")
	}
}

func TestPickSamplesOrderAndCap(t *testing.T) {
	mk := func(id int64, scope string, ind int64, sort int) model.SampleAsset {
		a := model.SampleAsset{ID: id, ScopeType: scope, Kind: "image", URL: "u", Title: "t", SortOrder: sort}
		if scope == model.SampleScopeIndustry {
			a.IndustryID = sql.NullInt64{Int64: ind, Valid: true}
		}
		return a
	}
	rows := []model.SampleAsset{
		mk(1, model.SampleScopeIndustry, 1, 0), // 一级
		mk(2, model.SampleScopeIndustry, 2, 5), // 所选二级
		mk(3, model.SampleScopeTemplate, 0, 9), // 模板级
		mk(4, model.SampleScopeIndustry, 2, 1),
	}
	got := pickSamples(rows, 2)
	order := []int64{}
	for _, g := range got {
		order = append(order, g.ID)
	}
	if !eq(order, []int64{3, 4, 2, 1}) {
		t.Fatalf("模板级 → 所选行业 → 其一级: %v", order)
	}
	var many []model.SampleAsset
	for i := int64(1); i <= 10; i++ {
		many = append(many, mk(i, model.SampleScopeIndustry, 2, int(i)))
	}
	if got := pickSamples(many, 2); len(got) != MaxSamples {
		t.Fatalf("最多 %d 条: %d", MaxSamples, len(got))
	}
	if pickSamples(nil, 2) == nil {
		t.Fatal("空结果是 [] 而不是 null")
	}
}

// H1：写权属还在 Java（AdminWriteEnabled=false）时用户端额外要求 enabled=1；接管后只看 state + 窗口。
func TestVisibilityRequiresEnabledUntilGoOwnsWrites(t *testing.T) {
	src := stdFixture()
	src.tpls[1].Enabled = 0 // 102：Java 已停用，但 state 仍是 PUBLISHED（Java 不维护 state）
	sc := func(c *Catalog) *Scope {
		s, err := c.Resolve("foot_bath", "")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	javaOwned, _ := BuildCatalog(ctx0, src, t0, true)
	if got := idsOf(javaOwned.List(sc(javaOwned), ListQuery{Page: 1, Size: 50}, t0).Recs); containsID(got, 102) {
		t.Fatalf("Java 写权属期间 enabled=0 的模板不应可见: %v", got)
	}
	if _, err := javaOwned.Get(102, t0); codeOf(err) != xerr.NotFound {
		t.Fatalf("按 id 取 enabled=0 的模板应为不存在: %v", err)
	}
	goOwned, _ := BuildCatalog(ctx0, src, t0, false)
	if got := idsOf(goOwned.List(sc(goOwned), ListQuery{Page: 1, Size: 50}, t0).Recs); !containsID(got, 102) {
		t.Fatalf("Go 接管后按 state+窗口判断，102 应可见: %v", got)
	}
	// 服务层按配置接线
	s := newTestService(src)
	if _, err := s.TemplateByID(ctx0, 102); codeOf(err) != xerr.NotFound {
		t.Fatalf("AdminWriteEnabled=false 时服务层应要求 enabled=1: %v", err)
	}
	cfg := s.cfg
	cfg.AdminWriteEnabled = true
	s2 := NewWithSource(src, cfg, func() time.Time { return t0 })
	if _, err := s2.TemplateByID(ctx0, 102); err != nil {
		t.Fatalf("AdminWriteEnabled=true 时 102 应可见: %v", err)
	}
}

// M2：按 id 取模板要求至少一个有效行业关系。
func TestGetRequiresEffectiveIndustry(t *testing.T) {
	src := stdFixture()
	src.tpls = append(src.tpls, tpl(106, "F", "image", t0.Add(-time.Hour)), tpl(107, "G", "image", t0.Add(-time.Hour)))
	src.rels = append(src.rels, rel(107, 5, 0)) // 106 无关系；107 只挂在停用行业 dead
	c := catalogOf(t, src)
	for _, id := range []int64{106, 107} {
		if _, err := c.Get(id, t0); codeOf(err) != xerr.NotFound {
			t.Errorf("模板 %d 没有有效行业关系，应按不存在: %v", id, err)
		}
	}
	if _, err := c.Get(105, t0); err != nil { // 105 挂在停用的 creator 与 dead_sub（父级停用）——都无效
		if codeOf(err) != xerr.NotFound {
			t.Fatal(err)
		}
	} else {
		t.Error("105 只挂在无效行业下，应按不存在")
	}
	if _, err := c.Get(101, t0); err != nil {
		t.Fatalf("101 挂在有效行业下应可取: %v", err)
	}
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
