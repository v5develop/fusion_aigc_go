package pipeline

import (
	"strings"

	"aigc-local-life/pkg/reverse"
)

// 商拍模特图的「面」（SubmitTryonReq.model_views[i]）：这位模特拍的是正面 / 背面 / 侧面。
// 编排按它给这位模特单独筛服装图：正面模特不喂背面图、背面模特不喂正面图（反推读图与生图参考图同一份）。
const (
	ViewFront = "front"
	ViewBack  = "back"
	ViewSide  = "side"
)

// NormalizeModelView 提交里的 model_views 项 → front / back / side；空串返回 ""（= 不筛）；其它值 ok=false。
// 只认三个规范值（大小写、首尾空白不敏感），不认中文别名：契约字段是枚举，别名只在 sidecar 内部兼容。
func NormalizeModelView(s string) (string, bool) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "":
		return "", true
	case ViewFront, ViewBack, ViewSide:
		return v, true
	}
	return "", false
}

// 与 sidecar（tools/prompt-reverse/serve_reverse.py FRONT_WORDS / BACK_WORDS / SIDE_WORDS / _image_view）同一份词表与判定顺序：
// angle == side 精确命中；其余在 angle + label 拼起来的小写文本里按 back → side → front 找词。
// 两边必须同口径：Go 先按它筛一遍再发给 sidecar，sidecar 自己再筛一遍时结果不变（筛完的图都命中同一个面）。
var (
	frontWords = []string{"front", "正面", "前片", "左前", "右前", "前中", "前袋", "前门襟"}
	backWords  = []string{"back", "背面", "后片", "左后", "右后", "后中", "后袋", "后领", "后腰"}
	sideWords  = []string{"侧面", "侧片", "侧身", "侧背"}
)

// ViewOf 一张服装图 / 细节图属于哪个面；判断不出返回 ""。
func ViewOf(im Image) string {
	angle := strings.ToLower(strings.TrimSpace(im.Angle))
	if angle == ViewSide {
		return ViewSide
	}
	text := strings.ToLower(angle + " " + im.Label)
	switch {
	case containsAny(text, backWords):
		return ViewBack
	case containsAny(text, sideWords):
		return ViewSide
	case containsAny(text, frontWords):
		return ViewFront
	}
	return ""
}

func containsAny(text string, words []string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// FilterByView 只留属于 view 的图；view 为空，或一张都没有命中（用户没传这个面的图）时原样返回——与 sidecar 同一条回退，
// 主图与细节图各自判断（「有背面主图、没有背面细节」时主图只留背面、细节保留全部）。返回新切片，不改入参。
func FilterByView(list []Image, view string) []Image {
	if view == "" {
		return list
	}
	out := make([]Image, 0, len(list))
	for _, im := range list {
		if ViewOf(im) == view {
			out = append(out, im)
		}
	}
	if len(out) == 0 {
		return list
	}
	return out
}

// AnalyzedOrder 把「发给 sidecar 的图」换成 sidecar 实际读图的顺序（提示词里「参考图 N」的 N）。
// 依据是响应的 image_manifest（每项回显请求里的 key）：sidecar 可能过滤 / 去重（一件单品只留一张材质图）/ 重排，
// 提示词编号跟的是它的 manifest，不是我们发送的顺序。manifest 为空（旧版 sidecar）或没有 key、
// 或 key 对不上发送的图 / 有重复时 ok=false，调用方按发送顺序处理。
func AnalyzedOrder(sent []Image, manifest []reverse.ManifestItem) (out []Image, ok bool) {
	if len(manifest) == 0 {
		return nil, false
	}
	byKey := make(map[string]Image, len(sent))
	var model *Image
	for i, im := range sent {
		if im.Key != "" {
			byKey[im.Key] = im
		}
		if im.Kind == ImageModel {
			model = &sent[i]
		}
	}
	seen := make(map[string]bool, len(manifest))
	out = make([]Image, 0, len(manifest))
	for _, m := range manifest {
		// 反推结果按模特图内容缓存（同一张模特图的几个编号共用）：manifest 里的模特图 key 可能是另一位同图模特的，按角色认
		if m.Role == RoleModel && model != nil && !seen[model.Key] {
			seen[model.Key] = true
			out = append(out, *model)
			continue
		}
		im, found := byKey[m.Key]
		if m.Key == "" || !found || seen[m.Key] {
			return nil, false
		}
		seen[m.Key] = true
		out = append(out, im)
	}
	return out, true
}

// TryonGenPrompt 一位模特的生图参考图与最终提示词：图序以这里为准。
//   - gen：按该模特的组图结果 sel（已按这位模特的面筛过）和参考图上限 refCap 组装的生图参考图，图 1 = 模特，其后主图、细节；
//   - 提示词里的「参考图 N」按 sidecar 实际读图顺序（manifest；没有或对不上时按发送顺序 = withModel(sel, m)）编号，
//     再按图片 Key 改号对齐 gen 的顺序（RemapTryonPrompt）。
//
// manifestOK=false 表示 sidecar 返回了 manifest 却对不上我们发的图（调用方记日志，仍按发送顺序改号）。
func TryonGenPrompt(sel Selection, m Image, refCap int, prompt string, manifest []reverse.ManifestItem) (gen GenRefs, out string, manifestOK bool) {
	gen = GenerateRefs(sel, m, refCap)
	sent := withSelModel(sel, m)
	analyzed, manifestOK := sent, true
	if len(manifest) > 0 {
		if got, ok := AnalyzedOrder(sent, manifest); ok {
			analyzed = got
		} else {
			manifestOK = false
		}
	}
	return gen, RemapTryonPrompt(prompt, analyzed, gen.Images), manifestOK
}

// withSelModel 组图结果里的模特换成 m（各模特共用同一组服装图），即发给 sidecar 读图的图序：主图 → 细节 → 模特。
func withSelModel(sel Selection, m Image) []Image {
	imgs := append([]Image(nil), sel.Images...)
	if sel.Model && len(imgs) > 0 {
		imgs[len(imgs)-1] = m
	} else {
		imgs = append(imgs, m)
	}
	return imgs
}
