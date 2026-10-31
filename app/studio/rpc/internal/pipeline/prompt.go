package pipeline

import (
	"fmt"
	"strings"

	"aigc-local-life/pkg/reverse"
)

// OrderLinePrefix 图序声明行的固定开头（用于识别与幂等替换）。
const OrderLinePrefix = "图序："

// ImageOrderLine 图序声明（F2）：
//
//	tryon    图序：图1 是模特，图2–图N 是服装参考图（主图在前，局部细节在后）。
//	pose     图序：图1 是原图，图2 是姿势参考图。
//	imagegen 图序：图1–图N 是参考图。（无参考图返回空串）
func ImageOrderLine(kind string, n int) string {
	switch kind {
	case KindTryon:
		switch {
		case n <= 1:
			return OrderLinePrefix + "图1 是模特。"
		case n == 2:
			return OrderLinePrefix + "图1 是模特，图2 是服装参考图。"
		}
		return fmt.Sprintf("%s图1 是模特，图2–图%d 是服装参考图（主图在前，局部细节在后）。", OrderLinePrefix, n)
	case KindPose:
		return OrderLinePrefix + "图1 是原图，图2 是姿势参考图。"
	case KindImageGen:
		switch {
		case n <= 0:
			return ""
		case n == 1:
			return OrderLinePrefix + "图1 是参考图。"
		}
		return fmt.Sprintf("%s图1–图%d 是参考图。", OrderLinePrefix, n)
	}
	return ""
}

// WithImageOrderLine 在首行加图序声明，正文逐字不动（E9 / F3）。
// 已有我们加的图序行时先去掉再加（重试 / 截断参考图后重算不会叠两行）。
func WithImageOrderLine(prompt, kind string, n int) string {
	body := StripImageOrderLine(prompt)
	line := ImageOrderLine(kind, n)
	if line == "" {
		return body
	}
	return line + "\n" + body
}

// StripImageOrderLine 去掉 WithImageOrderLine 加的首行，返回原正文。
func StripImageOrderLine(prompt string) string {
	if !strings.HasPrefix(prompt, OrderLinePrefix) {
		return prompt
	}
	if _, rest, ok := strings.Cut(prompt, "\n"); ok {
		return rest
	}
	return ""
}

// TryonSpec 商拍反推需要的提交字段（已校验）。
type TryonSpec struct {
	Items   []string
	SetMode string // one / two，空按 one
	Fabric  string // 按件写，原样透传（E4）
	Notes   string // 前端 prompt
	Lengths map[string]string
	Sleeves map[string]string
}

func (s TryonSpec) setMode() string {
	if s.SetMode == "" {
		return "one"
	}
	return s.SetMode
}

// AnalyzeImages 组图结果 → sidecar images[]（需已填 Data）。主图 role=单品中文名 / 整套；细节 role=局部细节 + label；模特 role=人物模特。
func AnalyzeImages(list []Image) []reverse.AnalyzeImage {
	out := make([]reverse.AnalyzeImage, 0, len(list))
	for _, im := range list {
		ai := reverse.AnalyzeImage{Role: im.Role, Data: im.Data}
		switch im.Kind {
		case ImageDetail:
			ai.Role, ai.Label = RoleDetail, im.Label
		case ImageModel:
			ai.Role = RoleModel
		default:
			if ai.Role == "" {
				ai.Role = ItemLabels[im.Item]
			}
		}
		out = append(out, ai)
	}
	return out
}

// BuildTryonAnalyzeReq 单个模特的 analyze 请求：category 按 items 推断，deep=true；
// lengths / sleeves 只保留 items 里的键，单件时同时填 length_class / sleeve_class。
func BuildTryonAnalyzeReq(spec TryonSpec, sel Selection) reverse.AnalyzeReq {
	req := reverse.AnalyzeReq{
		Category: InferCategory(spec.Items),
		Items:    append([]string(nil), spec.Items...),
		Images:   AnalyzeImages(sel.Images),
		SetMode:  spec.setMode(),
		Fabric:   spec.Fabric,
		Notes:    strings.TrimSpace(spec.Notes),
		Lengths:  FilterByItems(spec.Lengths, spec.Items),
		Sleeves:  FilterByItems(spec.Sleeves, spec.Items),
		Deep:     true,
	}
	if len(spec.Items) == 1 {
		req.LengthClass = req.Lengths[spec.Items[0]]
		req.SleeveClass = req.Sleeves[spec.Items[0]]
	}
	if len(req.Lengths) == 0 {
		req.Lengths = nil
	}
	if len(req.Sleeves) == 0 {
		req.Sleeves = nil
	}
	return req
}

// BuildTryonAnalyzeBatchReq 同一组服装 × 多个模特：images 只放主图 + 细节（selection 里的模特被剔除），
// models[].id 用模特 key（m-1…），结果按 model_id 回填缓存。
func BuildTryonAnalyzeBatchReq(spec TryonSpec, sel Selection, models []Image, concurrency int) reverse.AnalyzeBatchReq {
	base := sel
	if base.Model && len(base.Images) > 0 {
		base.Images = base.Images[:len(base.Images)-1]
		base.Model = false
	}
	req := reverse.AnalyzeBatchReq{AnalyzeReq: BuildTryonAnalyzeReq(spec, base), Concurrency: concurrency}
	for _, m := range models {
		req.Models = append(req.Models, reverse.BatchModel{ID: m.Key, Data: m.Data})
	}
	return req
}

// BuildPoseNotes 姿态补充说明：全局 + 本张，按行拼，空的不写。
func BuildPoseNotes(global, perPose string) string {
	var lines []string
	for _, s := range []string{global, perPose} {
		if s = strings.TrimSpace(s); s != "" {
			lines = append(lines, s)
		}
	}
	return strings.Join(lines, "\n")
}
