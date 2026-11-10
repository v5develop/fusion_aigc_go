package billing

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

// 与 Java 迁移 V1.0.223 写入的参数逐字相同（Java ActualCostFreezeCapTest 用同一组向量；两侧各自钉死，不共享文件）。
const (
	canvasCapJSON = `{"currency":"USD","quality":{"low":"0.0687","medium":"0.1061","high":"0.2326"},"default_quality":"medium","edit_quality":"medium","base_pixels":1048576,"tier_max_pixels":{"1k":1048576,"2k":4300800,"4k":8294400},"per_ref":"0.056192","aux_per_image":"0"}`
)

var studioCapJSON = strings.Replace(canvasCapJSON, `"aux_per_image":"0"`, `"aux_per_image":"0.005"`, 1)

func mustCap(t *testing.T, s string) *FreezeCap {
	t.Helper()
	fc, ok := ParseFreezeCap(s)
	if !ok {
		t.Fatalf("parse %s", s)
	}
	return fc
}

func TestFreezeCapVectors(t *testing.T) {
	canvas, studio := mustCap(t, canvasCapJSON), mustCap(t, studioCapJSON)
	rate, markup := rat("7.2"), rat("1.3")
	cases := []struct {
		name   string
		fc     *FreezeCap
		res, q string
		refs   int
		want   int64
	}{
		{"画布 1k medium 文生图", canvas, "1k", "medium", 0, 100},
		{"画布 1k high 文生图", canvas, "1k", "high", 0, 218},
		{"画布 1k low", canvas, "1k", "low", 0, 65},
		{"画布 1k 14 张参考图 = 价目表", canvas, "1k", "medium", 14, 836},
		{"有参考图网关固定 medium", canvas, "1k", "high", 14, 836},
		{"画布 1k 1 张参考图", canvas, "1k", "medium", 1, 152},
		{"没给质量 → default", canvas, "1k", "", 0, 100},
		{"auto → default", canvas, "1k", "auto", 0, 100},
		{"大小写无关", canvas, "1K", "MEDIUM", 0, 100},
		{"画布 2k medium", canvas, "2k", "medium", 0, 408},
		{"画布 2k high", canvas, "2k", "high", 0, 893},
		{"画布 4k high = 价目表", canvas, "4k", "high", 0, 1723},
		{"画布 4k 3 张参考图", canvas, "4k", "medium", 3, 944},
		{"商拍 6 张参考图 + 辅助 = 价目表", studio, "1k", "high", 6, 420},
		{"姿态 2 张参考图 + 辅助", studio, "1k", "medium", 2, 210},
		{"姿态 2k", studio, "2k", "medium", 2, 518},
		{"图片生成文生图 + 辅助", studio, "1k", "medium", 0, 104},
		{"图片生成 2k 3 张", studio, "2k", "medium", 3, 570},
		{"4k low + 辅助", studio, "4k", "low", 0, 514},
	}
	for _, c := range cases {
		got, ok := c.fc.UnitCapPoints(c.res, ImageUsage{Quality: c.q, RefImages: c.refs}, rate, markup)
		if !ok || got != c.want {
			t.Errorf("%s: got %d ok=%v want %d", c.name, got, ok, c.want)
		}
	}
	if got, _ := canvas.UnitCapPoints("1k", ImageUsage{Quality: "medium"}, rate, rat("1.5")); got != 115 {
		t.Errorf("加成 1.5: %d", got)
	}
	if got, _ := canvas.UnitCapPoints("1k", ImageUsage{Quality: "medium"}, rat("7.0"), markup); got != 97 {
		t.Errorf("汇率 7.0: %d", got)
	}
}

// 参数 × 各载体价目表推导时的最坏组合 = V1.0.215 价目表（与 Java 的迁移钉子同口径）。
func TestFreezeCapWorstCaseIsTierPrice(t *testing.T) {
	rate, markup := rat("7.2"), rat("1.3")
	type carrier struct {
		json    string
		maxRefs int
		tiers   [3]int64
	}
	for name, c := range map[string]carrier{
		"canvas": {canvasCapJSON, 14, [3]int64{836, 1144, 1723}},
		"studio": {studioCapJSON, 6, [3]int64{420, 898, 1727}},
		"creaft": {studioCapJSON, 5, [3]int64{367, 898, 1727}},
	} {
		fc := mustCap(t, c.json)
		for i, res := range []string{"1k", "2k", "4k"} {
			hi, _ := fc.UnitCapPoints(res, ImageUsage{Quality: "high"}, rate, markup)
			ed, _ := fc.UnitCapPoints(res, ImageUsage{Quality: "medium", RefImages: c.maxRefs}, rate, markup)
			if w := max(hi, ed); w != c.tiers[i] {
				t.Errorf("%s %s 最坏 %d，价目表 %d", name, res, w, c.tiers[i])
			}
		}
	}
}

