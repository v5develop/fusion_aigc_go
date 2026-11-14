package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

func prefService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	cfg := Config{}
	cfg.Industry.Enabled = true
	s := New(bun.NewDB(sdb, mysqldialect.New()), nil, cfg)
	s.now = func() time.Time { return t0 }
	s.src = stdFixture() // 目录用内存假数据，偏好表走 sqlmock
	return s, mock
}

func TestPutPreferenceValidation(t *testing.T) {
	s, _ := prefService(t)
	cases := []struct {
		name string
		ids  []int64
		prim int64
		code int
		rsn  string
	}{
		{"空", nil, 0, xerr.ParamError, ""},
		{"超过 3 个", []int64{1, 2, 4, 7}, 0, xerr.ParamError, ""},
		{"primary 不在其中", []int64{1}, 4, xerr.ParamError, ""},
		{"行业不存在", []int64{999}, 0, xerr.NotFound, ReasonIndustryNotFound},
		{"停用行业", []int64{5}, 0, xerr.NotFound, ReasonIndustryNotFound},
		{"停用二级", []int64{3}, 0, xerr.NotFound, ReasonIndustryNotFound},
	}
	for _, c := range cases {
		_, err := s.PutPreference(ctx0, 7, c.ids, c.prim)
		if codeOf(err) != c.code || (c.rsn != "" && reasonOf(err) != c.rsn) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestPutPreferenceReplacesAndAllowsSub(t *testing.T) {
	s, mock := prefService(t)
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM `afv_user_industry_pref`").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO `afv_user_industry_pref`").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("FROM `afv_user_industry_pref`").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "industry_id", "is_primary", "sort_order"}).AddRow(1, 7, 2, 1, 0))
	// 允许传二级 id（足浴·蓝V号）
	got, err := s.PutPreference(ctx0, 7, []int64{2}, 0)
	if err != nil || got.Stale || got.Primary == nil || got.Primary.Code != "foot_bath_bluev" || got.Primary.ParentCode != "foot_bath" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPutPreferenceRetriesOnDuplicate(t *testing.T) {
	s, mock := prefService(t)
	dup := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM `afv_user_industry_pref`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO `afv_user_industry_pref`").WillReturnError(dup)
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM `afv_user_industry_pref`").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO `afv_user_industry_pref`").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("FROM `afv_user_industry_pref`").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "industry_id", "is_primary", "sort_order"}).AddRow(1, 7, 1, 1, 0))
	if _, err := s.PutPreference(ctx0, 7, []int64{1}, 1); err != nil {
		t.Fatalf("并发撞唯一键应重试而不是 500: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetPreferenceStaleAndEmpty(t *testing.T) {
	s, mock := prefService(t)
	cols := []string{"id", "user_id", "industry_id", "is_primary", "sort_order"}
	mock.ExpectQuery("FROM `afv_user_industry_pref`").WillReturnRows(sqlmock.NewRows(cols))
	got, err := s.GetPreference(ctx0, 7)
	if err != nil || got.IndustryIDs == nil || len(got.IndustryIDs) != 0 || got.Primary != nil || got.Stale {
		t.Fatalf("无偏好: %+v %v", got, err)
	}
	// 偏好指向已停用行业：stale=true、primary=null、不静默改写
	s, mock = prefService(t)
	mock.ExpectQuery("FROM `afv_user_industry_pref`").WillReturnRows(sqlmock.NewRows(cols).AddRow(1, 7, 5, 1, 0))
	got, err = s.GetPreference(ctx0, 7)
	if err != nil || !got.Stale || got.Primary != nil || got.PrimaryIndustryID != nil {
		t.Fatalf("stale: %+v %v", got, err)
	}
}

// ---------------------------------------------------------------- 报价

type fakeQuoter struct {
	calls []QuoteReq
	unit  int64
	err   error
}

func (f *fakeQuoter) ImageQuote(_ context.Context, r QuoteReq) (*JavaQuote, error) {
	f.calls = append(f.calls, r)
	if f.err != nil {
		return nil, f.err
	}
	n := int64(r.Num)
	if n < 1 {
		n = 1
	}
	q := &JavaQuote{UnitPoints: f.unit, ImagePoints: f.unit * n}
	if r.IncludeTurn {
		turn, bal, ok, free := int64(10), int64(500), true, false
		q.TurnPoints, q.Balance, q.Sufficient, q.FreeQuotaApplied = &turn, &bal, &ok, &free
		tot := q.ImagePoints + turn
		q.TotalPoints = &tot
	}
	return q, nil
}

func quoteService(q QuoteSource) (*Service, *time.Time) {
	now := t0
	s := newTestService(stdFixture())
	s.now = func() time.Time { return now }
	s.SetQuoteSource(q)
	return s, &now
}

func TestQuoteGuards(t *testing.T) {
	s, _ := quoteService(&fakeQuoter{unit: 40})
	if _, err := s.Quote(ctx0, QuoteParams{TemplateID: 104}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("缺 industry → 400: %v", err)
	}
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "spa"}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("缺 templateId: %v", err)
	}
	// 模板不属于该行业 / 不可见
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 101}); codeOf(err) != xerr.NotFound {
		t.Fatalf("101 属于足浴，不能在 spa 下报价: %v", err)
	}
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "ALL", TemplateID: 104}); reasonOf(err) != ReasonIndustryNotFound {
		t.Fatalf("ALL: %v", err)
	}
}

