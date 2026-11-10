package pipeline

import (
	"fmt"
	"strings"
	"unicode"

	"aigc-local-life/pkg/reverse"
)

// ---------------------------------------------------------------- 用户确认的细节框（AI 找细节 / 重新裁切）

// 用户细节框上限：单张服装图 ≤ MaxUserCropsPerImage，整批 ≤ MaxUserCrops。生图时还会按模型参考图上限再裁（CropBudget），
// 这里只防止前端塞进成百上千个框把切图打满。
const (
	MaxUserCropsPerImage = 12
	MaxUserCrops         = 24
	MaxCropLabelRunes    = 24
)

// UserCrop 用户在「AI 找细节」里确认后带回的一个细节框（SubmitTryonReq.detail_crops）。
// GarmentIndex 是提交时 garment_images 的下标（= Image.Source）。
type UserCrop struct {
	GarmentIndex int       `json:"garment_index"`
	Label        string    `json:"label"`
	Box          []float64 `json:"box"`
	Confidence   float64   `json:"confidence,omitempty"`
}

// UserCropPlans 用户细节框 → crop-plan 结果（Index = gs 里的下标），交给 BuildCropJobs 走与自动切图相同的校验 / 换像素。
// 指向不存在（被编号丢弃）的服装图的框丢弃并告警。
func UserCropPlans(gs []Image, crops []UserCrop) ([]reverse.CropPlan, []string) {
	pos := make(map[int]int, len(gs))
	for i, g := range gs {
		if _, ok := pos[g.Source]; !ok {
			pos[g.Source] = i
		}
	}
	byIdx := map[int]*reverse.CropPlan{}
	var order []int
	var warns []string
	for _, c := range crops {
		i, ok := pos[c.GarmentIndex]
		if !ok {
			warns = append(warns, fmt.Sprintf("细节框指向的服装图 %d 不存在，已丢弃", c.GarmentIndex+1))
			continue
		}
		p := byIdx[i]
		if p == nil {
			p = &reverse.CropPlan{Index: i, Angle: gs[i].Angle}
			byIdx[i] = p
			order = append(order, i)
		}
		conf := c.Confidence
		if conf <= 0 {
			conf = 1 // 用户确认过的框
		}
		p.Crops = append(p.Crops, reverse.Crop{Label: c.Label, Box: append([]float64(nil), c.Box...), Confidence: conf})
	}
	out := make([]reverse.CropPlan, 0, len(order))
	for _, i := range order {
		out = append(out, *byIdx[i])
	}
	return out, warns
}

// ---------------------------------------------------------------- 给用户看的读图提示（验收 F12 / B2）

// 读图降级的用户文案（与反推交付包 USAGE「常见提示」同口径）。sidecar 的 warning 原文里带网关失败类别 / HTTP 码，
// 一律不透出，只按 meta 判断后换成这几句固定文案。
const (
	WarnReadDegraded = "读图失败，已改用文字信息生成"
	WarnReadTemplate = "读图失败，已按模板生成提示词，建议检查服装图后重试"
)

// garmentTypeText 模型判断的品类（LLM 输出）收成一个短名词：去掉控制字符、全角引号括号与全部 ASCII 字符，≤16 字；收完为空返回「其他品类」。
func garmentTypeText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if n >= 16 {
			break
		}
		switch {
		case unicode.IsControl(r), strings.ContainsRune("\"'“”‘’「」『』()（）[]【】<>《》{}", r):
			continue
		case r < 0x80:
			continue // 不透出英文代号（模型名、错误码之类），ASCII 一律去掉
		}
		b.WriteRune(r)
		n++
	}
	out := strings.Trim(strings.TrimSpace(b.String()), "，,。.;；:：/、 ")
	if out == "" {
		return "其他品类"
	}
	return out
}

// itemsText 所选单品的中文名，「、」连接。
func itemsText(items []string) string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		if n := ItemLabels[it]; n != "" {
			names = append(names, n)
		}
	}
	return strings.Join(names, "、")
}

// ReadWarnings sidecar 反推结果 → 给用户看的中文提示（写进子任务 prompt_meta.user_warnings，任务详情 warnings 透出）：
//
//   - 品类不符（meta.garment_match=false）：「参考图看起来是X，与所选「文胸」不一致，请确认品类」；多件组合说缺件；
//   - 读图降级（meta.degraded）：读图失败但文字模型兜住 →「读图失败，已改用文字信息生成」；文字也失败、走模板 → WarnReadTemplate。
//
// warning 原文只用来区分「文字兜底 / 模板兜底」，本身不透出（里面有网关失败类别、HTTP 码）。
func ReadWarnings(meta reverse.AnalyzeMeta, warning string, items []string) []string {
	var out []string
	if meta.GarmentMatch != nil && !*meta.GarmentMatch {
		actual := garmentTypeText(meta.GarmentType)
		sel := itemsText(items)
		switch {
		case sel == "":
			out = append(out, fmt.Sprintf("参考图看起来是%s，与所选品类不一致，请确认品类", actual))
		case len(items) > 1:
			out = append(out, fmt.Sprintf("参考图里没有找齐所选的「%s」（识别为%s），请确认品类或补齐服装图", sel, actual))
		default:
			out = append(out, fmt.Sprintf("参考图看起来是%s，与所选「%s」不一致，请确认品类", actual, sel))
		}
	}
	if meta.Degraded {
		if strings.Contains(warning, "模板") {
			out = append(out, WarnReadTemplate)
		} else {
			out = append(out, WarnReadDegraded)
		}
	}
	return out
}

// PoseMappingUserWarning 姿态配对告警（技术原文，含下标数组）→ 给用户看的一句；不是配对告警返回空串。
func PoseMappingUserWarning(tech string) string {
	switch {
	case tech == "":
		return ""
	case strings.Contains(tech, "人数与预览不一致"):
		return "人物数量与预览时不一致，已按默认配对生成"
	case strings.Contains(tech, "配对被反推服务校正"):
		return "所选人物配对无效，已按默认配对生成"
	}
	return ""
}

// DedupeStrings 去空、去重，保持首次出现的顺序；结果为空返回 nil。
func DedupeStrings(list []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
