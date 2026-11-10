package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 商拍反推提示词的图号对齐。
//
// sidecar（tools/prompt-reverse/serve_reverse.py）写提示词时按**反推读图顺序**编号：
// select_images 的 主图 → 局部细节 → 模特（模特恒在最后），正文里有
//
//	【参考图】参考图1：整件服装（上衣）；…；参考图N：模特图（…）。生成时保持参考图顺序与此一致；…
//	以参考图N为底图，只把人物身上的上衣换成参考图1中的服装；…        （新版 _new_edit_instruction）
//	人物、姿势、表情、背景、光线只来自参考图N。                         （新版 _new_reference_role_line）
//	参考图2、参考图3中的…                                               （组合 / 内衣套装按单品指图）
//
// 生图却按 GenerateRefs 的顺序送图：图1 = 模特，后面是主图、细节（张数按模型上限，最多 14，和反推读的 ≤9 张不是同一组）。
// 不对齐的话「以参考图9为底图」会指向一张细节图、「参考图1中的服装」指向模特。所以生图前把正文里的图号
// 按图片 Key 从反推顺序映射到生图顺序，并按生图顺序重写【参考图】那一行。

var refNumRe = regexp.MustCompile(`参考图(\d+)`)

// refListItemRe 【参考图】行里的一项「参考图N：描述」。
var refListItemRe = regexp.MustCompile(`参考图(\d+)：`)

const (
	refListPrefix = "【参考图】"
	refListSuffix = "。生成时保持参考图顺序与此一致"
	// 与 sidecar 同一句（反推读图里没有模特描述可复用时兜底）
	refDescModel = "模特图（只提供人物、姿势、场景、光线，不作为服装依据）"
)

func imageID(im Image) string {
	switch {
	case im.Key != "":
		return im.Key
	case im.URL != "":
		return "u:" + im.URL
	case im.SHA1 != "":
		return "s:" + im.SHA1
	}
	return ""
}

// RefIndexMap 反推读图序号（1 起）→ 生图参考图序号（1 起）。反推读过但没送生图的图（参考图上限小于反推张数时）
// 映射到同一单品在生图里的第一张主图（没有就第一张同单品的图），仍找不到为 0。
func RefIndexMap(analyzed, gen []Image) map[int]int {
	pos := map[string]int{}
	for i, g := range gen {
		if id := imageID(g); id != "" {
			if _, ok := pos[id]; !ok {
				pos[id] = i + 1
			}
		}
	}
	sameItem := func(item string, kind string) int {
		for i, g := range gen {
			if g.Item == item && (kind == "" || g.Kind == kind) {
				return i + 1
			}
		}
		return 0
	}
	out := make(map[int]int, len(analyzed))
	for i, a := range analyzed {
		if p := pos[imageID(a)]; p > 0 {
			out[i+1] = p
			continue
		}
		switch {
		case a.Kind == ImageModel:
			out[i+1] = sameKind(gen, ImageModel)
		case a.Item != "":
			if p := sameItem(a.Item, ImageGarment); p > 0 {
				out[i+1] = p
			} else {
				out[i+1] = sameItem(a.Item, "")
			}
		}
	}
	return out
}

func sameKind(list []Image, kind string) int {
	for i, g := range list {
		if g.Kind == kind {
			return i + 1
		}
	}
	return 0
}

// RemapTryonPrompt 把 sidecar 按反推读图顺序（analyzed，即发给 sidecar 的 images：主图 → 细节 → 模特）写的图号
// 改成生图参考图顺序（gen，GenerateRefs 的 Images）。【参考图】行按 gen 逐张重写（描述沿用 sidecar 对同一张 / 同角色图的原话），
// 其余行里的「参考图N」按 RefIndexMap 改号。两边顺序相同时原样返回。
func RemapTryonPrompt(prompt string, analyzed, gen []Image) string {
	if prompt == "" || len(analyzed) == 0 || len(gen) == 0 || sameOrder(analyzed, gen) {
		return prompt
	}
	idx := RefIndexMap(analyzed, gen)
	lines := strings.Split(prompt, "\n")
	for i, line := range lines {
		if strings.Contains(line, refListPrefix) {
			lines[i] = rewriteRefList(line, analyzed, gen)
			continue
		}
		lines[i] = refNumRe.ReplaceAllStringFunc(line, func(m string) string {
			k, err := strconv.Atoi(refNumRe.FindStringSubmatch(m)[1])
			if err != nil {
				return m
			}
			if n := idx[k]; n > 0 {
				return fmt.Sprintf("参考图%d", n)
			}
			return "服装参考图" // 指向的图没送生图、也没有同单品的替代：去掉图号，免得指错
		})
	}
	return strings.Join(lines, "\n")
}

func sameOrder(a, b []Image) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if imageID(a[i]) == "" || imageID(a[i]) != imageID(b[i]) {
			return false
		}
	}
	return true
}

// rewriteRefList 按生图顺序重写「【参考图】参考图1：…；参考图2：…。生成时保持…」一行。
func rewriteRefList(line string, analyzed, gen []Image) string {
	start := strings.Index(line, refListPrefix) + len(refListPrefix)
	end := strings.Index(line[start:], refListSuffix)
	if end < 0 {
		end = len(line) - start
	}
	body := line[start : start+end]
	// 解析旧描述：反推序号 → 描述
	descByOld := map[int]string{}
	locs := refListItemRe.FindAllStringSubmatchIndex(body, -1)
	for j, loc := range locs {
		k, _ := strconv.Atoi(body[loc[2]:loc[3]])
		stop := len(body)
		if j+1 < len(locs) {
			stop = locs[j+1][0]
		}
		descByOld[k] = strings.TrimRight(body[loc[1]:stop], "；")
	}
	descByID := map[string]string{}
	descByRole := map[string]string{}
	for i, a := range analyzed {
		d, ok := descByOld[i+1]
		if !ok {
			continue
		}
		if id := imageID(a); id != "" {
			descByID[id] = d
		}
		if r := a.Kind + "|" + a.Role; descByRole[r] == "" {
			descByRole[r] = d
		}
	}
	parts := make([]string, 0, len(gen))
	for i, g := range gen {
		d := descByID[imageID(g)]
		if d == "" && g.Kind != ImageDetail { // 细节图的描述带各自 label，不能套用别张
			d = descByRole[g.Kind+"|"+g.Role]
		}
		if d == "" {
			d = fallbackRefDesc(g)
		}
		parts = append(parts, fmt.Sprintf("参考图%d：%s", i+1, d))
	}
	return line[:start] + strings.Join(parts, "；") + line[start+end:]
}

// fallbackRefDesc 与 sidecar ref_lines 同措辞（只在反推读图里没有同一张 / 同角色的描述可复用时用）。
func fallbackRefDesc(g Image) string {
	switch g.Kind {
	case ImageModel:
		return refDescModel
	case ImageDetail:
		if g.Label != "" {
			return "局部细节（" + g.Label + "）"
		}
		return "局部细节"
	}
	role := g.Role
	if role == "" {
		role = ItemLabels[g.Item]
	}
	if role == "" {
		role = "服装"
	}
	return "整件服装（" + role + "）"
}
