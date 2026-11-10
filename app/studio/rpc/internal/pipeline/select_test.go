package pipeline

import (
	"fmt"
	"strings"
	"testing"
)

func mains(item string, n int) []Image {
	var out []Image
	for i := 0; i < n; i++ {
		out = append(out, Image{Key: fmt.Sprintf("g-%s-front-%d", item, i+1), Kind: ImageGarment, Item: item, SHA1: fmt.Sprintf("%s-m%d", item, i)})
	}
	return out
}

func details(item string, n int) []Image {
	var out []Image
	for i := 0; i < n; i++ {
		out = append(out, Image{Key: fmt.Sprintf("d-%s-%d", item, i+1), Kind: ImageDetail, Item: item, Label: ItemLabels[item] + "·x", SHA1: fmt.Sprintf("%s-d%d", item, i)})
	}
	return out
}

var model = &Image{Key: "m-1", Kind: ImageModel, SHA1: "model"}

func assertShape(t *testing.T, sel Selection) {
	t.Helper()
	if len(sel.Images) > MaxInputImages {
		t.Fatalf("D1: %d images", len(sel.Images))
	}
	// D2：主图 → 细节 → 模特（恰 1 张，最后）
	phase := 0
	models := 0
	for i, im := range sel.Images {
		p := map[string]int{ImageGarment: 0, ImageDetail: 1, ImageModel: 2}[im.Kind]
		if p < phase {
			t.Fatalf("D2 order broken at %d: %v", i, keys(sel.Images))
		}
		phase = p
		if im.Kind == ImageModel {
			models++
			if i != len(sel.Images)-1 {
				t.Fatal("model must be last")
			}
		}
	}
	if sel.Model && models != 1 {
		t.Fatalf("models = %d", models)
	}
	seen := map[string]bool{}
	for _, im := range sel.Images {
		if seen[im.SHA1] {
			t.Fatalf("D4 dup %s", im.SHA1)
		}
		seen[im.SHA1] = true
	}
}

func TestSelectSingleItem(t *testing.T) {
	sel := SelectImages("top", mains("top", 6), details("top", 10), model)
	assertShape(t, sel)
	if sel.Mains != 4 || sel.Details != 4 || len(sel.Images) != 9 {
		t.Fatalf("mains=%d details=%d", sel.Mains, sel.Details)
	}
	sel = SelectImages("top", mains("top", 1), details("top", 10), model)
	if sel.Mains != 1 || sel.Details != 7 {
		t.Fatalf("one main: mains=%d details=%d", sel.Mains, sel.Details)
	}
	sel = SelectImages("top", mains("top", 1), nil, model)
	if len(sel.Images) != 2 {
		t.Fatalf("no details: %v", keys(sel.Images))
	}
}

func TestSelectComposeEveryItemCovered(t *testing.T) {
	// 3 件 × 3 张主图 = 9：旧算法 garments[:6] 会丢掉第 3 件（D3）
	var m []Image
	m = append(m, mains("top", 3)...)
	m = append(m, mains("bottom", 3)...)
	m = append(m, mains("shoes", 3)...)
	var d []Image
	d = append(d, details("top", 5)...)
	d = append(d, details("bottom", 5)...)
	d = append(d, details("shoes", 5)...)
	sel := SelectImages(CategoryCompose, m, d, model)
	assertShape(t, sel)
	if sel.Mains != 6 || sel.Details != 2 || len(sel.Images) != 9 {
		t.Fatalf("mains=%d details=%d", sel.Mains, sel.Details)
	}
	perItem := map[string]int{}
	for _, im := range sel.Images[:sel.Mains] {
		perItem[im.Item]++
	}
	if perItem["top"] != 2 || perItem["bottom"] != 2 || perItem["shoes"] != 2 {
		t.Fatalf("D3 mains per item = %v (%v)", perItem, keys(sel.Images))
	}
	// 主图保持编号顺序
	if !strings.HasPrefix(sel.Images[0].Key, "g-top") || !strings.HasPrefix(sel.Images[5].Key, "g-shoes") {
		t.Fatalf("main order: %v", keys(sel.Images))
	}
}

