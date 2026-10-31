package logic

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
)

func TestCategoryItemsNullIsUncategorized(t *testing.T) {
	got := categoryItems([]repo.KindCount{{N: 5}, {Kind: sql.NullString{String: "model", Valid: true}, N: 2}, {Kind: sql.NullString{String: "hair", Valid: true}, N: 1}})
	want := map[string]int64{"all": 8, "model": 2, "scene": 0, "makeup": 0, "hair": 1, "accessory": 0, "uncategorized": 5}
	if len(got) != 7 || got[0].Key != "all" || got[6].Key != "uncategorized" {
		t.Fatalf("顺序不对：%v", got)
	}
	for _, c := range got {
		if c.Count != want[c.Key] || c.Label == "" {
			t.Errorf("%s: %d want %d", c.Key, c.Count, want[c.Key])
		}
	}
}

func TestToAsset(t *testing.T) {
	r := &repo.AssetRow{}
	r.ID, r.Name, r.CreateTime = 9, "a", time.UnixMilli(1730000000123)
	r.Type = sql.NullString{String: "hitVideo", Valid: true}
	r.ResultURL = sql.NullString{String: "https://x/a.mp4", Valid: true}
	r.CoverURL = sql.NullString{String: "https://x/a.jpg", Valid: true}
	r.SourceType = sql.NullInt64{Int64: 2, Valid: true}
	r.SourceTaskID = sql.NullString{String: "task1", Valid: true}
	r.ExtMetadata = []byte(`{"style":"日系"}`)
	r.IsVideo = true
	a := toAsset(r, nil)
	if a.Media != "video" || a.VideoUrl != "https://x/a.mp4" || a.Image != "https://x/a.jpg" || a.Source != "generated" ||
		a.CreatedAt != 1730000000123 || a.Metadata.Style != "日系" || a.Provenance.RunId != "task1" || a.Tags == nil || a.Kind != "" {
		t.Fatalf("%+v", a)
	}
	// 当前版本图优先；source_type 为空 = official
	r.Type, r.IsVideo = sql.NullString{String: "image", Valid: true}, false
	r.SourceType = sql.NullInt64{}
	r.CurrentImage = sql.NullString{String: "https://x/v2.png", Valid: true}
	a = toAsset(r, []string{"红"})
	if a.Media != "image" || a.Image != "https://x/v2.png" || a.Source != model.SourceOfficial || a.Tags[0] != "红" {
		t.Fatalf("%+v", a)
	}
}

func TestValidators(t *testing.T) {
	if _, err := cleanTagName(strings.Repeat("字", 21), 20); err == nil {
		t.Fatal("21 字应被拒")
	}
	if n, err := cleanTagName("  夏季 ", 20); err != nil || n != "夏季" {
		t.Fatalf("n=%q err=%v", n, err)
	}
	if code, _, _ := xerr.Parse(func() error { _, err := cleanTagName(" ", 20); return err }()); code != xerr.ParamMissing {
		t.Fatalf("空名应 1002，got %d", code)
	}
	for _, k := range []string{"", "uncategorized", "model", "accessory"} {
		if checkListKind(k) != nil {
			t.Errorf("kind %q 应合法", k)
		}
	}
	if checkListKind("shoes") == nil || checkSource("mine") == nil || checkMedia("audio") == nil {
		t.Fatal("非法枚举应被拒")
	}
	if checkURL("blob:abc") == nil || checkURL("https://x/a.png") != nil {
		t.Fatal("url 校验")
	}
	if _, err := checkJSON("params", "{bad"); err == nil {
		t.Fatal("非法 JSON 应被拒")
	}
	if l, o := subPage(2, 0); l != 6 || o != 6 {
		t.Fatalf("默认子分页 6：%d %d", l, o)
	}
}

func TestCheckOwnedURL(t *testing.T) {
	owns := func(u string) bool { return strings.HasPrefix(u, "https://cdn.example.com/") }
	if err := checkOwnedURL(owns, "image_url", " https://cdn.example.com/a.png "); err != nil {
		t.Fatalf("本服务地址应放行：%v", err)
	}
	err := checkOwnedURL(owns, "image_url", "https://evil.example.org/a.png")
	if code, msg, _ := xerr.Parse(err); code != xerr.ParamError || msg != "image_url 不是本服务上传的地址" {
		t.Fatalf("外链应被拒：code=%d msg=%q", code, msg)
	}
	if checkOwnedURL(nil, "image_url", "https://cdn.example.com/a.png") == nil {
		t.Fatal("owns 为空应一律拒绝")
	}
	if checkOwnedURL(owns, "image_url", "blob:abc") == nil {
		t.Fatal("非 http(s) 仍应拒")
	}
}
