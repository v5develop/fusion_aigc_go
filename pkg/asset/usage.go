package asset

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 用途与系统标签（sys_tags）——Go 侧唯一一份口径。
//
// 用途（Usage）与资产库分类 afv_asset_ext.kind 是同一组取值：用途是「进库时系统给的默认分类」，
// 以一条「用途:<中文名>」系统标签写进 afv_asset.sys_tags；资产库读取时 ext.kind 为空就按
// 用途标签 → entry_code（EntryUsage）→ type / 媒体形态 的顺序推算默认分类（见 app/assetlib/model/kind.go），
// 用户在资产库改分类才写 ext.kind。
//
// Java 侧的对应口径在 fusion-aigc 的 AssetSysTags（用途枚举、功能 → 用途、模板图片位 label → 用途），
// 两边的用途标签文本必须逐字一致；对照表见 docs/api/api-asset-library.md「系统标签与默认分类」。

// Usage 用途 = 资产库分类键（afv_asset_ext.kind）。
type Usage string

const (
	UsageModel        Usage = "model"         // 模特 / 人物
	UsageScene        Usage = "scene"         // 场景 / 环境
	UsageMakeup       Usage = "makeup"        // 妆容
	UsageHair         Usage = "hair"          // 发型
	UsageAccessory    Usage = "accessory"     // 配饰
	UsageProduct      Usage = "product"       // 商品
	UsageStorefront   Usage = "storefront"    // 门头 / 门店
	UsageDish         Usage = "dish"          // 菜品
	UsageLogo         Usage = "logo"          // Logo
	UsagePoster       Usage = "poster"        // 海报 / 营销图
	UsageVideoFinal   Usage = "video_final"   // 成片
	UsageVideoSegment Usage = "video_segment" // 视频片段
	UsageReference    Usage = "reference"     // 参考图
	UsageOther        Usage = "other"         // 其他
)

// Usages 全部用途（固定顺序：分类条、推算 CASE 都按它展开）。
var Usages = []Usage{
	UsageModel, UsageScene, UsageMakeup, UsageHair, UsageAccessory,
	UsageProduct, UsageStorefront, UsageDish, UsageLogo, UsagePoster, UsageReference, UsageOther,
	UsageVideoFinal, UsageVideoSegment,
}

// usageLabels 用途中文名（= 资产库分类名；用途标签 = "用途:" + 中文名）。
var usageLabels = map[Usage]string{
	UsageModel:        "模特",
	UsageScene:        "场景",
	UsageMakeup:       "妆容",
	UsageHair:         "发型",
	UsageAccessory:    "配饰",
	UsageProduct:      "商品",
	UsageStorefront:   "门头门店",
	UsageDish:         "菜品",
	UsageLogo:         "Logo",
	UsagePoster:       "海报",
	UsageReference:    "参考图",
	UsageOther:        "其他",
	UsageVideoFinal:   "成片",
	UsageVideoSegment: "视频片段",
}

// UsageTagPrefix 用途标签前缀。
const UsageTagPrefix = "用途:"

// Valid 是否已知用途。
func (u Usage) Valid() bool {
	_, ok := usageLabels[u]
	return ok
}

// Label 中文名；未知为空。
func (u Usage) Label() string { return usageLabels[u] }

// Tag 用途标签（如「用途:门头门店」）；未知用途为空。
func (u Usage) Tag() string {
	if l := usageLabels[u]; l != "" {
		return UsageTagPrefix + l
	}
	return ""
}

// UsageOfTag 用途标签 → 用途。
func UsageOfTag(tag string) (Usage, bool) {
	for _, u := range Usages {
		if u.Tag() == tag {
			return u, true
		}
	}
	return "", false
}