func TestSelectDetailsPreferUncoveredItem(t *testing.T) {
	// 只有上衣有主图（6 张，compose），下装只有细节：细节名额 2，必须先给下装（D3）
	d := append(details("top", 4), details("bottom", 1)...)
	sel := SelectImages(CategoryCompose, mains("top", 6), d, model)
	assertShape(t, sel)
	got := keys(sel.Images[sel.Mains : sel.Mains+sel.Details])
	if len(got) != 2 || got[0] != "d-bottom-1" {
		t.Fatalf("details = %v", got)
	}
}

func TestSelectRoundRobinDetails(t *testing.T) {
	d := append(details("top", 5), details("bottom", 5)...)
	sel := SelectImages(CategoryCompose, append(mains("top", 1), mains("bottom", 1)...), d, model)
	assertShape(t, sel)
	got := keys(sel.Images[sel.Mains : sel.Mains+sel.Details])
	want := "d-top-1,d-bottom-1,d-top-2,d-bottom-2,d-top-3,d-bottom-3"
	if strings.Join(got, ",") != want {
		t.Fatalf("details = %v", got)
	}
}

func TestSelectDedupe(t *testing.T) {
	m := mains("top", 2)
	m[1].SHA1 = m[0].SHA1 // 同图两次
	d := details("top", 3)
	d[0].SHA1 = m[0].SHA1 // 细节与主图同内容
	d[2].SHA1 = "model"   // 与模特同内容
	sel := SelectImages("top", m, d, model)
	assertShape(t, sel)
	if sel.Mains != 1 || sel.Details != 1 || sel.Dropped != 3 {
		t.Fatalf("sel = %+v", sel)
	}
}

func TestGenerateOrder(t *testing.T) {
	sel := SelectImages("top", mains("top", 2), details("top", 1), model)
	got := keys(GenerateOrder(sel))
	if strings.Join(got, ",") != "m-1,g-top-front-1,g-top-front-2,d-top-1" {
		t.Fatalf("order = %v", got)
	}
	if keys(sel.Images)[0] != "g-top-front-1" {
		t.Fatal("GenerateOrder must not mutate selection")
	}
}

func poolSel(cat string, ms, ds []Image) Selection { return SelectImages(cat, ms, ds, model) }

func TestRefCapPerModel(t *testing.T) {
	cases := []struct {
		group, kind string
		catalog     int
		want        int
	}{
		{"img.gpt-image-2", KindTryon, 0, 14},  // 标称 16 → 14
		{"img.gpt-image-2", KindTryon, 16, 14}, // catalog 16 → 14
		{"img.nano-banana-2.1", KindTryon, 0, 14},
		{"img.seedream-5-pro", KindTryon, 0, 10},
		{"img.seedream-5-pro", KindTryon, 14, 14},       // catalog 说 14 以 catalog 为准
		{"img.gpt-image-2.6-sunburst", KindTryon, 0, 6}, // 计费硬上限
		{"img.qwen-image-edit", KindTryon, 0, 3},
	}
	for _, c := range cases {
		if got := RefCap(c.group, c.kind, c.catalog); got != c.want {
			t.Errorf("RefCap(%s,%d)=%d want %d", c.group, c.catalog, got, c.want)
		}
	}
}

