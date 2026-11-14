package core

import (
	"strings"

	"aigc-local-life/app/replicate/rules/segment"
)

// RenderVersion 基础模板版本（未知模型回落它；四家方言各自的版本见 dialect.go，进 plan_hash）。
const RenderVersion = "base-1"

// RenderSegmentPrompt 基础模板渲染一段（参考式改写：原片该段作 @视频1，参考图按序 @图片1..n）。
func RenderSegmentPrompt(an *AnalysisResult, ann Annotations, refCount int, seg segment.Segment, total int) string {
	return RenderDialect(dialectBase, an, ann, refCount, seg, total)
}

func orText(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