func TestQuoteAnonymousListPriceCachedAndVariants(t *testing.T) {
	fq := &fakeQuoter{unit: 40}
	s, now := quoteService(fq)
	r, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, Variants: 3, IPHash: "ip"})
	if err != nil {
		t.Fatal(err)
	}
	if r.UnitPoints != 40 || r.ImagePoints != 120 || r.Variants != 3 || !r.ListPrice || r.Personalized || r.TurnFeeIncluded || r.TurnPoints != nil || r.Stale || r.ReferenceCount != 1 {
		t.Fatalf("匿名标价不含轮次费: %+v", r)
	}
	if len(fq.calls) != 1 || fq.calls[0].IncludeTurn || fq.calls[0].Bearer != "" || fq.calls[0].Num != 1 {
		t.Fatalf("匿名走探针、不含轮次费、按单价取: %+v", fq.calls)
	}
	// 5 分钟内命中缓存，不再打 Java
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, Variants: 1, IPHash: "ip"}); err != nil || len(fq.calls) != 1 {
		t.Fatalf("应命中缓存: %v calls=%d", err, len(fq.calls))
	}
	// variants 夹到 [1,4]
	if r, _ = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, Variants: 99, IPHash: "ip"}); r.Variants != 4 || r.ImagePoints != 160 {
		t.Fatalf("variants 上限 4: %+v", r)
	}
	if r, _ = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, Variants: -2, IPHash: "ip"}); r.Variants != 1 {
		t.Fatalf("variants 下限 1: %+v", r)
	}
	// 过期后重取
	*now = now.Add(6 * time.Minute)
	fq.unit = 50
	if r, _ = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"}); r.UnitPoints != 50 || len(fq.calls) != 2 {
		t.Fatalf("过期重取: %+v", r)
	}
}

func TestQuoteStaleThenUnavailableNeverInvents(t *testing.T) {
	fq := &fakeQuoter{unit: 40}
	s, now := quoteService(fq)
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"}); err != nil {
		t.Fatal(err)
	}
	fq.err = errors.New("java down")
	*now = now.Add(10 * time.Minute) // 缓存已过期，Java 不可用：沿用最近一次成功值并标 stale
	r, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"})
	if err != nil || !r.Stale || r.UnitPoints != 40 {
		t.Fatalf("应返回 stale 标价: %+v %v", r, err)
	}
	*now = now.Add(2 * time.Hour) // 再久：不编造价格
	_, err = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"})
	if reasonOf(err) != ReasonQuoteUnavailable || codeOf(err) != xerr.ThirdPartyError {
		t.Fatalf("超过 1 小时应 QUOTE_UNAVAILABLE: %v", err)
	}
	// 没配报价源：同样不编造
	s2, _ := quoteService(nil)
	s2.quoter = nil
	if _, err := s2.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"}); reasonOf(err) != ReasonQuoteUnavailable {
		t.Fatalf("没有报价源: %v", err)
	}
}