func TestGenerateRefsCropBudget(t *testing.T) {
	// 上衣 6 张 + 12 张细节图，cap 14：主图 6、细节 14-6-1=7、模特第一位，合计 14
	sel := poolSel(ItemTop, mains(ItemTop, 6), details(ItemTop, 12))
	if len(sel.Images) > MaxInputImages {
		t.Fatalf("反推读图 %d > 9", len(sel.Images))
	}
	g := GenerateRefs(sel, *model, 14)
	if len(g.Images) != 14 || g.Mains != 6 || g.Details != 7 || g.Images[0].Kind != ImageModel {
		t.Fatalf("cap14: %d mains=%d details=%d", len(g.Images), g.Mains, g.Details)
	}
	if g.DroppedMains != 0 || g.DroppedDetails != 5 || len(g.Warnings()) != 1 || !strings.Contains(g.Warnings()[0], "细节图已减少为 7 张") {
		t.Fatalf("%+v %v", g, g.Warnings())
	}
	// Seedream cap 10：细节 3
	if g := GenerateRefs(sel, *model, 10); len(g.Images) != 10 || g.Details != 3 {
		t.Fatalf("cap10: %d %d", len(g.Images), g.Details)
	}
	// 主图占满：cap 7、主图 6 → 细节 0，提示已省略
	g = GenerateRefs(sel, *model, 7)
	if len(g.Images) != 7 || g.Details != 0 || !strings.Contains(strings.Join(g.Warnings(), ""), "细节图已省略") {
		t.Fatalf("cap7: %+v %v", g, g.Warnings())
	}
}

func TestGenerateRefsDropsMainsKeepsEveryItem(t *testing.T) {
	// 三件各 3 张共 9 张 + model，cap 6（GPT 2.6）：主图只留 5 张，每件至少 1 张；细节 0
	var ms []Image
	for _, it := range []string{ItemTop, ItemBottom, ItemOuter} {
		ms = append(ms, mains(it, 3)...)
	}
	sel := poolSel(CategoryCompose, ms, details(ItemTop, 4))
	g := GenerateRefs(sel, *model, 6)
	if len(g.Images) != 6 || g.Mains != 5 || g.DroppedMains != 4 {
		t.Fatalf("%+v", g)
	}
	items := map[string]bool{}
	for _, im := range g.Images[1:] {
		items[im.Item] = true
	}
	if len(items) != 3 {
		t.Fatalf("单品覆盖 %v", items)
	}
	if w := strings.Join(g.Warnings(), "|"); !strings.Contains(w, "已保留 5 张服装图") || strings.Contains(w, "gpt") {
		t.Fatalf("warnings %q", w)
	}
}

func TestGenerateRefsNeverExceedsCap(t *testing.T) {
	for cp := 2; cp <= 20; cp++ {
		for n := 1; n <= 9; n++ {
			sel := poolSel(ItemTop, mains(ItemTop, min(n, 6)), details(ItemTop, 20))
			g := GenerateRefs(sel, *model, cp)
			if len(g.Images) > min(cp, GlobalMaxRefImages) || len(g.Images) > 14 {
				t.Fatalf("cap %d n %d → %d", cp, n, len(g.Images))
			}
		}
	}
}

func TestSubmitRefWarnings(t *testing.T) {
	if w := SubmitRefWarnings(14, 1); len(w) != 0 {
		t.Fatalf("1 张不该预警：%v", w)
	}
	// Seedream（10）6 张服装图：细节名额 3，预估 7
	if w := SubmitRefWarnings(10, 6); len(w) != 1 || !strings.Contains(w[0], "细节图将减少为约 3 张") {
		t.Fatalf("%v", w)
	}
	// 9 张服装图 cap 10：主图全留，细节省略
	if w := SubmitRefWarnings(10, 9); len(w) != 1 || !strings.Contains(w[0], "细节图将省略") {
		t.Fatalf("%v", w)
	}
	// cap 3：服装图只留 2 张
	if w := SubmitRefWarnings(3, 4); len(w) != 2 || !strings.Contains(w[0], "将保留 2 张服装图") || !strings.Contains(w[0], "将有 2 张服装图不参与生成") {
		t.Fatalf("%v", w)
	}
	// 提交预警一律将来时，不出现事后口吻
	for _, w := range append(SubmitRefWarnings(3, 4), SubmitRefWarnings(10, 9)...) {
		if strings.Contains(w, "已") {
			t.Fatalf("提交预警不该用过去时：%s", w)
		}
	}
}
