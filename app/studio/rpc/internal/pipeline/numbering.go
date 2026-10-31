// Package pipeline studio 链路（商拍换装 / 姿态复刻 / 图片生成）的编排纯逻辑：编号、切图框、组图、
// 提示词图序、gateway 请求构造、缓存键与步骤接口。不依赖数据库，不 import repo / svc / logic；
// 外部服务（媒体服务、反推 sidecar、ai-gateway）只经 steps.go 里的小接口进入。
package pipeline

import (
	"fmt"
	"sort"
	"strings"
)

// 单品 key（与 sidecar ITEM_LABELS、logic/validate.go 一致）。
const (
	ItemTop       = "top"
	ItemBottom    = "bottom"
	ItemDress     = "dress"
	ItemOuter     = "outer"
	ItemShoes     = "shoes"
	ItemAccessory = "accessory"
	ItemBra       = "bra"
	ItemPanties   = "panties"
	// ItemLook 一套合影（role='整套'）：一张图里同时有多件单品，不属于任何单件。
	ItemLook = "look"
)

// CategoryCompose 多件单品时的 category（sidecar 的组合替换）。
const CategoryCompose = "compose"

// RoleLook 一套合影的 role。
const RoleLook = "整套"

// ItemLabels 单品中文名（也是 crop 细节 label 的前缀、反推 images[].role）。
var ItemLabels = map[string]string{
	ItemTop: "上衣", ItemBottom: "下装", ItemDress: "连衣裙", ItemOuter: "外套",
	ItemShoes: "鞋子", ItemAccessory: "配饰", ItemBra: "文胸", ItemPanties: "内裤",
	ItemLook: RoleLook,
}

// itemAliases role / label 前缀 → 单品 key（含 sidecar 合影候选表里的「上装」「裤子/裙子」等写法）。
var itemAliases = map[string]string{
	"上衣": ItemTop, "上装": ItemTop, "top": ItemTop,
	"下装": ItemBottom, "裤子": ItemBottom, "裙子": ItemBottom, "裤子/裙子": ItemBottom, "半身裙": ItemBottom, "bottom": ItemBottom,
	"连衣裙": ItemDress, "dress": ItemDress,
	"外套": ItemOuter, "outer": ItemOuter,
	"鞋子": ItemShoes, "鞋": ItemShoes, "shoes": ItemShoes,
	"配饰": ItemAccessory, "accessory": ItemAccessory,
	"文胸": ItemBra, "bra": ItemBra,
	"内裤": ItemPanties, "panties": ItemPanties, "panty": ItemPanties,
	"整套": ItemLook, "整套穿搭": ItemLook, "套装": ItemLook, "look": ItemLook,
}

// ItemOf role 或前缀 → 单品 key；不认识返回空串。
func ItemOf(s string) string {
	s = strings.TrimSpace(s)
	if k, ok := itemAliases[s]; ok {
		return k
	}
	return itemAliases[strings.ToLower(s)]
}

// Angles 角度固定顺序（编号与组图都按它排）。
var Angles = []string{"front", "back", "side", "detail", "flat", "unknown"}

func angleRank(a string) int {
	for i, v := range Angles {
		if v == a {
			return i
		}
	}
	return len(Angles) - 1
}

// NormalizeAngle 小写，空 / 未知 → unknown。
func NormalizeAngle(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	for _, v := range Angles {
		if v == a {
			return a
		}
	}
	return "unknown"
}

// 批次种类（afv_studio_batch.kind）。
const (
	KindTryon    = "tryon"
	KindPose     = "pose"
	KindImageGen = "imagegen"
)

// 图片种类（Image.Kind）。
const (
	ImageGarment = "garment" // 服装主图（含一套合影）
	ImageDetail  = "detail"  // 切出来的局部细节
	ImageModel   = "model"   // 模特 / 姿态原图
	ImagePose    = "pose"    // 姿势参考图
	ImageRef     = "ref"     // 图片生成的参考图
)

// sidecar 反推的 role。
const (
	RoleDetail = "局部细节"
	RoleModel  = "人物模特"
)