func TestQuotePersonalizedForwardsBearerAndFallsBack(t *testing.T) {
	fq := &fakeQuoter{unit: 40}
	s, _ := quoteService(fq)
	r, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, Variants: 2, IPHash: "ip", Bearer: "Bearer tok"})
	if err != nil || !r.Personalized || r.ListPrice || !r.TurnFeeIncluded || r.TurnPoints == nil || *r.TurnPoints != 10 || r.Balance == nil {
		t.Fatalf("个性化报价: %+v %v", r, err)
	}
	if fq.calls[0].Bearer != "Bearer tok" || !fq.calls[0].IncludeTurn {
		t.Fatalf("应转发用户令牌并含轮次费: %+v", fq.calls[0])
	}
	// 个性化失败 → 退回匿名标价
	flaky := &flakyQuoter{inner: &fakeQuoter{unit: 40}}
	s, _ = quoteService(flaky)
	r, err = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", Bearer: "Bearer bad"})
	if err != nil || r.Personalized || !r.ListPrice {
		t.Fatalf("应退回匿名标价: %+v %v", r, err)
	}
}

type flakyQuoter struct{ inner *fakeQuoter }

func (f *flakyQuoter) ImageQuote(ctx context.Context, r QuoteReq) (*JavaQuote, error) {
	if r.Bearer != "" {
		return nil, errors.New("401")
	}
	return f.inner.ImageQuote(ctx, r)
}

func TestQuoteRateLimit(t *testing.T) {
	cfg := Config{}
	cfg.Industry.Enabled = true
	cfg.Quote.AnonPerMinute = 2
	s := NewWithSource(stdFixture(), cfg, func() time.Time { return t0 })
	s.SetQuoteSource(&fakeQuoter{unit: 40})
	p := QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "same-ip"}
	for i := 0; i < 2; i++ {
		if _, err := s.Quote(ctx0, p); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Quote(ctx0, p)
	if reasonOf(err) != ReasonTooFrequent {
		t.Fatalf("超限应限流: %v", err)
	}
	p.IPHash = "other-ip" // 别的 IP 不受影响
	if _, err := s.Quote(ctx0, p); err != nil {
		t.Fatal(err)
	}
}

func TestEtaFromSamples(t *testing.T) {
	if etaFromSamples(make([]int64, 29)) != nil {
		t.Fatal("样本 < 30 不给预计耗时")
	}
	ms := make([]int64, 0, 100)
	for i := 1; i <= 100; i++ {
		ms = append(ms, int64(i)*1000)
	}
	e := etaFromSamples(ms)
	if e == nil || e.Samples != 100 || e.P50 != 50 || e.P80 != 80 {
		t.Fatalf("%+v", e)
	}
	// 毫秒向上取整到秒
	ms = make([]int64, 30)
	for i := range ms {
		ms[i] = 1500
	}
	if e := etaFromSamples(ms); e.P50 != 2 || e.P80 != 2 {
		t.Fatalf("%+v", e)
	}
}

// ---------------------------------------------------------------- 搜索缺口

func missService(t *testing.T) (*Service, *miniredis.Miniredis, sqlmock.Sqlmock) {
	t.Helper()
	mr := miniredis.RunT(t)
	rd := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rd.Close() })
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	cfg := Config{}
	cfg.Industry.Enabled = true
	s := New(bun.NewDB(sdb, mysqldialect.New()), rd, cfg)
	s.now = func() time.Time { return t0 }
	s.src = stdFixture()
	return s, mr, mock
}

