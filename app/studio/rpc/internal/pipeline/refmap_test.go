package pipeline

import (
	"fmt"
	"strings"
	"testing"
)

func garment(item, angle string, seq int) Image {
	return Image{Key: GarmentKey(item, angle, seq), Kind: ImageGarment, Item: item, Angle: angle, Role: ItemLabels[item]}
}

func detail(item string, seq int, label string) Image {
	return Image{Key: fmt.Sprintf("g-%s-front-1-c%d", item, seq), Kind: ImageDetail, Item: item, Role: RoleDetail,
		Label: ItemLabels[item] + "·" + label}
}

// sidecarPrompt 按 sidecar 的写法（反推读图顺序编号、模特在末位）拼一段提示词。
func sidecarPrompt(analyzed []Image) string {
	var refs []string
	modelIdx := 0
	for i, im := range analyzed {
		switch im.Kind {
		case ImageModel:
			modelIdx = i + 1
			refs = append(refs, fmt.Sprintf("参考图%d：%s", i+1, refDescModel))
		case ImageDetail:
			refs = append(refs, fmt.Sprintf("参考图%d：局部细节（%s）", i+1, im.Label))
		default:
			refs = append(refs, fmt.Sprintf("参考图%d：整件服装（%s）", i+1, im.Role))
		}
	}
	return "【编辑指令】以参考图" + fmt.Sprint(modelIdx) + "为底图，只把人物身上的上衣换成参考图1中的服装；\n\n" +
		"【参考图】" + strings.Join(refs, "；") + "。生成时保持参考图顺序与此一致；局部细节只用于结构/纹理，模特图只用于人物/姿势/场景。\n" +
		"【参考图角色】人物、姿势、表情、背景、光线只来自参考图" + fmt.Sprint(modelIdx) + "。"
}

// 反推读 ≤9 张（主图 → 细节 → 模特），生图送 ≤14 张（模特 → 主图 → 细节）：正文图号必须按生图顺序改写。
func TestRemapTryonPromptFollowsGenerateRefs(t *testing.T) {
	model := Image{Key: "m-1", Kind: ImageModel, Role: RoleModel}
	var mains, details []Image
	for i, a := range []string{"front", "back", "side", "flat", "detail", "unknown"} {
		mains = append(mains, garment(ItemTop, a, i+1))
	}
	for i := 1; i <= 6; i++ {
		details = append(details, detail(ItemTop, i, fmt.Sprintf("细节%d", i)))
	}
	sel := SelectImages(ItemTop, mains, details, &model)
	if len(sel.Images) != 9 || sel.Images[len(sel.Images)-1].Key != "m-1" {
		t.Fatalf("反推读图应 9 张、模特在末位: %d", len(sel.Images))
	}
	analyzed := sel.Images
	gen := GenerateRefs(sel, model, 14)
	if len(gen.Images) <= len(analyzed) || gen.Images[0].Key != "m-1" {
		t.Fatalf("生图应多于反推张数且图1是模特: %d", len(gen.Images))
	}
	// 反推读过的服装图在生图里整体后移一位（模特从末位挪到图1）
	idx := RefIndexMap(analyzed, gen.Images)
	for i := 1; i < len(analyzed); i++ {
		if analyzed[i-1].Kind == ImageGarment && idx[i] != i+1 {
			t.Fatalf("反推主图 %d 应映射到生图 %d，得 %d", i, i+1, idx[i])
		}
		if got := gen.Images[idx[i]-1].Key; got != analyzed[i-1].Key {
			t.Fatalf("反推图 %d（%s）映射到生图 %d 是 %s", i, analyzed[i-1].Key, idx[i], got)
		}
	}
	if idx[len(analyzed)] != 1 {
		t.Fatalf("模特应映射到图1，得 %d", idx[len(analyzed)])
	}

	out := RemapTryonPrompt(sidecarPrompt(analyzed), analyzed, gen.Images)
	if !strings.Contains(out, "以参考图1为底图") || !strings.Contains(out, "换成参考图2中的服装") ||
		!strings.Contains(out, "只来自参考图1。") {
		t.Fatalf("正文图号没改对:\n%s", out)
	}
	// 【参考图】行按生图顺序逐张重写：张数 = 生图张数，第 i 项描述对应生图第 i 张
	line := out[strings.Index(out, refListPrefix):]
	line = line[:strings.Index(line, "\n")]
	if n := len(refListItemRe.FindAllString(line, -1)); n != len(gen.Images) {
		t.Fatalf("【参考图】应列 %d 张，得 %d: %s", len(gen.Images), n, line)
	}
	if !strings.Contains(line, "参考图1："+refDescModel) {
		t.Fatalf("图1应是模特: %s", line)
	}
	for i, g := range gen.Images {
		var want string
		switch g.Kind {
		case ImageModel:
			want = refDescModel
		case ImageDetail:
			want = "局部细节（" + g.Label + "）"
		default:
			want = "整件服装（" + g.Role + "）"
		}
		if !strings.Contains(line, fmt.Sprintf("参考图%d：%s", i+1, want)) {
			t.Fatalf("参考图%d 应是 %s: %s", i+1, want, line)
		}
	}
	if !strings.HasSuffix(line, "模特图只用于人物/姿势/场景。") {
		t.Fatalf("行尾说明应保留: %s", line)
	}
	// 重放幂等：从同一份 sidecar 原文改，结果不变
	if again := RemapTryonPrompt(sidecarPrompt(analyzed), analyzed, gen.Images); again != out {
		t.Fatal("同一原文两次改写结果应一致")
	}
}

// 参考图上限小于反推张数（如 3 张）：没送生图的服装图改指同单品的主图，模特仍是图1。
func TestRemapTryonPromptSmallCap(t *testing.T) {
	model := Image{Key: "m-1", Kind: ImageModel, Role: RoleModel}
	mains := []Image{garment(ItemTop, "front", 1), garment(ItemTop, "back", 2), garment(ItemBottom, "front", 1)}
	sel := SelectImages(CategoryCompose, mains, nil, &model)
	gen := GenerateRefs(sel, model, 3)
	if len(gen.Images) != 3 || gen.Images[0].Key != "m-1" {
		t.Fatalf("cap 3: %+v", gen.Images)
	}
	idx := RefIndexMap(sel.Images, gen.Images)
	for k, v := range idx {
		if v < 1 || v > len(gen.Images) {
			t.Fatalf("反推图 %d 映射越界: %d", k, v)
		}
		if a, g := sel.Images[k-1], gen.Images[v-1]; a.Item != g.Item && a.Kind != ImageModel {
			t.Fatalf("反推图 %d（%s）应映射到同单品，得 %s", k, a.Key, g.Key)
		}
	}
	out := RemapTryonPrompt(sidecarPrompt(sel.Images), sel.Images, gen.Images)
	if !strings.Contains(out, "以参考图1为底图") || strings.Contains(out, "参考图4") {
		t.Fatalf("小上限改写:\n%s", out)
	}
}

func TestRemapTryonPromptSameOrderUnchanged(t *testing.T) {
	list := []Image{{Key: "m-1", Kind: ImageModel}, garment(ItemTop, "front", 1)}
	p := "以参考图1为底图，换成参考图2中的服装"
	if got := RemapTryonPrompt(p, list, list); got != p {
		t.Fatalf("顺序相同应原样返回: %s", got)
	}
}
