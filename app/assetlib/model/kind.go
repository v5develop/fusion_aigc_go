package model

import (
	"sort"
	"strings"

	"aigc-local-life/pkg/asset"
)

// 默认分类推算（读时推算，不回填 afv_asset_ext.kind）——Go 唯一一份推算表，与 Java 的口径对照见
// docs/api/api-asset-library.md「系统标签与默认分类」。
//
// 有效分类 = ext.kind（用户改过）> 用途标签（sys_tags 里的「用途:xxx」）> entry_code（asset.EntryUsage）
// > type（asset.TypeUsage）> 媒体形态是视频 → 成片 > 未分类（NULL）。
// 推算只靠 SQL 表达式完成，列表筛选 / 分类计数 / 后台统计与返回值共用同一条表达式，口径不会分叉。

// sqlStr 常量 → SQL 字符串字面量（只用于本文件里的固定映射，不接用户输入）。
func sqlStr(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// AutoKindExpr 不含 ext.kind 的推算表达式（SQL CASE），videoCond 为媒体形态是视频的判据（可带 ? 占位，参数由调用方给）。
// 结果为 NULL 表示推算不出（= 未分类）。
func AutoKindExpr(videoCond string) string {
	var b strings.Builder
	b.WriteString("(CASE")
	// 1. 用途标签：按 Usages 顺序，第一个命中的为准
	for _, u := range asset.Usages {
		b.WriteString(" WHEN JSON_CONTAINS(a.sys_tags, " + sqlStr(`"`+u.Tag()+`"`) + ") THEN " + sqlStr(string(u)))
	}
	// 2. 入口码（按用途分组，组内入口码排序保证 SQL 稳定）
	for _, u := range asset.Usages {
		if codes := keysOf(asset.EntryUsage, u); len(codes) > 0 {
			b.WriteString(" WHEN a.entry_code IN (" + joinStr(codes) + ") THEN " + sqlStr(string(u)))
		}
	}
	// 3. 类型
	for _, u := range asset.Usages {
		if types := keysOf(asset.TypeUsage, u); len(types) > 0 {
			b.WriteString(" WHEN a.type IN (" + joinStr(types) + ") THEN " + sqlStr(string(u)))
		}
	}
	// 4. 视频兜底为成片
	if videoCond != "" {
		b.WriteString(" WHEN " + videoCond + " THEN " + sqlStr(string(asset.UsageVideoFinal)))
	}
	b.WriteString(" ELSE NULL END)")
	return b.String()
}

// EffectiveKindExpr 有效分类：COALESCE(ext.kind, 推算)。
func EffectiveKindExpr(videoCond string) string {
	return "COALESCE(e.kind, " + AutoKindExpr(videoCond) + ")"
}

// InferKind Go 侧同口径的推算（单测对照 SQL、以及不经库的场景用）。isVideo 为媒体形态判定结果。
func InferKind(sysTags []string, entryCode, typ string, isVideo bool) string {
	for _, u := range asset.Usages {
		for _, t := range sysTags {
			if t == u.Tag() {
				return string(u)
			}
		}
	}
	if u, ok := asset.EntryUsage[entryCode]; ok {
		return string(u)
	}
	if u, ok := asset.TypeUsage[typ]; ok {
		return string(u)
	}
	if isVideo {
		return string(asset.UsageVideoFinal)
	}
	return ""
}

func keysOf(m map[string]asset.Usage, u asset.Usage) []string {
	var out []string
	for k, v := range m {
		if v == u {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func joinStr(vals []string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = sqlStr(v)
	}
	return strings.Join(parts, ", ")
}