func TestSearchMissOnlyFirstPageZeroResults(t *testing.T) {
	s, mr, _ := missService(t)
	// 有结果：不计
	if _, err := s.Templates(ctx0, ListParams{Industry: "spa", Keyword: "标签-D", Page: 1, Size: 10, IPHash: "ip"}); err != nil {
		t.Fatal(err)
	}
	// 第 2 页 0 结果：不计
	if _, err := s.Templates(ctx0, ListParams{Industry: "spa", Keyword: "不存在", Page: 2, Size: 10, IPHash: "ip"}); err != nil {
		t.Fatal(err)
	}
	// 无关键词 0 结果：不计
	if _, err := s.Templates(ctx0, ListParams{Industry: "empty", Page: 1, Size: 10, IPHash: "ip"}); err != nil {
		t.Fatal(err)
	}
	for _, k := range mr.Keys() {
		t.Fatalf("不应有任何缺口计数: %s", k)
	}
	// 首页 0 结果：计一次；同一 IP 同一天同词重复搜只计一次；换 IP 再计
	for i := 0; i < 3; i++ {
		if _, err := s.Templates(ctx0, ListParams{Industry: "spa", Keyword: "  不存在  ", Page: 1, Size: 10, IPHash: "ip1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Templates(ctx0, ListParams{Industry: "spa", Keyword: "不存在", Page: 1, Size: 10, IPHash: "ip2"}); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet("aigc:plaza:miss:20261113:spa", "不存在"); got != "2" {
		t.Fatalf("同 IP 同词同日只计 1，换 IP 再计：%q", got)
	}
	// 疑似手机号 / 纯数字 不计
	for _, kw := range []string{"13800138000", "123456", "a@b.com"} {
		if _, err := s.Templates(ctx0, ListParams{Industry: "spa", Keyword: kw, Page: 1, Size: 10, IPHash: "ip9"}); err != nil {
			t.Fatal(err)
		}
	}
	fields, _ := mr.HKeys("aigc:plaza:miss:20261113:spa")
	if len(fields) != 1 {
		t.Fatalf("个人信息 / 纯数字不应入计数: %v", fields)
	}
	// Redis 里没有任何用户标识（去重 key 带 IP 哈希，但缺口 hash 里只有词与计数）
	for _, f := range fields {
		if f != "不存在" {
			t.Fatalf("缺口 hash 里只应有词: %s", f)
		}
	}
}

func TestFlushSearchMissesUpsertsAndClears(t *testing.T) {
	s, mr, mock := missService(t)
	mr.HIncrBy("aigc:plaza:miss:20261113:spa", "按摩", 3)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO afv_plaza_search_miss_daily .* VALUES \('2026-11-13', 'spa', '按摩', 3\)`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO afv_plaza_search_miss_term .* VALUES \('spa', '按摩', 3, '2026-11-13', '2026-11-13', 'NEW'\)`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	n, err := s.FlushSearchMisses(ctx0)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	for _, k := range mr.Keys() {
		t.Fatalf("入库后 Redis 增量应清掉: %s", k)
	}
}

// L14：日表与汇总表同一事务；汇总表失败整体回滚并把增量放回原 key（带 TTL）。
func TestFlushSearchMissesRollbackAndPutBackWithTTL(t *testing.T) {
	s, mr, mock := missService(t)
	mr.HIncrBy("aigc:plaza:miss:20261113:spa", "按摩", 3)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO afv_plaza_search_miss_daily`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO afv_plaza_search_miss_term`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()
	n, err := s.FlushSearchMisses(ctx0)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	k := "aigc:plaza:miss:20261113:spa"
	if v := mr.HGet(k, "按摩"); v != "3" {
		t.Fatalf("增量应放回原 key: %q", v)
	}
	if ttl := mr.TTL(k); ttl <= 0 {
		t.Fatalf("放回的 key 必须带 TTL: %v", ttl)
	}
	if mr.Exists("aigc:plaza:miss:proc:20261113:spa") {
		t.Fatal("处理中 key 应已删除")
	}
}

// L14：上一轮遗留的处理中 key 本轮直接接着入库。
func TestFlushSearchMissesResumesLeftoverProcKey(t *testing.T) {
	s, mr, mock := missService(t)
	mr.HIncrBy("aigc:plaza:miss:proc:20261113:spa", "按摩", 2)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO afv_plaza_search_miss_daily .* VALUES \('2026-11-13', 'spa', '按摩', 2\)`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO afv_plaza_search_miss_term`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if n, err := s.FlushSearchMisses(ctx0); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(mr.Keys()) != 0 {
		t.Fatalf("应清空: %v", mr.Keys())
	}
}

// L15：删除行业后清相关用户的偏好缓存。
func TestEvictPrefsAfterIndustryDelete(t *testing.T) {
	s, mr, _ := missService(t)
	_ = mr.Set(prefKey(1), "x")
	_ = mr.Set(prefKey(2), "x")
	_ = mr.Set(prefKey(3), "x")
	s.evictPrefs(ctx0, []int64{1, 2})
	if mr.Exists(prefKey(1)) || mr.Exists(prefKey(2)) || !mr.Exists(prefKey(3)) {
		t.Fatalf("只清相关用户: %v", mr.Keys())
	}
	s.evictPrefs(ctx0, nil) // 无用户不报错
}

// ---------------------------------------------------------------- 视频不报价 / 参考图张数 / 档位

func TestQuoteVideoTemplateIsUnavailableWithClearMessage(t *testing.T) {
	fq := &fakeQuoter{unit: 40}
	s, _ := quoteService(fq)
	_, err := s.Quote(ctx0, QuoteParams{Industry: "foot_bath_bluev", TemplateID: 103, IPHash: "ip", Bearer: "Bearer tok"})
	if reasonOf(err) != ReasonQuoteUnavailable || err == nil || !strings.Contains(err.Error(), "视频模板在生成视频页报价") {
		t.Fatalf("视频模板应 QUOTE_UNAVAILABLE 且文案明确: %v", err)
	}
	if len(fq.calls) != 0 {
		t.Fatalf("视频不应打 Java: %+v", fq.calls)
	}
}

func TestQuoteRefImageCountFromRequestAndFallback(t *testing.T) {
	fq := &fakeQuoter{unit: 40}
	s, _ := quoteService(fq)
	three := 3
	r, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", Bearer: "Bearer tok", RefImageCount: &three})
	if err != nil || r.ReferenceCount != 3 || fq.calls[0].ReferenceCount != 3 {
		t.Fatalf("个性化按请求里的张数: %+v %v %+v", r, err, fq.calls)
	}
	huge := 999
	if r, _ = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", RefImageCount: &huge}); r.ReferenceCount != 50 {
		t.Fatalf("张数夹到 50: %+v", r)
	}
	zero := 0
	if r, _ = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", RefImageCount: &zero}); r.ReferenceCount != 0 {
		t.Fatalf("显式 0 张不回退: %+v", r)
	}
	// 缺省：退回 NeedsProductImage 口径（104 需要产品图 → 1）
	if r, _ = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"}); r.ReferenceCount != 1 {
		t.Fatalf("缺省退回 needsProductImage: %+v", r)
	}
	// 匿名缓存按 有无参考图 归一：3 张与 1 张共用一个单价，不按张数放大
	fq2 := &fakeQuoter{unit: 40}
	s2, _ := quoteService(fq2)
	for _, n := range []int{1, 3, 5} {
		n := n
		if _, err := s2.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", RefImageCount: &n}); err != nil {
			t.Fatal(err)
		}
	}
	if len(fq2.calls) != 1 || fq2.calls[0].ReferenceCount != 1 {
		t.Fatalf("匿名取价应只打一次且按 1 归一: %+v", fq2.calls)
	}
}