func TestFreezeCapNoCapCases(t *testing.T) {
	rate, markup := rat("7.2"), rat("1.3")
	fc := mustCap(t, canvasCapJSON)
	if _, ok := fc.UnitCapPoints("", ImageUsage{Quality: "medium"}, rate, markup); ok {
		t.Error("没有分辨率应不封顶")
	}
	if _, ok := fc.UnitCapPoints("8k", ImageUsage{}, rate, markup); ok {
		t.Error("未知档位应不封顶")
	}
	noDef := mustCap(t, strings.Replace(canvasCapJSON, `"default_quality":"medium",`, "", 1))
	if _, ok := noDef.UnitCapPoints("1k", ImageUsage{Quality: "xhigh"}, rate, markup); ok {
		t.Error("质量取不到应不封顶")
	}
	noEdit := mustCap(t, strings.Replace(canvasCapJSON, `"edit_quality":"medium",`, "", 1))
	if got, _ := noEdit.UnitCapPoints("1k", ImageUsage{Quality: "high", RefImages: 1}, rate, markup); got != 271 {
		t.Errorf("没配 edit_quality 按请求质量: %d", got)
	}
	for _, bad := range []string{"", "  ", "not json",
		strings.Replace(canvasCapJSON, `"USD"`, `"CNY"`, 1),
		strings.Replace(canvasCapJSON, `"per_ref":"0.056192"`, `"per_ref":"-1"`, 1),
		strings.Replace(canvasCapJSON, `"base_pixels":1048576`, `"base_pixels":0`, 1),
		`{"currency":"USD","quality":{},"base_pixels":1,"tier_max_pixels":{"1k":1}}`,
	} {
		if _, ok := ParseFreezeCap(bad); ok {
			t.Errorf("%q 应非法", bad)
		}
	}
	// 数字写法与字符串写法等价
	num := strings.NewReplacer(`"0.1061"`, `0.1061`, `"0.056192"`, `0.056192`).Replace(canvasCapJSON)
	if got, _ := mustCap(t, num).UnitCapPoints("1k", ImageUsage{Quality: "medium", RefImages: 14}, rate, markup); got != 836 {
		t.Errorf("数字写法: %d", got)
	}
}

// capQuote 只压载体上按张计价的步骤、只降不升，重算总价并记下两头。
func TestCapQuote(t *testing.T) {
	q := &Quote{CarrierModelID: 60, PricePoints: 1672 + 5, Lines: []QuoteLine{
		{ModelID: 60, Unit: unitPerImage, UnitPoints: 836, Quantity: 2, Multiplier: 1, Points: 1672},
		{ModelID: 9, Unit: unitPerImage, UnitPoints: 5, Quantity: 1, Multiplier: 1, Points: 5},
		{ModelID: 60, CostOnly: true},
	}}
	capQuote(q, 100)
	if q.PricePoints != 205 || q.Lines[0].UnitPoints != 100 || q.Lines[0].Points != 200 || q.Lines[1].Points != 5 {
		t.Fatalf("%+v", q)
	}
	if q.TierUnitPoints != 836 || q.UnitPointsCap != 100 {
		t.Fatalf("tier=%d cap=%d", q.TierUnitPoints, q.UnitPointsCap)
	}
	if !strings.Contains(q.Snapshot(), `"unitPointsCap":100`) || !strings.Contains(q.Snapshot(), `"tierUnitPoints":836`) {
		t.Fatal(q.Snapshot())
	}
	up := &Quote{CarrierModelID: 60, PricePoints: 836, Lines: []QuoteLine{{ModelID: 60, Unit: unitPerImage, UnitPoints: 836, Quantity: 1, Multiplier: 1, Points: 836}}}
	capQuote(up, 954)
	if up.PricePoints != 836 || up.UnitPointsCap != 0 || strings.Contains(up.Snapshot(), "unitPointsCap") {
		t.Fatalf("不能抬价: %+v", up)
	}
	sec := &Quote{CarrierModelID: 60, PricePoints: 40, Lines: []QuoteLine{{ModelID: 60, Unit: unitPerSecond, UnitPoints: 8, Quantity: 5, Multiplier: 1, Points: 40}}}
	capQuote(sec, 1)
	if sec.PricePoints != 40 {
		t.Fatalf("按秒不受影响: %+v", sec)
	}
}

