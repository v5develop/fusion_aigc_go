package pipeline

import (
	"fmt"
	"sort"
)

// MaxInputImages 一次反推 / 生图的输入图上限（D1）。
const MaxInputImages = 9

// Selection 组图结果。Images 顺序：主图 → 细节 → 模特（1 张，最后），即反推用的顺序（D2）。
// 生图时图 1 必须是模特，用 GenerateOrder。
type Selection struct {
	Images  []Image
	Mains   int
	Details int
	Model   bool
	Dropped int // 去重或超额未选的图数
	// 去重后的全部主图 / 细节图（未按 9 张截取）：生图按各模型的参考图上限另行组装（GenerateRefs），
	// Images 只用于反推读图（sidecar 一次最多读 9 张）。
	PoolMains   []Image
	PoolDetails []Image
}

// MainLimit 主图上限：look / compose ≤6；其他品类多于 1 张时 ≤4，仅 1 张时 ≤2（即全取）。
func MainLimit(category string, n int) int {
	switch {
	case category == ItemLook || category == CategoryCompose:
		return min(6, n)
	case n > 1:
		return min(4, n)
	}
	return min(2, n)
}

// SelectImages 组图（与 sidecar select_images 同口径，修正其 D3 问题）：
//
//  1. 先按 SHA1 去重（模特优先保留，其次主图、细节；SHA1 为空的不参与去重）；
//  2. 主图取 MainLimit 张：按单品轮询（每件至少 1 张，D3），输出保持编号顺序；
//  3. 细节名额 max(1, 9-主图-模特位)：按单品分组轮询，还没有主图入选的单品排在前面（D3）；
//  4. 模特恰好 1 张放最后；总数 ≤9（D1）。
//
// category 为 InferCategory 的结果；model 为 nil 时没有模特位（图片生成 / 只反推服装）。
func SelectImages(category string, mains, details []Image, model *Image) Selection {
	seen := map[string]bool{}
	dedupe := func(list []Image) ([]Image, int) {
		out := make([]Image, 0, len(list))
		dup := 0
		for _, im := range list {
			if im.SHA1 != "" {
				if seen[im.SHA1] {
					dup++
					continue
				}
				seen[im.SHA1] = true
			}
			out = append(out, im)
		}
		return out, dup
	}
	var sel Selection
	if model != nil && model.SHA1 != "" {
		seen[model.SHA1] = true
	}
	mains, d1 := dedupe(mains)
	details, d2 := dedupe(details)
	sel.Dropped = d1 + d2
	sel.PoolMains, sel.PoolDetails = mains, details

	modelSlots := 0
	if model != nil {
		modelSlots = 1
	}
	pickedMains := pickRoundRobin(mains, MainLimit(category, len(mains)), nil, true)
	covered := map[string]bool{}
	for _, m := range pickedMains {
		covered[m.Item] = true
	}
	quota := max(1, MaxInputImages-len(pickedMains)-modelSlots)
	pickedDetails := pickRoundRobin(details, quota, covered, false)

	// 主图 ≤6、细节名额 = 9-主图-模特位，合计恒 ≤9（D1），无需再截断。
	sel.Images = append(sel.Images, pickedMains...)
	sel.Images = append(sel.Images, pickedDetails...)
	if model != nil {
		sel.Images = append(sel.Images, *model)
		sel.Model = true
	}
	sel.Mains, sel.Details = len(pickedMains), len(pickedDetails)
	sel.Dropped += len(mains) - len(pickedMains) + len(details) - len(pickedDetails)
	return sel
}

func groupKey(im Image) string {
	if im.Item != "" {
		return im.Item
	}
	if k := LabelItem(im.Label); k != "" {
		return k
	}
	return "?"
}

