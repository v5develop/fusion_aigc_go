// Package modelgroup 模型组码 / 计费档次（tier）的规范名与旧名等价表，单一来源。
//
// 2026-11-10 起应用侧（Java / Go / web）不再区分厂商的 Dev / Developer 档：网关自己挑部署，
// 规范组 img.nano-banana-2.1 / img.gpt-image-2.5-sunburst 已路由到原 Dev 部署，旧 Dev 组码在网关里留作弃用别名。
// 应用侧对外（提交、冻结、目录、文案）一律用规范名；读到旧名（换代前入库的任务、重做 / 重试 / 结算、
// 迁移前的 Java 计费库）时按等价处理，所以 Go 与 Java 迁移 V1.0.221 没有上线先后约束。
//
// 本包只放这张等价表；所有比较模型组 / tier 字符串的地方都走这里，不要另写一份。
package modelgroup

import (
	"slices"
	"strings"
)

// 规范模型组码。
const (
	NanoBanana21       = "img.nano-banana-2.1"
	GPTImage25Sunburst = "img.gpt-image-2.5-sunburst"
	// GPTImage25SunburstStd GPT Image 2.5 标准版（openai/gpt-image-2.5-sunburst，按 token 计费、支持画质；2026-11-19 画布启用）。
	// 与 Dev 版（按张计费）是两个独立的组，不在等价表里。
	GPTImage25SunburstStd = "img.gpt-image-2.5-sunburst-std"
)

// 规范计费档次（afv_billing_offer_variant.quality_tier / <offer_code>_<tier> 档位码后缀）。
const TierGPT25 = "gpt25"

// legacyGroups 旧组码（小写）→ 规范组码。只增不删：历史任务与旧草稿永久按它读。
var legacyGroups = map[string]string{
	"img.nano-banana-2.1-dev":              NanoBanana21,
	"img.gpt-image-2.5-sunburst-developer": GPTImage25Sunburst,
}

// legacyTiers 旧档次（小写）→ 规范档次。
var legacyTiers = map[string]string{
	"gpt25dev": TierGPT25,
}

// Canonical 模型组码 → 规范组码；不在等价表里的原样返回（去首尾空白）。
func Canonical(group string) string {
	g := strings.TrimSpace(group)
	l := strings.ToLower(g)
	if c, ok := legacyGroups[l]; ok {
		return c
	}
	for _, c := range legacyGroups { // 规范组码大小写写法不同也归一（便于 Aliases 带出旧组）
		if c == l {
			return c
		}
	}
	return g
}

// Aliases 与 group 等价的全部组码，规范组码在第一位（查库时按顺序优先）；空串返回 nil。
func Aliases(group string) []string {
	c := Canonical(group)
	if c == "" {
		return nil
	}
	return withLegacy(c, legacyGroups)
}

// Equal 两个组码是否等价（规范名与旧名视为同一组）。
func Equal(a, b string) bool {
	return strings.EqualFold(Canonical(a), Canonical(b))
}

// Is group 是否（等价于）规范组 canonical。
func Is(group, canonical string) bool { return Equal(group, canonical) }

// CanonicalTier 档次 → 规范档次（小写比较）；不在等价表里的原样返回（去首尾空白）。
func CanonicalTier(tier string) string {
	t := strings.TrimSpace(tier)
	if c, ok := legacyTiers[strings.ToLower(t)]; ok {
		return c
	}
	return t
}

// TierAliases 与 tier 等价的全部档次，规范档次在第一位；空串返回 nil。
func TierAliases(tier string) []string {
	c := CanonicalTier(tier)
	if c == "" {
		return nil
	}
	return withLegacy(c, legacyTiers)
}

// withLegacy 规范值在前，其后是映射到它的旧值（按字典序，map 遍历无序，排序保证查库参数与日志稳定）。
func withLegacy(canonical string, legacy map[string]string) []string {
	var olds []string
	for old, canon := range legacy {
		if canon == canonical {
			olds = append(olds, old)
		}
	}
	slices.Sort(olds)
	return append([]string{canonical}, olds...)
}