// EntryUsage 功能（entry_code）→ 默认用途：Go 写入方没有更具体的语义时按它取，资产库读取时
// 没有用途标签的老数据也按它推算。只登记「看入口就能确定用途」的；登记不到的再看 type / 媒体形态。
// 与 Java AssetSysTags.ENTRY_USAGE 同口径（Java 那份覆盖 Java 自己的入口）。
var EntryUsage = map[string]Usage{
	// studio 商拍
	"studio_tryon":    UsageModel,
	"studio_pose":     UsageModel,
	"studio_imagegen": UsageOther,
	// 画布
	"canvas_image":     UsageOther,
	"canvas_tool_crop": UsageOther,
	"canvas_video":     UsageVideoFinal,
	// 数字人
	"digitalHumanPortrait": UsageModel,
	"digitalHuman":         UsageVideoFinal,
	"voiceover":            UsageVideoFinal,
	// 本地生活 / 营销图
	"poster":        UsagePoster,
	"longImage":     UsagePoster,
	"imageTextNote": UsagePoster,
	"weTouch":       UsagePoster,
	// 视频
	"actionTransfer":  UsageVideoFinal,
	"exploreShop":     UsageVideoFinal,
	"shortDrama":      UsageVideoFinal,
	"videoRemake":     UsageVideoFinal,
	"video_replicate": UsageVideoFinal,
	// Java 电商 / 商拍入口（与 Java ENTRY_USAGE 对齐，保证没回填的老数据也能推算）
	"productSetImages":        UsageProduct,
	"wearingProductSetImages": UsageProduct,
	"aPlusDetailPage":         UsageProduct,
	"hitReplication":          UsageProduct,
	"removeWrinkles":          UsageProduct,
	"aiCutout":                UsageProduct,
	"ecomBatchImage":          UsageProduct,
	"aiExpert":                UsageModel,
	"aiClothExchange":         UsageModel,
	"changeClothes":           UsageModel,
	"mergeClothes":            UsageModel,
	"shoeSwapping":            UsageModel,
	"aiGestures":              UsageModel,
	"backgroundSwitching":     UsageModel,
	// creaft v1 兼容能力入口（Java 写，取值同 Java ENTRY_USAGE）
	"creaft.v1.tools.ai-product-replace":         UsageProduct,
	"creaft.v1.tools.ai-first-screen-main-image": UsageProduct,
	"creaft.v1.tools.ai-clothing-dewrinkle":      UsageProduct,
	"creaft.v1.workflows.ecommerce":              UsageProduct,
	"creaft.v1.tools.ai-model-tryon":             UsageModel,
	"creaft.v1.tools.ai-posture-fission":         UsageModel,
	"creaft.v1.workflows.fashion":                UsageModel,
	"creaft.v1.workflows.local-life":             UsagePoster,
	"creaft.v1.workflows.local-life-template":    UsagePoster,
	"creaft.v1.workflows.local-life.continue":    UsagePoster,
	"creaft.v1.tools.ai-background-generator":    UsageScene,
}

// TypeUsage afv_asset.type → 默认用途（入口也查不到时的最后一级；type=video 由媒体形态兜底，不在这里）。
var TypeUsage = map[string]Usage{
	"customizedModel": UsageModel,
	"realPersonModel": UsageModel,
}

// UsageFor 写入方的默认用途：入口表 → type 表 → 视频成片 → 其他。
func UsageFor(entryCode, typ string) Usage {
	if u, ok := EntryUsage[entryCode]; ok {
		return u
	}
	if u, ok := TypeUsage[typ]; ok {
		return u
	}
	if typ == "video" || strings.Contains(strings.ToLower(typ), "video") {
		return UsageVideoFinal
	}
	return UsageOther
}

// 系统标签的固定写法（文本即对外展示，改了要同步 Java 与文档）。
const (
	TagPartial = "部分完成"
	TagOverlay = "叠字版"
	TagPlain   = "无字版"
)

// 系统标签上限：单个 ≤32 字（与用户标签同宽），一条资产 ≤20 个。
const (
	SysTagMaxLen  = 32
	SysTagMaxSize = 20
)

// IndustryTag 「行业:餐饮」。
func IndustryTag(name string) string { return prefixed("行业:", name) }

// TemplateTag 「模板:夏日上新」。
func TemplateTag(name string) string { return prefixed("模板:", name) }

// SegmentTag 「第3段」。
func SegmentTag(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("第%d段", n)
}

var resolutionRe = regexp.MustCompile(`^(?i)([1-8]k|\d{3,4}p)$`)

// ResolutionTag 清晰度标签：只认 1K~8K / 720P / 1080P 这类写法，统一大写；其它（像素尺寸、档位名）不写。
func ResolutionTag(s string) string {
	s = strings.TrimSpace(s)
	if !resolutionRe.MatchString(s) {
		return ""
	}
	return strings.ToUpper(s)
}

func prefixed(p, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return p + name
}

// SysTags 一条资产的系统标签。
type SysTags struct {
	Feature    string   // 来源功能中文名（如「商拍换装」）
	Usage      Usage    // 用途（写成「用途:xxx」）
	Industry   string   // 行业名（不带前缀）
	Template   string   // 模板名（不带前缀）
	Resolution string   // 清晰度原值（ResolutionTag 规整）
	Extra      []string // 其它已成形的标签（第N段 / 部分完成 / 叠字版 …）
}

// List 规整后的标签列表：功能、用途、行业、模板、清晰度、其它；去空白、去重、单个截断 32 字、最多 20 个。
func (t SysTags) List() []string {
	raw := append([]string{t.Feature, t.Usage.Tag(), IndustryTag(t.Industry), TemplateTag(t.Template), ResolutionTag(t.Resolution)}, t.Extra...)
	return NormalizeSysTags(raw)
}

// NormalizeSysTags 去首尾空白、跳过空值、单个截断到 32 字、去重保序、最多 20 个。
func NormalizeSysTags(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > SysTagMaxLen {
			s = string([]rune(s)[:SysTagMaxLen])
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == SysTagMaxSize {
			break
		}
	}
	return out
}