// pickRoundRobin 按单品分组轮询取 limit 张。covered 里的单品排在后面（优先补没覆盖的）；
// keepOrder=true 时输出恢复原顺序（主图），否则按轮询顺序（细节，与 sidecar _balance_crops 一致）。
func pickRoundRobin(list []Image, limit int, covered map[string]bool, keepOrder bool) []Image {
	if limit <= 0 || len(list) == 0 {
		return nil
	}
	groups := map[string][]int{}
	var order []string
	for i, im := range list {
		k := groupKey(im)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	if len(covered) > 0 {
		var first, last []string
		for _, k := range order {
			if covered[k] {
				last = append(last, k)
			} else {
				first = append(first, k)
			}
		}
		order = append(first, last...)
	}
	var picked []int
	for round := 0; len(picked) < limit; round++ {
		progressed := false
		for _, k := range order {
			if round < len(groups[k]) && len(picked) < limit {
				picked = append(picked, groups[k][round])
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	if keepOrder {
		sort.Ints(picked)
	}
	out := make([]Image, len(picked))
	for i, idx := range picked {
		out[i] = list[idx]
	}
	return out
}

// GenerateOrder 生图输入顺序：图 1 = 模特，图 2..N = 主图 → 细节（F2 / G4）。
func GenerateOrder(sel Selection) []Image {
	if !sel.Model || len(sel.Images) == 0 {
		return append([]Image(nil), sel.Images...)
	}
	n := len(sel.Images)
	out := make([]Image, 0, n)
	out = append(out, sel.Images[n-1])
	return append(out, sel.Images[:n-1]...)
}

// CropBudget 细节图名额：参考图上限 − 主图张数 − 1（模特图），不为负。
func CropBudget(refCap, mains int) int { return max(0, refCap-mains-1) }

// GenRefs 一次生图的参考图组装结果。Images 顺序：模特 → 主图 → 细节（图 1 恒为模特）。
type GenRefs struct {
	Images         []Image
	Cap            int
	Mains          int
	Details        int
	DroppedMains   int // 因参考图上限放不下、没送生图的服装主图数
	DroppedDetails int // 因名额不足没送生图的细节图数
}

// Warnings 给用户看的中文提示（不含供应商 / 模型代号）；没有裁剪返回 nil。
// 这是切图、组装之后的**事后**说明（写进子任务 prompt_meta.warnings），用过去时；
// 提交时还没切图，预警用 SubmitRefWarnings（将来时、约数）。
func (g GenRefs) Warnings() []string {
	var out []string
	if g.DroppedMains > 0 {
		out = append(out, fmt.Sprintf("所选模型最多支持 %d 张参考图，已保留 %d 张服装图，其余 %d 张未参与生成；可减少服装图，或换用支持更多参考图的模型", g.Cap, g.Mains, g.DroppedMains))
	}
	if g.DroppedDetails > 0 {
		if g.Details == 0 {
			out = append(out, "所选模型的参考图数量已被服装图占满，细节图已省略")
		} else {
			out = append(out, fmt.Sprintf("所选模型参考图数量有限，细节图已减少为 %d 张", g.Details))
		}
	}
	return out
}

// GenerateRefs 按参考图上限 refCap 组装生图参考图（商拍）：
//
//   - 模特 1 张放第一位；服装主图至多 refCap-1 张，按单品轮询取（每件至少保留 1 张）；
//   - 细节图名额 = CropBudget(refCap, 主图数)，没有主图入选的单品优先；总数恒 ≤ refCap（≤14）。
//
// 选哪些图与反推读图（sel.Images，≤9）无关：6 张/件的服装图全部可以作生图参考。但**顺序**向反推读图看齐：
// 主图段、细节段里反推读过的图按反推顺序排在前面，多出来的接在后面（preferAnalyzed）。这样在常见情形
// （上限 ≥ 反推张数）下，生图图号 = 反推图号整体后移一位（模特从末位挪到图1），sidecar 正文里的「参考图N」
// 由 RemapTryonPrompt 按 Key 改号即可对上；主图仍在细节之前，图序声明行「主图在前，局部细节在后」依然成立。
func GenerateRefs(sel Selection, model Image, refCap int) GenRefs {
	refCap = ClampRefCap(refCap)
	g := GenRefs{Cap: refCap}
	mains := pickRoundRobin(sel.PoolMains, max(0, refCap-1), nil, true)
	covered := map[string]bool{}
	for _, m := range mains {
		covered[m.Item] = true
	}
	details := pickRoundRobin(sel.PoolDetails, CropBudget(refCap, len(mains)), covered, false)
	mains, details = preferAnalyzed(mains, sel.Images), preferAnalyzed(details, sel.Images)
	g.Images = make([]Image, 0, 1+len(mains)+len(details))
	g.Images = append(g.Images, model)
	g.Images = append(g.Images, mains...)
	g.Images = append(g.Images, details...)
	g.Mains, g.Details = len(mains), len(details)
	g.DroppedMains = len(sel.PoolMains) - len(mains)
	g.DroppedDetails = len(sel.PoolDetails) - len(details)
	return g
}

// preferAnalyzed picked 里反推读过的图（出现在 analyzed 里的）按 analyzed 的顺序排前面，其余保持原相对顺序接在后面。
// 只调顺序、不增删。
func preferAnalyzed(picked, analyzed []Image) []Image {
	rank := map[string]int{}
	for i, a := range analyzed {
		if id := imageID(a); id != "" {
			if _, ok := rank[id]; !ok {
				rank[id] = i
			}
		}
	}
	out := make([]Image, 0, len(picked))
	var rest []Image
	var hit []Image
	for _, im := range picked {
		if _, ok := rank[imageID(im)]; ok && imageID(im) != "" {
			hit = append(hit, im)
		} else {
			rest = append(rest, im)
		}
	}
	sort.SliceStable(hit, func(i, j int) bool { return rank[imageID(hit[i])] < rank[imageID(hit[j])] })
	out = append(out, hit...)
	return append(out, rest...)
}

// 提交时预估每张主图能切出的细节图数（sidecar 每张图 ≤4 个框），只用于提交预警。
const estCropsPerMain = 4

// SubmitRefWarnings 提交时的预警（此时还没切图）：按服装图张数 mains 和模型参考图上限 refCap 推算，
// 服装图放不下 → 提示将有几张不参与生成；细节名额小于预估切图数 → 提示细节图将减少为约几张。
// 都是推算，所以用将来时与「约」；切图之后的实际裁剪见 GenRefs.Warnings（过去时）。
func SubmitRefWarnings(refCap, mains int) []string {
	refCap = ClampRefCap(refCap)
	kept := min(mains, max(0, refCap-1))
	droppedMains := mains - kept
	budget := CropBudget(refCap, kept)
	est := min(estCropsPerMain*mains, CropBudget(GlobalMaxRefImages, kept))
	var out []string
	if droppedMains > 0 {
		out = append(out, fmt.Sprintf("所选模型最多支持 %d 张参考图，将保留 %d 张服装图，将有 %d 张服装图不参与生成；可减少服装图，或换用支持更多参考图的模型", refCap, kept, droppedMains))
	}
	if budget < est {
		if budget == 0 {
			out = append(out, "所选模型的参考图数量将被服装图占满，细节图将省略")
		} else {
			out = append(out, fmt.Sprintf("所选模型参考图数量有限，细节图将减少为约 %d 张", budget))
		}
	}
	return out
}
