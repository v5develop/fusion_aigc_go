package pipeline

import "sort"

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
