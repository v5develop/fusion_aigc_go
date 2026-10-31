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
