package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"aigc-local-life/pkg/reverse"
)

func TestNormalizeModelView(t *testing.T) {
	for in, want := range map[string]string{"front": "front", " BACK ": "back", "Side": "side", "": ""} {
		if got, ok := NormalizeModelView(in); !ok || got != want {
			t.Fatalf("%q → %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"left", "正面", "auto", "front,back", "sides"} {
		if _, ok := NormalizeModelView(bad); ok {
			t.Fatalf("%q 不该被接受", bad)
		}
	}
}

// ViewOf 与 sidecar _image_view 同口径（tools/prompt-reverse/tests/select_images_test.py 同名用例）。
func TestViewOfMatchesSidecar(t *testing.T) {
	cases := []struct {
		im   Image
		want string
	}{
		{Image{Angle: "side"}, "side"},
		{Image{Angle: "front"}, "front"},
		{Image{Angle: "back"}, "back"},
		{Image{Angle: "unknown", Label: "outside pocket"}, ""}, // inside / outside 里的 side 不算侧面
		{Image{Angle: "unknown", Label: "上衣·侧面"}, "side"},
		{Image{Angle: "unknown", Label: "正面和侧面"}, "side"}, // 同时有正面和侧面按侧面算
		{Image{Angle: "front", Label: "上衣·后袋"}, "back"},   // 标签里的「后袋」优先于 front 角度
		{Image{Angle: "flat", Label: "上衣·领口"}, ""},
		{Image{Angle: "unknown", Label: "下装·前袋"}, "front"},
	}
	for _, c := range cases {
		if got := ViewOf(c.im); got != c.want {
			t.Fatalf("%+v → %q，want %q", c.im, got, c.want)
		}
	}
}

func TestFilterByView(t *testing.T) {
	list := []Image{
		{Key: "a", Angle: "front"}, {Key: "b", Angle: "back"}, {Key: "c", Angle: "side"}, {Key: "d", Angle: "flat"},
	}
	keys := func(l []Image) string {
		var k []string
		for _, im := range l {
			k = append(k, im.Key)
		}
		return strings.Join(k, ",")
	}
	for view, want := range map[string]string{"front": "a", "back": "b", "side": "c", "": "a,b,c,d"} {
		if got := keys(FilterByView(list, view)); got != want {
			t.Fatalf("%q → %s，want %s", view, got, want)
		}
	}
	// 没有该面的图：保留全部（用户没传这个面）
	only := []Image{{Key: "a", Angle: "front"}, {Key: "d", Angle: "flat"}}
	if got := keys(FilterByView(only, "back")); got != "a,d" {
		t.Fatalf("没有背面图应保留全部：%s", got)
	}
	if len(list) != 4 {
		t.Fatal("入参被改了")
	}
}

func TestAnalyzeImagesCarryAngleAndKey(t *testing.T) {
	list := []Image{
		{Key: "g-top-front-1", Kind: ImageGarment, Item: ItemTop, Angle: "front", Role: "上衣"},
		{Key: "g-top-flat-1", Kind: ImageGarment, Item: ItemTop, Angle: "flat", Role: "上衣"},
		{Key: "g-top-back-1-c1", Kind: ImageDetail, Item: ItemTop, Angle: "back", Label: "上衣·后领"},
		{Key: "m-1", Kind: ImageModel, Role: RoleModel},
	}
	got := AnalyzeImages(list)
	if got[0].Angle != "front" || got[0].Key != "g-top-front-1" {
		t.Fatalf("%+v", got[0])
	}
	if got[1].Angle != "" { // flat 不传
		t.Fatalf("flat 不应传 angle：%+v", got[1])
	}
	if got[2].Angle != "back" || got[2].Role != RoleDetail || got[2].Label != "上衣·后领" {
		t.Fatalf("%+v", got[2])
	}
	if got[3].Angle != "" || got[3].Key != "m-1" || got[3].Role != RoleModel {
		t.Fatalf("%+v", got[3])
	}
}

func TestViewAndGarmentCacheKeys(t *testing.T) {
	sha := strings.Repeat("A", 40)
	if AnalyzeViewKey(sha, "") != AnalyzeKey(sha) {
		t.Fatal("没传视角时必须与旧键逐字一致")
	}
	k := AnalyzeViewKey(sha, "side")
	if k == AnalyzeKey(sha) || len(k) > 80 {
		t.Fatalf("带视角的键应不同且 ≤80：%s", k)
	}
	if AnalyzeViewKey(sha, "front") == AnalyzeViewKey(sha, "back") {
		t.Fatal("不同视角不能共用缓存")
	}
	if SourceGarmentsKey(sha, nil) != SourceKey(sha) {
		t.Fatal("没有服装图时必须与旧键逐字一致")
	}
	a, b := SourceGarmentsKey(sha, []string{"x", "y"}), SourceGarmentsKey(sha, []string{"y", "x"})
	if a == b || a == SourceKey(sha) || len(a) > 80 {
		t.Fatalf("服装图集合（顺序敏感）要进键：%s %s", a, b)
	}
}

func TestImageOrderLinePoseGarments(t *testing.T) {
	cases := map[int]string{
		2: "图序：图1 是原图，图2 是姿势参考图。",
		3: "图序：图1 是原图，图2 是姿势参考图，图3 是服装参考图。",
		8: "图序：图1 是原图，图2 是姿势参考图，图3–图8 是服装参考图。",
	}
	for n, want := range cases {
		if got := ImageOrderLine(KindPose, n); got != want {
			t.Fatalf("n=%d: %q", n, got)
		}
	}
}

func TestAnalyzedOrder(t *testing.T) {
	sent := []Image{
		{Key: "g-top-front-1", Kind: ImageGarment}, {Key: "g-top-front-1-c1", Kind: ImageDetail}, {Key: "g-top-front-1-c2", Kind: ImageDetail},
		{Key: "m-2", Kind: ImageModel, Role: RoleModel},
	}
	mf := func(keys ...string) []reverse.ManifestItem {
		var out []reverse.ManifestItem
		for i, k := range keys {
			it := reverse.ManifestItem{Index: i + 1, Key: k}
			if strings.HasPrefix(k, "m-") {
				it.Role = RoleModel
			}
			out = append(out, it)
		}
		return out
	}
	// sidecar 丢了一张细节、把细节调了顺序；manifest 里的模特是 m-1（同图模特的缓存），按角色认成当前的 m-2
	got, ok := AnalyzedOrder(sent, mf("g-top-front-1", "g-top-front-1-c2", "m-1"))
	if !ok || len(got) != 3 || got[1].Key != "g-top-front-1-c2" || got[2].Key != "m-2" {
		t.Fatalf("%v %+v", ok, got)
	}
	for name, bad := range map[string][]reverse.ManifestItem{
		"空":     nil,
		"没有key": {{Index: 1}},
		"未知key": mf("zzz"),
		"重复key": mf("g-top-front-1", "g-top-front-1"),
	} {
		if _, ok := AnalyzedOrder(sent, bad); ok {
			t.Fatalf("%s 不该匹配", name)
		}
	}
}

// ---------------------------------------------------------------- 图号 = 生图实际送图顺序（front / back / side）

// sidecarViewPrompt 按 sidecar 新版的写法拼提示词：「参考图N：整件服装（上衣·正面）」按读图顺序（analyzed）编号，
// 底图 / 角色句里引用模特与第一件服装的编号。
func sidecarViewPrompt(analyzed []Image) string {
	angleCN := map[string]string{"front": "正面", "back": "背面", "side": "侧面"}
	var refs []string
	model, firstGarment := 0, 0
	for i, im := range analyzed {
		switch im.Kind {
		case ImageModel:
			model = i + 1
			refs = append(refs, fmt.Sprintf("参考图%d：%s", i+1, refDescModel))
		case ImageDetail:
			refs = append(refs, fmt.Sprintf("参考图%d：局部细节（%s）", i+1, im.Label))
		default:
			if firstGarment == 0 {
				firstGarment = i + 1
			}
			a := angleCN[im.Angle]
			if a != "" {
				a = "·" + a
			}
			refs = append(refs, fmt.Sprintf("参考图%d：整件服装（%s%s）", i+1, im.Role, a))
		}
	}
	return fmt.Sprintf("【编辑指令】以参考图%d为底图，只把人物身上的上衣换成参考图%d中的服装；\n\n", model, firstGarment) +
		"【参考图】" + strings.Join(refs, "；") + "。生成时保持参考图顺序与此一致；局部细节只用于结构/纹理，模特图只用于人物/姿势/场景。\n" +
		fmt.Sprintf("【参考图角色】人物、姿势、表情、背景、光线只来自参考图%d。", model)
}

// manifestOf 模拟 sidecar 的 image_manifest：按它实际读图的顺序回显 key。
func manifestOf(analyzed []Image) []reverse.ManifestItem {
	out := make([]reverse.ManifestItem, len(analyzed))
	for i, im := range analyzed {
		out[i] = reverse.ManifestItem{Index: i + 1, Ref: fmt.Sprintf("参考图%d", i+1), Role: im.Role, Label: im.Label, Angle: im.Angle, Key: im.Key}
	}
	return out
}

var refItemRe = regexp.MustCompile(`参考图(\d+)：([^；。]*)`)

// 对每个面：筛图 → 组图 → sidecar 读图（这里让它把细节重排并丢掉一张，模拟 _balance_crops / 材质去重）→ 改号 →
// 最终提示词里「第 N 张」的描述必须与生图第 N 张图（gen.Images[N-1]）是同一张；底图 = 图1 = 模特；其它面的图一张不进。
func TestPromptNumberingEqualsSentOrderPerView(t *testing.T) {
	var gs, details []Image
	for _, a := range []string{"front", "back", "side"} {
		for seq := 1; seq <= 2; seq++ {
			gs = append(gs, garment(ItemTop, a, seq))
		}
		for c := 1; c <= 3; c++ {
			d := Image{Key: fmt.Sprintf("g-top-%s-1-c%d", a, c), Kind: ImageDetail, Item: ItemTop, Angle: a, Role: RoleDetail,
				Label: fmt.Sprintf("上衣·%s细节%d", a, c)}
			details = append(details, d)
		}
	}
	for _, view := range []string{ViewFront, ViewBack, ViewSide} {
		model := Image{Key: "m-1", Kind: ImageModel, Role: RoleModel, SHA1: "msha"}
		sel := SelectImages(ItemTop, FilterByView(gs, view), FilterByView(details, view), &model)
		for _, im := range sel.Images {
			if im.Kind != ImageModel && ViewOf(im) != view {
				t.Fatalf("%s：反推读图混进了 %s 的图 %s", view, ViewOf(im), im.Key)
			}
		}
		// sidecar 实际读图：同一批图，细节换顺序并丢掉其中一张
		sent := withSelModel(sel, model)
		var read []Image
		var ds []Image
		for _, im := range sent {
			if im.Kind == ImageDetail {
				ds = append(ds, im)
			} else if im.Kind != ImageModel {
				read = append(read, im)
			}
		}
		for i := len(ds) - 1; i >= 1; i-- { // 倒序、丢掉第一张
			read = append(read, ds[i])
		}
		read = append(read, model)
		raw := sidecarViewPrompt(read)

		gen, out, ok := TryonGenPrompt(sel, model, 14, raw, manifestOf(read))
		if !ok {
			t.Fatalf("%s：manifest 应对得上", view)
		}
		if gen.Images[0].Key != "m-1" {
			t.Fatalf("%s：图1 应是模特", view)
		}
		if !strings.Contains(out, "以参考图1为底图") || !strings.Contains(out, "只来自参考图1。") {
			t.Fatalf("%s：底图编号没改成图1：\n%s", view, out)
		}
		items := refItemRe.FindAllStringSubmatch(out[strings.Index(out, "【参考图】"):], -1)
		if len(items) != len(gen.Images) {
			t.Fatalf("%s：【参考图】应列 %d 张，得 %d：\n%s", view, len(gen.Images), len(items), out)
		}
		for i, it := range items {
			n, _ := strconv.Atoi(it[1])
			if n != i+1 {
				t.Fatalf("%s：第 %d 项编号是 %d", view, i+1, n)
			}
			g := gen.Images[i]
			switch g.Kind {
			case ImageModel:
				if !strings.Contains(it[2], "模特图") {
					t.Fatalf("%s：图%d 应是模特：%s", view, i+1, it[2])
				}
			case ImageDetail:
				if !strings.Contains(it[2], g.Label) {
					t.Fatalf("%s：图%d 应是细节 %s：%s", view, i+1, g.Label, it[2])
				}
			default:
				if !strings.Contains(it[2], map[string]string{"front": "正面", "back": "背面", "side": "侧面"}[g.Angle]) {
					t.Fatalf("%s：图%d 应是 %s 的服装图：%s", view, i+1, g.Angle, it[2])
				}
			}
		}
		// 首件服装的编号：「参考图K中的服装」的 K 必须指向生图里的第一张主图
		m := regexp.MustCompile(`换成参考图(\d+)中的服装`).FindStringSubmatch(out)
		k, _ := strconv.Atoi(m[1])
		if gen.Images[k-1].Kind != ImageGarment {
			t.Fatalf("%s：「参考图%d中的服装」指向了 %s", view, k, gen.Images[k-1].Kind)
		}
		// 其它面的图一张不送
		for _, g := range gen.Images {
			if g.Kind != ImageModel && ViewOf(g) != view {
				t.Fatalf("%s：生图混进了 %s 的图 %s", view, ViewOf(g), g.Key)
			}
		}
	}
}

// 没有 manifest（旧版 sidecar）：按发送顺序改号，行为与改动前一致。
func TestTryonGenPromptWithoutManifest(t *testing.T) {
	model := Image{Key: "m-1", Kind: ImageModel, Role: RoleModel}
	sel := SelectImages(ItemTop, []Image{garment(ItemTop, "front", 1), garment(ItemTop, "back", 1)}, nil, &model)
	raw := sidecarViewPrompt(withSelModel(sel, model))
	_, out, ok := TryonGenPrompt(sel, model, 14, raw, nil)
	if !ok || !strings.Contains(out, "以参考图1为底图") {
		t.Fatalf("%v\n%s", ok, out)
	}
	// manifest 对不上：ok=false，仍按发送顺序改号（不 panic、不丢提示词）
	_, out2, ok2 := TryonGenPrompt(sel, model, 14, raw, []reverse.ManifestItem{{Index: 1, Key: "nope"}})
	if ok2 || out2 != out {
		t.Fatalf("对不上时应回退到发送顺序：%v", ok2)
	}
}