var qFreezeCap = regexp.QuoteMeta("SELECT COALESCE(CAST(`freeze_cap_json` AS CHAR), '') FROM `afv_ai_model` WHERE `id` = ?")

func capQuoteFixture() *Quote {
	return &Quote{CarrierModelID: 60, PricePoints: 836, Resolution: "1K",
		Lines: []QuoteLine{{ModelID: 60, Unit: unitPerImage, UnitPoints: 836, Quantity: 1, Multiplier: 1, Points: 836}}}
}

func TestApplyFreezeCapActualCostCarrier(t *testing.T) {
	s, mock := onService(t)
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	mock.ExpectQuery(qFreezeCap).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"freeze_cap_json"}).AddRow(canvasCapJSON))
	mock.ExpectQuery(qKnob).WithArgs(KeyUSDCNYRate).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	mock.ExpectQuery(qKnob).WithArgs(KeyDefaultMarkup).WillReturnRows(sqlmock.NewRows([]string{"v"}))
	q := capQuoteFixture()
	s.applyFreezeCap(context.Background(), q, Charge{Usage: &ImageUsage{Quality: "medium"}})
	if q.PricePoints != 100 {
		t.Fatalf("1k medium 文生图应冻结 100: %d", q.PricePoints)
	}
	// 第二次走缓存（载体 / 参数 / 运行时参数都不再查库）
	q2 := capQuoteFixture()
	s.applyFreezeCap(context.Background(), q2, Charge{Usage: &ImageUsage{Quality: "high"}})
	if q2.PricePoints != 218 {
		t.Fatalf("1k high 文生图应冻结 218: %d", q2.PricePoints)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyFreezeCapSkips(t *testing.T) {
	// 没给 Usage：一条 SQL 都不发
	s, mock := onService(t)
	q := capQuoteFixture()
	s.applyFreezeCap(context.Background(), q, Charge{})
	if q.PricePoints != 836 {
		t.Fatal("nil Usage 应按价目表")
	}
	// FIXED 载体：不查参数
	mock.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("FIXED", nil))
	s.applyFreezeCap(context.Background(), q, Charge{Usage: &ImageUsage{}})
	if q.PricePoints != 836 {
		t.Fatal("FIXED 应按价目表")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// 列不存在（Java V1.0.223 未上）：按价目表，且 10 分钟内不再探
	s2, mock2 := onService(t)
	mock2.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	mock2.ExpectQuery(qFreezeCap).WithArgs(60).WillReturnError(&mysql.MySQLError{Number: 1054, Message: "Unknown column 'freeze_cap_json'"})
	q3 := capQuoteFixture()
	s2.applyFreezeCap(context.Background(), q3, Charge{Usage: &ImageUsage{Quality: "medium"}})
	s2.applyFreezeCap(context.Background(), q3, Charge{Usage: &ImageUsage{Quality: "medium"}})
	if q3.PricePoints != 836 {
		t.Fatal("列不存在应按价目表")
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// NULL / 非法参数：按价目表
	s3, mock3 := onService(t)
	mock3.ExpectQuery(qCarrier).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"settle_mode", "cost_markup"}).AddRow("ACTUAL_COST", nil))
	mock3.ExpectQuery(qFreezeCap).WithArgs(60).WillReturnRows(sqlmock.NewRows([]string{"freeze_cap_json"}).AddRow(""))
	q4 := capQuoteFixture()
	s3.applyFreezeCap(context.Background(), q4, Charge{Usage: &ImageUsage{Quality: "medium"}})
	if q4.PricePoints != 836 {
		t.Fatal("NULL 参数应按价目表")
	}
	if err := mock3.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