// Image 编排里流转的一张图。Data 是临时的 data URL，只在调 sidecar 时填，绝不入库、不写日志。
type Image struct {
	Key    string `json:"key"`              // g-top-front-1 / g-top-front-1-c2（细节）/ m-1
	Kind   string `json:"kind"`             // garment / detail / model / pose
	Item   string `json:"item,omitempty"`   // 单品 key；一套合影为 look
	Angle  string `json:"angle,omitempty"`  // 主图角度
	Role   string `json:"role"`             // sidecar role：单品中文名 / 整套 / 局部细节 / 人物模特
	Label  string `json:"label,omitempty"`  // 细节 label（带「单品·」前缀）
	Source int    `json:"source"`           // 提交时的下标（服装图 / 模特图各自从 0 起）
	URL    string `json:"url,omitempty"`    // 输入地址（或现签地址）
	RID    string `json:"rid,omitempty"`    // 媒体服务资源
	SHA1   string `json:"sha1,omitempty"`   // 内容 sha1（去重 / 缓存键）
	SHA256 string `json:"sha256,omitempty"` // 媒体服务给的内容 sha256（随 mr:// 发给 gateway）
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Data   string `json:"-"`
}

// GarmentInput 提交的一张服装图（studio.GarmentImage 的子集，避免依赖 pb）。
type GarmentInput struct {
	URL   string
	Role  string // 单品中文名 / 英文 key / 整套
	Angle string
}

// GarmentKey g-<item>-<angle>-<seq>，seq 从 1 起。
func GarmentKey(item, angle string, seq int) string {
	return fmt.Sprintf("g-%s-%s-%d", item, angle, seq)
}

// ModelKey m-<i>，i 为 0 起的模特下标，编号从 1 起（m-1 = ModelIndex 0）。
func ModelKey(i int) string { return fmt.Sprintf("m-%d", i+1) }

// NumberGarments 给服装图编号并排序：单品按 items 顺序（一套合影 look 排在所有单件之后），
// 同一单品内角度 front→back→side→detail→flat→unknown，同角度按提交顺序，seq 从 1 起。
//
// role 解析：认中文名 / 英文 key / 整套；空或不认识时，单件提交归到 items[0]，多件归到 look 并告警；
// 归到不在 items 里的单品的图丢弃并告警。
func NumberGarments(items []string, garments []GarmentInput) ([]Image, []string) {
	var warns []string
	rank := map[string]int{}
	for i, it := range items {
		rank[it] = i
	}
	rank[ItemLook] = len(items)
	out := make([]Image, 0, len(garments))
	for i, g := range garments {
		item := ItemOf(g.Role)
		switch {
		case item == "" && len(items) == 1:
			item = items[0]
		case item == "":
			item = ItemLook
			warns = append(warns, fmt.Sprintf("服装图第 %d 张单品未识别（%q），按整套处理", i+1, g.Role))
		case item != ItemLook:
			if _, ok := rank[item]; !ok {
				warns = append(warns, fmt.Sprintf("服装图第 %d 张是%s，不在所选单品里，已忽略", i+1, ItemLabels[item]))
				continue
			}
		}
		out = append(out, Image{
			Kind: ImageGarment, Item: item, Angle: NormalizeAngle(g.Angle), Role: ItemLabels[item],
			Source: i, URL: strings.TrimSpace(g.URL),
		})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if ra, rb := rank[out[a].Item], rank[out[b].Item]; ra != rb {
			return ra < rb
		}
		if ra, rb := angleRank(out[a].Angle), angleRank(out[b].Angle); ra != rb {
			return ra < rb
		}
		return out[a].Source < out[b].Source
	})
	seq := map[string]int{}
	for i := range out {
		k := out[i].Item + "/" + out[i].Angle
		seq[k]++
		out[i].Key = GarmentKey(out[i].Item, out[i].Angle, seq[k])
	}
	return out, warns
}

// NumberModels 模特图编号 m-1..m-n。
func NumberModels(urls []string) []Image {
	out := make([]Image, len(urls))
	for i, u := range urls {
		out[i] = Image{Key: ModelKey(i), Kind: ImageModel, Role: RoleModel, Source: i, URL: strings.TrimSpace(u)}
	}
	return out
}

// FilterByItems lengths / sleeves 只保留 items 里的键（去空白，空值丢弃）。
func FilterByItems(m map[string]string, items []string) map[string]string {
	out := map[string]string{}
	for _, it := range items {
		if v := strings.TrimSpace(m[it]); v != "" {
			out[it] = v
		}
	}
	return out
}

// InferCategory 单件 = items[0]，多件 = compose。
func InferCategory(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return CategoryCompose
}
