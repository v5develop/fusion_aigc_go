package pipeline

import (
	"fmt"
	"strings"

	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/reverse"
)

// 切图参数（s1.1：4% 内边距、短边 <256 放大、JPEG）。
var CropParams = mediasvc.CropParams{Pad: 0.04, MinShortEdge: 256, Format: "jpeg", Quality: 90}

// CropDeleteConfidence crop-check 只删 ok=false 且 confidence ≥ 该值的切图。
const CropDeleteConfidence = 0.85

// LabelSep 细节 label 的「单品·部位」分隔符。
const LabelSep = "·"

// ValidNormBox C3：0≤x,y≤1，w,h>0，x+w≤1，y+h≤1。
// sidecar 把 x、w 分别四舍五入到 4 位小数，x+w 可能是 1.0001，所以右下边界容差 1e-3（BoxFromNorm 会再裁回图内）。
func ValidNormBox(b []float64) bool {
	if len(b) != 4 {
		return false
	}
	const eps = 1e-3
	x, y, w, h := b[0], b[1], b[2], b[3]
	return x >= 0 && y >= 0 && x <= 1 && y <= 1 && w > 0 && h > 0 && x+w <= 1+eps && y+h <= 1+eps
}

// NormalizeCropLabel C4：细节 label 必须带其单品前缀，不能串件。
//
//   - 已带前缀（「上衣·领口」「上装·领口」）：前缀归一为单品中文名；单件图前缀与该图单品不符 → 丢弃（ok=false）；
//     一套合影图前缀必须是 items 里的单品，否则丢弃。
//   - 不带前缀（keypoints / 兜底模式的「领口」）：单件图补「单品·」；一套合影图补「整套·」。
func NormalizeCropLabel(label, item string, items []string) (string, bool) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", false
	}
	prefix, rest, has := strings.Cut(label, LabelSep)
	if has {
		if k := ItemOf(prefix); k != "" && k != ItemLook {
			if strings.TrimSpace(rest) == "" {
				return "", false
			}
			if item == ItemLook {
				if !containsStr(items, k) {
					return "", false
				}
			} else if k != item {
				return "", false
			}
			return ItemLabels[k] + LabelSep + strings.TrimSpace(rest), true
		}
	}
	name := ItemLabels[item]
	if name == "" {
		return "", false
	}
	return name + LabelSep + label, true
}

// LabelItem 细节 label 的单品 key（前缀不认识返回空串）。
func LabelItem(label string) string {
	prefix, _, has := strings.Cut(label, LabelSep)
	if !has {
		return ""
	}
	return ItemOf(prefix)
}

// CropJob 对一张主图的切图任务：Poller 调 Media.Crop(Source.RID, Boxes, CropParams, idem)。
// Boxes[i].Label 已是规范 label；Confidence 与 Boxes 一一对应。
type CropJob struct {
	Source     Image
	Boxes      []mediasvc.PixelBox
	Confidence []float64
}

// BuildCropJobs crop-plan 结果 → 每张主图的像素框。
//
// sources 是发给 crop-plan 的主图（顺序同请求 images[]，需带 Width/Height）；plans 的 Index 已由 reverse.CropPlan 回映。
// 依次：C3 过滤非法框 → C4 label 前缀校验/补全 → 同图同 label 去重 → mediasvc.BoxFromNorm 换像素（Pad 交给 Media.Crop）。
// 返回的 warnings 只用于日志（不含图片数据）。
func BuildCropJobs(sources []Image, plans []reverse.CropPlan, items []string) ([]CropJob, []string) {
	var warns []string
	byIdx := map[int]*CropJob{}
	var order []int
	for _, p := range plans {
		if p.Index < 0 || p.Index >= len(sources) {
			warns = append(warns, fmt.Sprintf("crop-plan 下标越界 %d", p.Index))
			continue
		}
		src := sources[p.Index]
		if src.Width <= 0 || src.Height <= 0 {
			warns = append(warns, fmt.Sprintf("%s 缺少尺寸，跳过切图", src.Key))
			continue
		}
		job := byIdx[p.Index]
		if job == nil {
			job = &CropJob{Source: src}
			byIdx[p.Index] = job
			order = append(order, p.Index)
		}
		for _, c := range p.Crops {
			if !ValidNormBox(c.Box) {
				warns = append(warns, fmt.Sprintf("%s 丢弃非法框 %q %v", src.Key, c.Label, c.Box))
				continue
			}
			label, ok := NormalizeCropLabel(c.Label, src.Item, items)
			if !ok {
				warns = append(warns, fmt.Sprintf("%s 丢弃串件细节 %q", src.Key, c.Label))
				continue
			}
			if hasLabel(job.Boxes, label) {
				continue
			}
			pb := mediasvc.BoxFromNorm(c.Box[0], c.Box[1], c.Box[2], c.Box[3], src.Width, src.Height)
			if pb.W < 2 || pb.H < 2 {
				warns = append(warns, fmt.Sprintf("%s 丢弃过小的框 %q", src.Key, label))
				continue
			}
			pb.Label = label
			job.Boxes = append(job.Boxes, pb)
			job.Confidence = append(job.Confidence, c.Confidence)
		}
	}
	out := make([]CropJob, 0, len(order))
	for _, i := range order {
		if j := byIdx[i]; len(j.Boxes) > 0 {
			out = append(out, *j)
		}
	}
	return out, warns
}

func hasLabel(boxes []mediasvc.PixelBox, label string) bool {
	for _, b := range boxes {
		if b.Label == label {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// DetailImages 切图结果 → 细节图。res 与 job.Boxes 一一对应（Media.Crop 的返回）。
// Key = <主图 key>-c<n>（n 从 1 起），Item 取 label 前缀（一套合影的「整套·」细节为 look）。
func DetailImages(job CropJob, res []mediasvc.Resource) []Image {
	out := make([]Image, 0, len(res))
	for i, r := range res {
		if i >= len(job.Boxes) {
			break
		}
		label := job.Boxes[i].Label
		item := LabelItem(label)
		if item == "" {
			item = job.Source.Item
		}
		out = append(out, Image{
			Key: fmt.Sprintf("%s-c%d", job.Source.Key, i+1), Kind: ImageDetail, Item: item, Angle: job.Source.Angle,
			Role: RoleDetail, Label: label, Source: job.Source.Source, RID: r.RID, URL: r.URL,
			Width: r.Width, Height: r.Height,
		})
	}
	return out
}

// FilterCropChecks crop-check 结果过滤：只删 ok=false && confidence≥0.85；checkErr 非空时全部保留并返回告警（失败不阻断）。
// 返回保留的细节与告警。
func FilterCropChecks(details []Image, results []reverse.CropCheckResult, checkErr error) ([]Image, []string) {
	var warns []string
	if checkErr != nil {
		warns = append(warns, "crop-check 失败，保留全部切图："+checkErr.Error())
	}
	drop := map[int]bool{}
	for _, r := range results {
		if !r.OK && r.Confidence >= CropDeleteConfidence && r.Index >= 0 && r.Index < len(details) {
			drop[r.Index] = true
		}
	}
	out := make([]Image, 0, len(details))
	for i, d := range details {
		if drop[i] {
			warns = append(warns, fmt.Sprintf("crop-check 删除 %s %q", d.Key, d.Label))
			continue
		}
		out = append(out, d)
	}
	return out, warns
}
