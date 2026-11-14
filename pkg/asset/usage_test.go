package asset

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 用途表：每个用途有中文名、用途标签唯一且能反查。
func TestUsageTable(t *testing.T) {
	seen := map[string]bool{}
	for _, u := range Usages {
		if !u.Valid() || u.Label() == "" {
			t.Fatalf("用途 %s 缺中文名", u)
		}
		tag := u.Tag()
		if !strings.HasPrefix(tag, UsageTagPrefix) || seen[tag] {
			t.Fatalf("用途标签不唯一或缺前缀: %s", tag)
		}
		seen[tag] = true
		if back, ok := UsageOfTag(tag); !ok || back != u {
			t.Fatalf("反查失败: %s → %s", tag, back)
		}
	}
	if len(usageLabels) != len(Usages) {
		t.Fatalf("usageLabels(%d) 与 Usages(%d) 不一致", len(usageLabels), len(Usages))
	}
	if Usage("person").Valid() || Usage("person").Tag() != "" {
		t.Fatal("person 已并入 model，不是用途")
	}
	for code, u := range EntryUsage {
		if !u.Valid() {
			t.Fatalf("入口 %s 映射到未知用途 %s", code, u)
		}
	}
}

// 写入方默认用途：入口表 → type 表 → 视频成片 → 其他。
func TestUsageFor(t *testing.T) {
	cases := []struct {
		entry, typ string
		want       Usage
	}{
		{"studio_tryon", "image", UsageModel},
		{"studio_imagegen", "image", UsageOther},
		{"canvas_video", "video", UsageVideoFinal},
		{"poster", "image", UsagePoster},
		{"productSetImages", "productSetImages", UsageProduct},
		{"", "customizedModel", UsageModel},
		{"unknownEntry", "video", UsageVideoFinal},
		{"unknownEntry", "hitVideoX", UsageVideoFinal},
		{"", "image", UsageOther},
	}
	for _, c := range cases {
		if got := UsageFor(c.entry, c.typ); got != c.want {
			t.Errorf("UsageFor(%q,%q)=%s want %s", c.entry, c.typ, got, c.want)
		}
	}
}

// 系统标签组装与规整：顺序、前缀、清晰度规整、去重、截断、上限。
func TestSysTagsList(t *testing.T) {
	got := SysTags{Feature: " 商拍换装 ", Usage: UsageModel, Industry: "餐饮", Template: "夏日上新", Resolution: "2k",
		Extra: []string{SegmentTag(3), TagPartial, "商拍换装", ""}}.List()
	want := []string{"商拍换装", "用途:模特", "行业:餐饮", "模板:夏日上新", "2K", "第3段", "部分完成"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
	for in, want := range map[string]string{"1080p": "1080P", "4K": "4K", "1024x1024": "", "high": "", "": ""} {
		if g := ResolutionTag(in); g != want {
			t.Errorf("ResolutionTag(%q)=%q want %q", in, g, want)
		}
	}
	if SegmentTag(0) != "" || IndustryTag(" ") != "" {
		t.Fatal("空值不应出标签")
	}
	long := strings.Repeat("长", 40)
	many := []string{long}
	for i := 0; i < 30; i++ {
		many = append(many, string(rune('a'+i)))
	}
	out := NormalizeSysTags(many)
	if len(out) != SysTagMaxSize || len([]rune(out[0])) != SysTagMaxLen {
		t.Fatalf("上限 / 截断不对: len=%d first=%d", len(out), len([]rune(out[0])))
	}
	if NormalizeSysTags(nil) == nil || len(NormalizeSysTags([]string{" ", ""})) != 0 {
		t.Fatal("空输入应得空切片")
	}
}

// 写入带 sys_tags：规整后的 JSON 数组；没有标签写 NULL。
func TestInsertWritesSysTags(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ins := regexp.QuoteMeta("INSERT INTO afv_asset (user_id, workspace_id, source_task_id, type, entry_code, name, cover_url, result_url, aspect_ratio, properties, ai_prompt, sys_tags, source_type, status)")
	mock.ExpectBegin()
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec(ins).WithArgs(int64(7), int64(3), "t1", "image", "studio_tryon", "商拍换装", "https://cdn/a", "https://cdn/a", "3:4",
		nil, nil, `["商拍换装","用途:模特","2K"]`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(ins).WithArgs(int64(7), int64(3), "t1", "image", "studio_tryon", "商拍换装", "https://cdn/b", "https://cdn/b", "3:4",
		nil, nil, nil).WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()
	w := NewWriter(sqlx.NewSqlConnFromDB(db))
	base := Asset{UserID: 7, SourceTaskID: "t1", Type: "image", EntryCode: "studio_tryon", Name: "商拍换装", AspectRatio: "3:4"}
	a, b := base, base
	a.CoverURL, a.ResultURL = "https://cdn/a", "https://cdn/a"
	a.SysTags = SysTags{Feature: "商拍换装", Usage: UsageFor("studio_tryon", "image"), Resolution: "2k"}.List()
	b.CoverURL, b.ResultURL = "https://cdn/b", "https://cdn/b"
	if err := w.InsertSet(context.Background(), []Asset{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 叠字版：无字版补「无字版」，叠字版沿用无字版标签再加「叠字版」（两份互不串）。
func TestSaveTextOverlayVariantSysTags(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, source_task_id, type, entry_code, name, aspect_ratio, ai_prompt, sys_tags")).
		WithArgs(int64(7), "https://cdn/plain", "https://cdn/plain").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_task_id", "type", "entry_code", "name", "aspect_ratio", "ai_prompt", "sys_tags", "result_url", "watermark_url"}).
			AddRow(11, "t1", "agentImage", "agentImage", "海报", "3:4", "p", `["智能体生图","用途:其他","无字版"]`, "https://cdn/plain", nil))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE afv_asset SET properties = JSON_MERGE_PATCH")).
		WithArgs(`{"textVariant":"plain"}`, `["智能体生图","用途:其他","无字版"]`, int64(11)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(1) FROM afv_asset WHERE user_id = ? AND result_url = ? AND deleted = 0")).
		WithArgs(int64(7), "https://cdn/ov").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO afv_asset")).
		WithArgs(int64(7), int64(3), "t1", "agentImage", "agentImage", "海报（叠字版）", "https://cdn/ov", "https://cdn/ov", "3:4",
			sqlmock.AnyArg(), "p", `["智能体生图","用途:其他","叠字版"]`).WillReturnResult(sqlmock.NewResult(12, 1))
	mock.ExpectCommit()
	w := NewWriter(sqlx.NewSqlConnFromDB(db))
	ok, err := w.SaveTextOverlayVariant(context.Background(), 7, "https://cdn/plain", "https://cdn/ov", "")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
