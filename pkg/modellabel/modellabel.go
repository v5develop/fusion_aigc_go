// Package modellabel 把内部模型组码 / 档位码映射成用户可见的友好名。
// 用户端不得出现厂商档位标记（Dev / Developer / Sunburst / Preview）、模型组码（img.* / vlm.* / llm.* / vid.*）。
package modellabel

import (
	"regexp"
	"strings"

	"aigc-local-life/pkg/modelgroup"
)

// groups model group → 用户端展示名（与 web MODEL_OPTIONS 一致）。
// 旧 Dev 组码（img.*-dev / *-developer）由 Group / Of 先经 pkg/modelgroup 归一到规范组再查，这里只登记规范组。
var groups = map[string]string{
	"img.gpt-image-2.5-sunburst": "GPT Image 2.5",
	"img.gpt-image-2.6-sunburst": "GPT Image 2.6",
	"img.gpt-image-2":            "GPT Image 2",
	"img.nano-banana-2.1":        "Nano Banana 2.1",
	"img.seedream-5.5":           "Seedream 5.5",
	"img.seedream-5-pro":         "Seedream 5 Pro",
	"vid.seedance-2.5":           "Seedance 2.5",
	"vid.seedance-2":             "Seedance 2.0",
	"vid.seedance-2-fast":        "Seedance 2.0 Fast",
	"vid.seedance-2-mini":        "Seedance 2.0 Mini",
}

// tiers 档位码 → 展示名（studio 模型档 / 计费档次，小写）。旧档次 gpt25dev 先经 pkg/modelgroup 归一。
var tiers = map[string]string{
	"gpt25":    "GPT Image 2.5",
	"gpt":      "GPT Image 2.6",
	"banana":   "Nano Banana 2.1",
	"seedream": "Seedream 5.5",
	"sd5p":     "Seedream 5.5",
	"image":    "GPT Image 2.5",
}

var (
	reTier   = regexp.MustCompile(`(?i)[\s-]*(sunburst[\s-]*)?(developer|dev|preview)\b`)
	reSunbur = regexp.MustCompile(`(?i)[\s-]*sunburst\b`)
	rePrefix = regexp.MustCompile(`(?i)^(img|vlm|llm|vid)\.`)
)

// Sanitize 去掉档位标记；组码形态只留尾部并转成可读名。
func Sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if rePrefix.MatchString(s) {
		s = rePrefix.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, "-", " ")
	}
	s = reTier.ReplaceAllString(s, "")
	s = reSunbur.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// Group 模型组码 → 展示名；没登记的清洗后回落，清洗后为空给「其他模型」。
func Group(group string) string {
	if l, ok := groups[strings.ToLower(modelgroup.Canonical(group))]; ok {
		return l
	}
	return Of(group)
}

// Of 任意模型值（档位码 / 组码 / 展示名）→ 用户可见名。
func Of(v string) string {
	t := strings.TrimSpace(v)
	if t == "" {
		return t
	}
	if l, ok := groups[strings.ToLower(modelgroup.Canonical(t))]; ok {
		return l
	}
	if l, ok := tiers[strings.ToLower(modelgroup.CanonicalTier(t))]; ok {
		return l
	}
	if out := Sanitize(t); out != "" {
		return out
	}
	return "其他模型"
}

// Registered 模型组码是否有登记的展示名。
func Registered(group string) (string, bool) {
	l, ok := groups[strings.ToLower(modelgroup.Canonical(group))]
	return l, ok
}