func TestQuotePassesTurnPriceUnavailable(t *testing.T) {
	fq := &turnUnavailQuoter{}
	s, _ := quoteService(fq)
	r, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", Bearer: "Bearer tok"})
	if err != nil || r.TurnPriceUnavailable == nil || !*r.TurnPriceUnavailable {
		t.Fatalf("应透传 turnPriceUnavailable: %+v %v", r, err)
	}
}

type turnUnavailQuoter struct{}

func (turnUnavailQuoter) ImageQuote(_ context.Context, r QuoteReq) (*JavaQuote, error) {
	yes, zero := true, int64(0)
	return &JavaQuote{UnitPoints: 40, ImagePoints: 40, TurnPoints: &zero, TurnPriceUnavailable: &yes}, nil
}

func TestQuoteImageReportsMaxVariantsFour(t *testing.T) {
	s, _ := quoteService(&fakeQuoter{unit: 40})
	r, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"})
	if err != nil || r.MediaType != "image" || r.MaxVariants != 4 {
		t.Fatalf("图片 maxVariants=4: %+v %v", r, err)
	}
}

func TestQuoteResolutionSelectionAndValidation(t *testing.T) {
	fq := &fakeQuoter{unit: 40}
	s, _ := quoteService(fq)
	r, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", Resolution: " 2K "})
	if err != nil || r.Resolution != "2k" || fq.calls[0].Resolution != "2k" {
		t.Fatalf("按所选档位报价: %+v %v %+v", r, err, fq.calls)
	}
	// 匿名缓存键含档位：换档位要重新取价
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", Resolution: "4k"}); err != nil || len(fq.calls) != 2 || fq.calls[1].Resolution != "4k" {
		t.Fatalf("换档位应重取: %v %+v", err, fq.calls)
	}
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", Resolution: "2k"}); err != nil || len(fq.calls) != 2 {
		t.Fatalf("同档位应命中缓存: %v %d", err, len(fq.calls))
	}
	if r, _ = s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip"}); r.Resolution != "1k" {
		t.Fatalf("缺省 1k: %+v", r)
	}
	if _, err := s.Quote(ctx0, QuoteParams{Industry: "spa", TemplateID: 104, IPHash: "ip", Resolution: "8k"}); codeOf(err) != xerr.ParamError {
		t.Fatalf("非法档位 400: %v", err)
	}
}
