package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"
)

// ---- 替身 ----

type fakeTrending struct {
	rows      []TrendingRow
	inds      map[int64][]string // 用户 → 行业 codes
	primary   map[int64]string   // 用户 → 主行业
	scans     int
	err       error
	prefErr   error
	scanSince time.Time
}

func (f *fakeTrending) ConfirmedPlanChanges(_ context.Context, since time.Time, _ int) ([]TrendingRow, error) {
	f.scans++
	f.scanSince = since
	return f.rows, f.err
}
func (f *fakeTrending) UserIndustries(_ context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	for _, id := range ids {
		if c, ok := f.inds[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}
func (f *fakeTrending) UserIndustry(_ context.Context, uid int64) (string, error) {
	return f.primary[uid], f.prefErr
}

func ann(audio string, dims map[string]string) json.RawMessage {
	a := Annotations{Dims: map[string]DimEdit{}, Audio: audio}
	for k, m := range dims {
		a.Dims[k] = DimEdit{Mode: m, Note: "用户私有备注-不该出现", Refs: []int{1}}
	}
	b, _ := json.Marshal(a)
	return b
}

// rowsFor n 个不同用户（起始 uid0）各一张同样改法的方案。
func rowsFor(uid0 int64, n int, raw json.RawMessage) []TrendingRow {
	out := make([]TrendingRow, n)
	for i := range out {
		out[i] = TrendingRow{UserID: uid0 + int64(i), Annotations: raw}
	}
	return out
}

func newTrendSvc(t *testing.T, ft *fakeTrending) (*Service, *time.Time) {
	t.Helper()
	e := newEnv(t, false)
	clock := e.clock
	cfg := DefaultConfig()
	svc := New(cfg, e.store, e.ledger, WithClock(func() time.Time { return clock }), WithTrending(ft, nil))
	return svc, &clock
}

// ---- 聚合：分组 / k-匿名 / 排序 ----

func TestAggregateTrendingGroupsByDimSetAndAudio(t *testing.T) {
	modelKeep := ann("keep", map[string]string{"model": ModeReplace})
	modelOnly := ann("", map[string]string{"model": ModeReplace})
	// 同一组合、维度顺序与备注不同 = 同一组；声音不同 = 不同组；全 keep 且默认声音不算改法
	var rows []TrendingRow
	rows = append(rows, rowsFor(1, 25, modelKeep)...)
	rows = append(rows, rowsFor(100, 22, modelOnly)...)
	rows = append(rows, rowsFor(200, 30, ann("", map[string]string{"scene": ModeKeep}))...)
	rows = append(rows, rowsFor(300, 30, json.RawMessage(`not json`))...)
	g := aggregateTrending(rows, nil, 20, 5)
	if len(g) != 2 {
		t.Fatalf("应有 2 组（keep 声音 / 默认声音）：%d", len(g))
	}
	if g[0].key != "model|keep" || len(g[0].users) != 25 || g[1].key != "model|regenerate" {
		t.Fatalf("排序 / 分组不对：%s(%d) %s(%d)", g[0].key, len(g[0].users), g[1].key, len(g[1].users))
	}
}

func TestAggregateTrendingKAnonymityAndDistinctUsers(t *testing.T) {
	raw := ann("keep", map[string]string{"model": ModeReplace})
	// 19 个用户各做 5 条 = 95 条方案，但只有 19 个人：不够 20，不展示
	var rows []TrendingRow
	for i := 0; i < 5; i++ {
		rows = append(rows, rowsFor(1, 19, raw)...)
	}
	if g := aggregateTrending(rows, nil, 20, 5); len(g) != 0 {
		t.Fatalf("19 个去重用户不应展示：%+v", g)
	}
	// 第 20 个人加入：恰好达到阈值，展示
	rows = append(rows, rowsFor(500, 1, raw)...)
	g := aggregateTrending(rows, nil, 20, 5)
	if len(g) != 1 || len(g[0].users) != 20 || g[0].plans != 96 {
		t.Fatalf("20 个去重用户应展示：%+v", g)
	}
	// 阈值可调
	if g := aggregateTrending(rows, nil, 21, 5); len(g) != 0 {
		t.Fatal("阈值 21 时不应展示")
	}
}

func TestAggregateTrendingLimitAndTieBreak(t *testing.T) {
	var rows []TrendingRow
	keys := []string{"model", "garment", "accessory", "product", "motion", "expression", "line"}
	for i, k := range keys {
		rows = append(rows, rowsFor(int64(i*1000), 20+i, ann("", map[string]string{k: ModeReplace}))...)
	}
	g := aggregateTrending(rows, nil, 20, 5)
	if len(g) != 5 {
		t.Fatalf("最多 5 组：%d", len(g))
	}
	if g[0].key != "line|regenerate" { // 用户数最多的排第一
		t.Fatalf("按用户数降序：%s", g[0].key)
	}
	// 同用户数 → 按组合键升序（稳定）
	rows = append(rowsFor(1, 20, ann("", map[string]string{"scene": ModeReplace})), rowsFor(1000, 20, ann("", map[string]string{"light": ModeReplace}))...)
	g = aggregateTrending(rows, nil, 20, 5)
	if g[0].key != "light|regenerate" || g[1].key != "scene|regenerate" {
		t.Fatalf("同数应按键升序：%s %s", g[0].key, g[1].key)
	}
}

func TestTrendingLabelsAndModes(t *testing.T) {
	cases := []struct {
		dims  []TrendingDim
		audio string
		want  string
	}{
		{[]TrendingDim{{"model", ModeReplace}}, AudioKeep, "换模特 + 保留原声"},
		{[]TrendingDim{{"garment", ModeReplace}, {"scene", ModeDescribe}}, AudioRegenerate, "换服装 + 改场景"},
		{nil, AudioRevoice, "换声音"},
		{[]TrendingDim{{"product", ModeDescribe}}, AudioMute, "改商品 + 静音"},
	}
	for _, c := range cases {
		if got := trendingLabel(c.dims, c.audio); got != c.want {
			t.Errorf("label=%q want=%q", got, c.want)
		}
	}
	// 同一组合里多数用户 describe → 展示 describe；平手取 replace
	var rows []TrendingRow
	rows = append(rows, rowsFor(1, 12, ann("", map[string]string{"scene": ModeDescribe}))...)
	rows = append(rows, rowsFor(100, 10, ann("", map[string]string{"scene": ModeReplace}))...)
	items := trendingItems(aggregateTrending(rows, nil, 20, 5), nil)
	if len(items) != 1 || items[0].Dims[0].Mode != ModeDescribe || items[0].Label != "改场景" {
		t.Fatalf("多数 describe：%+v", items)
	}
	rows = append(rows, rowsFor(500, 2, ann("", map[string]string{"scene": ModeReplace}))...)
	items = trendingItems(aggregateTrending(rows, nil, 20, 5), nil)
	if items[0].Dims[0].Mode != ModeReplace {
		t.Fatalf("平手取 replace：%+v", items[0].Dims)
	}
}

// ---- 视图：不含用户内容、示例号、行业回落 ----

func TestTrendingViewNeverLeaksUserContent(t *testing.T) {
	ft := &fakeTrending{rows: rowsFor(1, 30, ann("keep", map[string]string{"model": ModeReplace}))}
	svc, _ := newTrendSvc(t, ft)
	v, err := svc.Trending(context.Background(), 7, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	for _, bad := range []string{"用户私有备注", "refs", "note", "users", "userId", "occurrences"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("视图不应含 %q：%s", bad, b)
		}
	}
	if v.Period != "7d" || len(v.Items) != 1 || v.Items[0].Label != "换模特 + 保留原声" || v.Items[0].Audio != AudioKeep || v.Items[0].Desc == "" {
		t.Fatalf("视图：%s", b)
	}
	if !ft.scanSince.Equal(svc.now().Add(-7 * 24 * time.Hour)) {
		t.Fatalf("统计窗口应是近 7 天：%v", ft.scanSince)
	}
}

func TestTrendingPicksSampleWithSameCombo(t *testing.T) {
	e := newEnv(t, false)
	ft := &fakeTrending{rows: rowsFor(1, 20, ann("keep", map[string]string{"model": ModeReplace}))}
	e.svc = New(DefaultConfig(), e.store, e.ledger, WithClock(func() time.Time { return e.clock }), WithTrending(ft, nil))
	l := e.withLib()
	aid := int64(9)
	mk := func(id int64, a json.RawMessage, analysis *int64) *model.Sample {
		return &model.Sample{ID: id, Title: "s", Enabled: true, Annotations: a, AnalysisID: analysis}
	}
	l.samples = []*model.Sample{
		mk(1, ann("keep", map[string]string{"model": ModeReplace}), nil),   // 同组合但分析没就绪：不能直接试
		mk(2, ann("mute", map[string]string{"model": ModeReplace}), &aid),  // 声音不同
		mk(3, ann("keep", map[string]string{"model": ModeDescribe}), &aid), // 同键集合 + 同声音 → 命中（组合不看 replace/describe）
		mk(4, ann("keep", map[string]string{"model": ModeReplace}), &aid),  // 也命中，但排后面
	}
	v, err := e.svc.Trending(e.ctx, 7, "")
	if err != nil || len(v.Items) != 1 || v.Items[0].SampleID != 3 {
		t.Fatalf("应带上排在最前、分析就绪、组合相同的示例 3：%+v err=%v", v, err)
	}
}

func TestTrendingIndustryParamAndPreference(t *testing.T) {
	food := rowsFor(1, 22, ann("keep", map[string]string{"model": ModeReplace}))
	shoes := rowsFor(1000, 25, ann("", map[string]string{"scene": ModeReplace}))
	ft := &fakeTrending{rows: append(append([]TrendingRow{}, food...), shoes...), inds: map[int64][]string{}, primary: map[int64]string{7: "food"}}
	for _, r := range food {
		ft.inds[r.UserID] = []string{"food"}
	}
	for _, r := range shoes {
		ft.inds[r.UserID] = []string{"shoes", "fashion"} // 二级行业的用户也算进一级
	}
	svc, _ := newTrendSvc(t, ft)
	ctx := context.Background()

	// 显式参数优先
	v, _ := svc.Trending(ctx, 7, "fashion")
	if v.Industry != "fashion" || v.Fallback || len(v.Items) != 1 || v.Items[0].Label != "改场景" && v.Items[0].Label != "换场景" {
		t.Fatalf("显式行业：%+v", v)
	}
	// 缺省取用户的行业偏好
	v, _ = svc.Trending(ctx, 7, "")
	if v.Industry != "food" || len(v.Items) != 1 || v.Items[0].Label != "换模特 + 保留原声" {
		t.Fatalf("行业偏好：%+v", v)
	}
	// 没有偏好 → 全站（两个组合都在）
	v, _ = svc.Trending(ctx, 8, "")
	if v.Industry != "" || v.Fallback || len(v.Items) != 2 {
		t.Fatalf("没有偏好应看全站：%+v", v)
	}
	// 读偏好出错 → 当作没有偏好
	ft.prefErr = errors.New("db down")
	v, err := svc.Trending(ctx, 7, "")
	if err != nil || v.Industry != "" || len(v.Items) != 2 {
		t.Fatalf("读偏好失败应回落全站：%+v err=%v", v, err)
	}
}

func TestTrendingIndustryTooSmallFallsBackToGlobal(t *testing.T) {
	big := rowsFor(1, 30, ann("keep", map[string]string{"model": ModeReplace}))
	small := rowsFor(1000, 5, ann("", map[string]string{"scene": ModeReplace})) // 美妆只有 5 个人
	ft := &fakeTrending{rows: append(append([]TrendingRow{}, big...), small...), inds: map[int64][]string{}}
	for _, r := range small {
		ft.inds[r.UserID] = []string{"beauty"}
	}
	svc, _ := newTrendSvc(t, ft)
	v, err := svc.Trending(context.Background(), 7, "beauty")
	if err != nil || !v.Fallback || v.Industry != "" || len(v.Items) != 1 || v.Items[0].Label != "换模特 + 保留原声" {
		t.Fatalf("行业凑不够 k 应回落全站并标 fallback：%+v err=%v", v, err)
	}
	// 全站也没有 → 空列表（不是 nil），不报错
	svc2, _ := newTrendSvc(t, &fakeTrending{})
	v, err = svc2.Trending(context.Background(), 7, "")
	if err != nil || v.Items == nil || len(v.Items) != 0 {
		t.Fatalf("没数据应是空列表：%+v err=%v", v, err)
	}
}

func TestTrendingMinUsersConfigurable(t *testing.T) {
	ft := &fakeTrending{rows: rowsFor(1, 10, ann("keep", map[string]string{"model": ModeReplace}))}
	e := newEnv(t, false)
	cfg := DefaultConfig()
	cfg.TrendingMinUsers = 10
	svc := New(cfg, e.store, e.ledger, WithClock(func() time.Time { return e.clock }), WithTrending(ft, nil))
	if v, _ := svc.Trending(e.ctx, 7, ""); len(v.Items) != 1 {
		t.Fatalf("阈值调成 10 后应展示：%+v", v)
	}
	if got := DefaultConfig().withDefaults().TrendingMinUsers; got != 20 {
		t.Fatalf("默认阈值应是 20：%d", got)
	}
}

// ---- 缓存：每小时一算、过期刷新、失败沿用旧值、跨 Service 共用 ----

func TestTrendingCacheHourlyAndStaleOnError(t *testing.T) {
	ft := &fakeTrending{rows: rowsFor(1, 20, ann("keep", map[string]string{"model": ModeReplace}))}
	svc, clock := newTrendSvc(t, ft)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := svc.Trending(ctx, 7, ""); err != nil {
			t.Fatal(err)
		}
	}
	if ft.scans != 1 {
		t.Fatalf("一小时内只算一次：%d", ft.scans)
	}
	*clock = clock.Add(61 * time.Minute)
	svc.now = func() time.Time { return *clock }
	ft.rows = rowsFor(1, 20, ann("mute", map[string]string{"scene": ModeReplace}))
	v, _ := svc.Trending(ctx, 7, "")
	if ft.scans != 2 || v.Items[0].Label != "换场景 + 静音" {
		t.Fatalf("过期后应重算：scans=%d %+v", ft.scans, v.Items)
	}
	// 过期后重算失败 → 沿用旧结果
	*clock = clock.Add(61 * time.Minute)
	svc.now = func() time.Time { return *clock }
	ft.err = errors.New("db down")
	v, err := svc.Trending(ctx, 7, "")
	if err != nil || len(v.Items) != 1 || v.Items[0].Label != "换场景 + 静音" {
		t.Fatalf("失败应沿用旧值：%+v err=%v", v, err)
	}
	// 从没算过又失败 → 报错
	svc2, _ := newTrendSvc(t, &fakeTrending{err: errors.New("down")})
	if _, err := svc2.Trending(ctx, 7, ""); err == nil {
		t.Fatal("没有旧值时应报错")
	}
}

func TestTickTrendingPrecomputesAndRefreshes(t *testing.T) {
	ft := &fakeTrending{rows: rowsFor(1, 20, ann("keep", map[string]string{"model": ModeReplace})), inds: map[int64][]string{}}
	for _, r := range ft.rows {
		ft.inds[r.UserID] = []string{"food"}
	}
	svc, clock := newTrendSvc(t, ft)
	ctx := context.Background()
	svc.TickTrending(ctx) // 预热全站
	if ft.scans != 1 {
		t.Fatalf("首轮应预热全站：%d", ft.scans)
	}
	svc.TickTrending(ctx)
	if ft.scans != 1 {
		t.Fatalf("没过期不重算：%d", ft.scans)
	}
	if _, err := svc.Trending(ctx, 1, "food"); err != nil { // 现算 food
		t.Fatal(err)
	}
	scans := ft.scans
	*clock = clock.Add(2 * time.Hour)
	svc.now = func() time.Time { return *clock }
	svc.TickTrending(ctx) // 全站 + food 都过期：各刷新一次
	if ft.scans != scans+2 {
		t.Fatalf("过期的行业都应刷新：%d → %d", scans, ft.scans)
	}
	// 缓存跨 Service 共用（运行参数重建 Service 不丢缓存）
	cache := svc.trend
	e := newEnv(t, false)
	svc2 := New(DefaultConfig(), e.store, e.ledger, WithClock(func() time.Time { return *clock }), WithTrending(ft, cache))
	before := ft.scans
	if _, err := svc2.Trending(ctx, 1, ""); err != nil || ft.scans != before {
		t.Fatalf("共用缓存不应重算：%d → %d err=%v", before, ft.scans, err)
	}
}

func TestTrendingNotReadyWithoutStore(t *testing.T) {
	e := newEnv(t, false)
	_, err := e.svc.Trending(e.ctx, 1, "")
	if code, _, _ := xerr.Parse(err); code != xerr.NotReady {
		t.Fatalf("没装配应回 7003：%v", err)
	}
	e.svc.TickTrending(e.ctx) // 不 panic
	svc, _ := newTrendSvc(t, &fakeTrending{})
	if _, err := svc.Trending(e.ctx, 1, strings.Repeat("x", 65)); err == nil {
		t.Fatal("行业 code 过长应拒绝")
	}
}
