package logic

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/xerr"
)

// 服务端复查的上限（s1.1 验收 + 03 前端包 B 节）。前端也有同样的校验，这里是兜底，口径必须一致。
const (
	MaxItems          = 3    // 单品最多 3 件
	MaxGarmentImages  = 9    // 服装图最多 9 张
	MaxImagesPerItem  = 6    // 每件单品最多 6 个角度槽
	MaxModels         = 9    // 模特 1–9
	MaxCountPerModel  = 9    // 每位模特张数 1–9
	MaxPoses          = 9    // 姿势 1–9
	MaxImageGenCount  = 9    // 图片生成张数 1–9
	MaxImageGenRefs   = 9    // 图片生成参考图最多 9 张
	MaxPromptLen      = 3000 // 提示词 / 补充说明（按字符）
	MaxRequestIDLen   = 64   // 与 afv_studio_batch.request_id 一致
	MaxURLLen         = 1024 // 与 result_url 等列一致
	DefaultResolution = "1k"
	// DefaultAspectRatio 商拍 / 姿态不传比例时留空 = 自动：编排时按模特图 / 人物图的宽高取最近的标准档
	// （pipeline.NearestAspectRatio），并显式传给 gateway；实际用的比例写在子任务 prompt_meta.gen。
	DefaultAspectRatio = ""
	// DefaultImageGenAspectRatio 图片生成默认 1:1（与原型一致）
	DefaultImageGenAspectRatio = "1:1"
)

// 单品 key（items[] 的取值）。
const (
	ItemTop       = "top"       // 上衣
	ItemBottom    = "bottom"    // 下装
	ItemDress     = "dress"     // 连衣裙
	ItemOuter     = "outer"     // 外套
	ItemShoes     = "shoes"     // 鞋子
	ItemAccessory = "accessory" // 配饰
	ItemBra       = "bra"       // 文胸
	ItemPanties   = "panties"   // 内裤
)

var knownItems = map[string]bool{
	ItemTop: true, ItemBottom: true, ItemDress: true, ItemOuter: true,
	ItemShoes: true, ItemAccessory: true, ItemBra: true, ItemPanties: true,
}

// itemExclusive 互斥的单品对：裙↔上衣/下装；文胸↔上衣/裙；内裤↔下装/裙。
var itemExclusive = [][2]string{
	{ItemDress, ItemTop}, {ItemDress, ItemBottom},
	{ItemBra, ItemTop}, {ItemBra, ItemDress},
	{ItemPanties, ItemBottom}, {ItemPanties, ItemDress},
}

var itemLabel = map[string]string{
	ItemTop: "上衣", ItemBottom: "下装", ItemDress: "连衣裙", ItemOuter: "外套",
	ItemShoes: "鞋子", ItemAccessory: "配饰", ItemBra: "文胸", ItemPanties: "内裤",
}

// 角度：固定顺序 front→back→side→detail→flat→unknown（编号与组图都按它排）。
var Angles = []string{"front", "back", "side", "detail", "flat", "unknown"}

var knownAngles = func() map[string]bool {
	m := map[string]bool{}
	for _, a := range Angles {
		m[a] = true
	}
	return m
}()

var knownResolutions = map[string]bool{"1k": true, "2k": true, "4k": true}

var aspectRatioRe = regexp.MustCompile(`^[1-9][0-9]{0,2}:[1-9][0-9]{0,2}$`)

// ---------------- 通用 ----------------

func checkUserRequest(userID int64, requestID string) error {
	if userID <= 0 {
		return xerr.New(xerr.ParamMissing, "缺少用户")
	}
	rid := strings.TrimSpace(requestID)
	if rid == "" {
		return xerr.New(xerr.ParamMissing, "缺少幂等键（Idempotency-Key）")
	}
	if len(rid) > MaxRequestIDLen {
		return xerr.Param("幂等键过长")
	}
	return nil
}

// checkURL 只收 http(s) 绝对地址：blob:（前端还没上传完）、data:、file: 等一律拒绝。
func checkURL(what, raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return xerr.New(xerr.ParamMissing, what+"缺少地址")
	}
	if len(s) > MaxURLLen {
		return xerr.Param(what + "地址过长")
	}
	if strings.HasPrefix(strings.ToLower(s), "blob:") {
		return xerr.Param(what + "还没上传完成，请等上传结束再提交")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return xerr.Param(what + "地址不合法，只支持 http(s) 地址")
	}
	return nil
}

func checkURLs(what string, list []string) error {
	for i, u := range list {
		if err := checkURL(fmt.Sprintf("%s第 %d 张", what, i+1), u); err != nil {
			return err
		}
	}
	return nil
}

func checkPromptLen(what, s string) error {
	if utf8.RuneCountInString(s) > MaxPromptLen {
		return xerr.Param(fmt.Sprintf("%s不能超过 %d 字", what, MaxPromptLen))
	}
	return nil
}

// NormalizeResolution 分辨率大小写归一：1K / 1k → 1k；空 → 1k。
func NormalizeResolution(s string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(s))
	if r == "" {
		return DefaultResolution, nil
	}
	if !knownResolutions[r] {
		return "", xerr.Param("分辨率只支持 1k / 2k / 4k")
	}
	return r, nil
}

// NormalizeAspectRatio 比例 W:H；空用默认。
func NormalizeAspectRatio(s, def string) (string, error) {
	r := strings.TrimSpace(s)
	if r == "" {
		return def, nil
	}
	if !aspectRatioRe.MatchString(r) {
		return "", xerr.Param("比例格式不对，应为 宽:高，如 3:4")
	}
	return r, nil
}

// NormalizeModel 前端模型档 → 档名（空=通用）与 gateway model group。groups 为空时不校验档名（仅单测用）。
func NormalizeModel(s string, groups map[string]string) (name, group string, err error) {
	name = strings.TrimSpace(s)
	if name == "" {
		name = "通用"
	}
	if len(groups) == 0 {
		return name, "", nil
	}
	if g, ok := groups[name]; ok {
		return name, g, nil
	}
	// 大小写不敏感（gpt / GPT）
	for k, g := range groups {
		if strings.EqualFold(k, name) {
			return k, g, nil
		}
	}
	return "", "", xerr.Param("不支持的模型：" + name)
}

// ---------------- 商拍换装 ----------------

// TryonInput 校验并归一后的商拍入参。
type TryonInput struct {
	Items         []string
	Garments      []*studio.GarmentImage // angle 已归一
	Models        []string
	SetMode       string
	Lengths       map[string]string // 只留 items 里有的键
	Sleeves       map[string]string
	Resolution    string
	AspectRatio   string
	ModelName     string
	ModelGroup    string
	CountPerModel int32
	Warnings      []string // 分辨率按 model group 归一的告警
}

// CheckItems 单品：1–3 件、取值已知、不重复、互斥。
func CheckItems(items []string) ([]string, error) {
	if len(items) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "请选择单品")
	}
	if len(items) > MaxItems {
		return nil, xerr.Param(fmt.Sprintf("单品最多 %d 件", MaxItems))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, it := range items {
		k := strings.ToLower(strings.TrimSpace(it))
		if !knownItems[k] {
			return nil, xerr.Param("未知的单品：" + it)
		}
		if seen[k] {
			return nil, xerr.Param("单品重复：" + itemLabel[k])
		}
		seen[k] = true
		out = append(out, k)
	}
	for _, p := range itemExclusive {
		if seen[p[0]] && seen[p[1]] {
			return nil, xerr.Param(fmt.Sprintf("%s和%s不能同时选择", itemLabel[p[0]], itemLabel[p[1]]))
		}
	}
	return out, nil
}

// CheckGarments 服装图：1–9 张、每张 http(s)、角度枚举（空=unknown）、同一单品（role）≤6 张。
func CheckGarments(list []*studio.GarmentImage) ([]*studio.GarmentImage, error) {
	if len(list) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "请上传服装图")
	}
	if len(list) > MaxGarmentImages {
		return nil, xerr.Param(fmt.Sprintf("服装图最多 %d 张", MaxGarmentImages))
	}
	perRole := map[string]int{}
	out := make([]*studio.GarmentImage, 0, len(list))
	for i, g := range list {
		if g == nil {
			return nil, xerr.Param(fmt.Sprintf("服装图第 %d 张为空", i+1))
		}
		if err := checkURL(fmt.Sprintf("服装图第 %d 张", i+1), g.Url); err != nil {
			return nil, err
		}
		angle := strings.ToLower(strings.TrimSpace(g.Angle))
		if angle == "" {
			angle = "unknown"
		}
		if !knownAngles[angle] {
			return nil, xerr.Param("服装图角度只支持 front / back / side / detail / flat / unknown")
		}
		role := strings.TrimSpace(g.Role)
		perRole[role]++
		if perRole[role] > MaxImagesPerItem {
			name := role
			if name == "" {
				name = "同一单品"
			}
			return nil, xerr.Param(fmt.Sprintf("%s最多 %d 张图", name, MaxImagesPerItem))
		}
		out = append(out, &studio.GarmentImage{Url: strings.TrimSpace(g.Url), Role: role, Angle: angle})
	}
	return out, nil
}

// filterByItems lengths / sleeves 只认 items 里有的件（前端会带历史 key）。
func filterByItems(m map[string]string, items []string) map[string]string {
	out := map[string]string{}
	for _, it := range items {
		if v, ok := m[it]; ok && strings.TrimSpace(v) != "" {
			out[it] = strings.TrimSpace(v)
		}
	}
	return out
}

// ValidateTryon 校验顺序与前端一致：单品 → 服装图 → 模特 → 其余。
func ValidateTryon(in *studio.SubmitTryonReq, groups map[string]string) (*TryonInput, error) {
	if err := checkUserRequest(in.UserId, in.RequestId); err != nil {
		return nil, err
	}
	items, err := CheckItems(in.Items)
	if err != nil {
		return nil, err
	}
	garments := in.GarmentImages
	if len(garments) == 0 && strings.TrimSpace(in.ClothImageUrl) != "" {
		// 旧版兼容位：只有首张服装图
		garments = []*studio.GarmentImage{{Url: in.ClothImageUrl, Role: itemLabel[items[0]], Angle: "front"}}
	}
	gs, err := CheckGarments(garments)
	if err != nil {
		return nil, err
	}
	if len(in.ModelImageUrls) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "请上传模特图")
	}
	if len(in.ModelImageUrls) > MaxModels {
		return nil, xerr.Param(fmt.Sprintf("模特最多 %d 位", MaxModels))
	}
	if err := checkURLs("模特图", in.ModelImageUrls); err != nil {
		return nil, err
	}
	if in.CountPerModel < 1 || in.CountPerModel > MaxCountPerModel {
		return nil, xerr.Param(fmt.Sprintf("每位模特生成张数为 1–%d", MaxCountPerModel))
	}
	setMode := strings.ToLower(strings.TrimSpace(in.SetMode))
	switch setMode {
	case "":
		setMode = "one"
	case "one", "two":
	default:
		return nil, xerr.Param("成套方式只支持 one / two")
	}
	if err := checkPromptLen("补充说明", in.Prompt); err != nil {
		return nil, err
	}
	if err := checkPromptLen("面料", in.Fabric); err != nil {
		return nil, err
	}
	res, err := NormalizeResolution(in.Resolution)
	if err != nil {
		return nil, err
	}
	ar, err := NormalizeAspectRatio(in.AspectRatio, DefaultAspectRatio)
	if err != nil {
		return nil, err
	}
	name, group, err := NormalizeModel(in.Model, groups)
	if err != nil {
		return nil, err
	}
	// 按 model group 归一分辨率（Seedream 4K → 2K、1K 非 1:1 → 2K）：快照、计费档位、生图参数都用归一值
	res, warns, err := pipeline.NormalizeForGroup(group, res, ar)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(in.ModelImageUrls))
	for _, u := range in.ModelImageUrls {
		models = append(models, strings.TrimSpace(u))
	}
	return &TryonInput{
		Items: items, Garments: gs, Models: models, SetMode: setMode,
		Lengths: filterByItems(in.Lengths, items), Sleeves: filterByItems(in.Sleeves, items),
		Resolution: res, AspectRatio: ar, ModelName: name, ModelGroup: group, CountPerModel: in.CountPerModel, Warnings: warns,
	}, nil
}

// ---------------- 姿态复刻 ----------------

// PoseInput 校验并归一后的姿态入参。
type PoseInput struct {
	Source      string
	Poses       []string
	PosePrompts map[int32]string
	// SourcePersonCount / Mappings 多人配对（PreviewPose 确认后带回）；没传 mappings 时为 0 / nil
	SourcePersonCount int
	Mappings          []PoseMappingInput // 按 pose_index 升序
	Resolution        string
	AspectRatio       string
	ModelName         string
	ModelGroup        string
	Warnings          []string
}

func ValidatePose(in *studio.SubmitPoseReq, groups map[string]string) (*PoseInput, error) {
	if err := checkUserRequest(in.UserId, in.RequestId); err != nil {
		return nil, err
	}
	if err := checkURL("人物图", in.SourceImageUrl); err != nil {
		return nil, err
	}
	if len(in.PoseImageUrls) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "请上传姿势图")
	}
	if len(in.PoseImageUrls) > MaxPoses {
		return nil, xerr.Param(fmt.Sprintf("姿势最多 %d 张", MaxPoses))
	}
	if err := checkURLs("姿势图", in.PoseImageUrls); err != nil {
		return nil, err
	}
	if err := checkPromptLen("补充说明", in.GlobalPrompt); err != nil {
		return nil, err
	}
	prompts := map[int32]string{}
	for idx, p := range in.PosePrompts {
		if idx < 0 || int(idx) >= len(in.PoseImageUrls) {
			return nil, xerr.Param(fmt.Sprintf("单张说明的姿势序号 %d 超出范围", idx))
		}
		if err := checkPromptLen(fmt.Sprintf("姿势 %d 的说明", idx+1), p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p) != "" {
			prompts[idx] = p
		}
	}
	res, err := NormalizeResolution(in.Resolution)
	if err != nil {
		return nil, err
	}
	ar, err := NormalizeAspectRatio(in.AspectRatio, DefaultAspectRatio)
	if err != nil {
		return nil, err
	}
	name, group, err := NormalizeModel(in.Model, groups)
	if err != nil {
		return nil, err
	}
	// 按 model group 归一分辨率（Seedream 4K → 2K、1K 非 1:1 → 2K）：快照、计费档位、生图参数都用归一值
	res, warns, err := pipeline.NormalizeForGroup(group, res, ar)
	if err != nil {
		return nil, err
	}
	mappings, err := validatePoseMappings(in.SourcePersonCount, in.Mappings, len(in.PoseImageUrls))
	if err != nil {
		return nil, err
	}
	srcCount := 0
	if len(mappings) > 0 {
		srcCount = int(in.SourcePersonCount)
	}
	poses := make([]string, 0, len(in.PoseImageUrls))
	for _, u := range in.PoseImageUrls {
		poses = append(poses, strings.TrimSpace(u))
	}
	return &PoseInput{
		Source: strings.TrimSpace(in.SourceImageUrl), Poses: poses, PosePrompts: prompts,
		SourcePersonCount: srcCount, Mappings: mappings,
		Resolution: res, AspectRatio: ar, ModelName: name, ModelGroup: group, Warnings: warns,
	}, nil
}

// PoseMappingInput 一张姿势图的配对（校验后）。JSON 键名与批次 payload / 前端提交一致。
type PoseMappingInput struct {
	PoseIndex       int   `json:"pose_index"`
	Mapping         []int `json:"mapping"`
	PosePersonCount int   `json:"pose_person_count"`
}

// validatePoseMappings 多人配对校验（人数来自 PreviewPose，付费前由用户确认）：
// 原图 2–4 人；每条 pose_index 在范围内且不重复；len(mapping) == 原图人数；值在 [-1, 姿势图人数-1]；
// 非负值不重复（sidecar 会把重复值改成 -1）；至少一个非负值（全 -1 sidecar 会退回默认配对，等于没确认）。
// 编排时若 sidecar 本次识别的人数与预览不同，配对会被放弃并记告警（见 resolvePoseMapping）。
func validatePoseMappings(srcCount int32, list []*studio.PoseMapping, poses int) ([]PoseMappingInput, error) {
	if len(list) == 0 {
		return nil, nil
	}
	if srcCount < 2 || srcCount > reverse.MaxPosePeople {
		return nil, xerr.Param(fmt.Sprintf("人物配对需要原图人数（2–%d），请先预览配对", reverse.MaxPosePeople))
	}
	if len(list) > poses {
		return nil, xerr.Param("人物配对条数多于姿势图张数")
	}
	out := make([]PoseMappingInput, 0, len(list))
	seen := map[int32]bool{}
	for _, m := range list {
		if m == nil {
			continue
		}
		idx := m.PoseIndex
		if idx < 0 || int(idx) >= poses {
			return nil, xerr.Param(fmt.Sprintf("人物配对的姿势序号 %d 超出范围", idx))
		}
		if seen[idx] {
			return nil, xerr.Param(fmt.Sprintf("姿势 %d 的人物配对重复", idx+1))
		}
		seen[idx] = true
		what := fmt.Sprintf("姿势 %d 的人物配对", idx+1)
		ppc := m.PosePersonCount
		if ppc < 1 || ppc > reverse.MaxPosePeople {
			return nil, xerr.Param(fmt.Sprintf("%s缺少姿势图人数（1–%d），请先预览配对", what, reverse.MaxPosePeople))
		}
		if len(m.Mapping) != int(srcCount) {
			return nil, xerr.Param(fmt.Sprintf("%s应为 %d 位（与原图人数一致），实际 %d 位", what, srcCount, len(m.Mapping)))
		}
		vals := make([]int, len(m.Mapping))
		used := map[int32]bool{}
		hasPose := false
		for i, v := range m.Mapping {
			if v < -1 || v >= ppc {
				return nil, xerr.Param(fmt.Sprintf("%s第 %d 位取值 %d 超出范围（-1 到 %d）", what, i+1, v, ppc-1))
			}
			if v >= 0 {
				if used[v] {
					return nil, xerr.Param(fmt.Sprintf("%s里姿势图人物 %d 被重复使用", what, v+1))
				}
				used[v], hasPose = true, true
			}
			vals[i] = int(v)
		}
		if !hasPose {
			return nil, xerr.Param(what + "至少要有一位人物学姿势")
		}
		out = append(out, PoseMappingInput{PoseIndex: int(idx), Mapping: vals, PosePersonCount: int(ppc)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PoseIndex < out[j].PoseIndex })
	return out, nil
}

// ---------------- 图片生成 ----------------

// ImageGenInput 校验并归一后的图片生成入参。
type ImageGenInput struct {
	Prompt      string
	Images      []string
	Count       int32
	Resolution  string
	AspectRatio string
	ModelName   string
	ModelGroup  string
	Warnings    []string
}

func ValidateImageGen(in *studio.SubmitImageGenReq, groups map[string]string) (*ImageGenInput, error) {
	if err := checkUserRequest(in.UserId, in.RequestId); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, xerr.New(xerr.ParamMissing, "请输入提示词")
	}
	if err := checkPromptLen("提示词", in.Prompt); err != nil {
		return nil, err
	}
	if len(in.ImageUrls) > MaxImageGenRefs {
		return nil, xerr.Param(fmt.Sprintf("参考图最多 %d 张", MaxImageGenRefs))
	}
	if err := checkURLs("参考图", in.ImageUrls); err != nil {
		return nil, err
	}
	if in.Count < 1 || in.Count > MaxImageGenCount {
		return nil, xerr.Param(fmt.Sprintf("生成张数为 1–%d", MaxImageGenCount))
	}
	res, err := NormalizeResolution(in.Resolution)
	if err != nil {
		return nil, err
	}
	ar, err := NormalizeAspectRatio(in.AspectRatio, DefaultImageGenAspectRatio)
	if err != nil {
		return nil, err
	}
	name, group, err := NormalizeModel(in.Model, groups)
	if err != nil {
		return nil, err
	}
	// 按 model group 归一分辨率（Seedream 4K → 2K、1K 非 1:1 → 2K）：快照、计费档位、生图参数都用归一值
	res, warns, err := pipeline.NormalizeForGroup(group, res, ar)
	if err != nil {
		return nil, err
	}
	imgs := make([]string, 0, len(in.ImageUrls))
	for _, u := range in.ImageUrls {
		imgs = append(imgs, strings.TrimSpace(u))
	}
	return &ImageGenInput{
		Prompt: in.Prompt, Images: imgs, Count: in.Count, Resolution: res, AspectRatio: ar,
		ModelName: name, ModelGroup: group, Warnings: warns,
	}, nil
}

// ---------------- 查询类 ----------------

func checkBatchUser(batchID, userID int64) error {
	if userID <= 0 {
		return xerr.New(xerr.ParamMissing, "缺少用户")
	}
	if batchID <= 0 {
		return xerr.New(xerr.ParamMissing, "缺少批次号")
	}
	return nil
}

// NormalizeLimit 历史分页条数：默认 20，上限 50。
func NormalizeLimit(n int32) int32 {
	switch {
	case n <= 0:
		return 20
	case n > 50:
		return 50
	}
	return n
}

// checkKind 空=全部。
func checkKind(k string) error {
	switch k {
	case "", "tryon", "pose", "imagegen":
		return nil
	}
	return xerr.Param("类型只支持 tryon / pose / imagegen")
}

// FormatCursor 历史游标：<created_at 毫秒>_<id>。按 (created_at, id) 倒序翻页，同一毫秒的多条也不会漏。
func FormatCursor(createdAtMs, id int64) string {
	return strconv.FormatInt(createdAtMs, 10) + "_" + strconv.FormatInt(id, 10)
}

// ParseCursor 空串 = 第一页（ok=true、零值）。格式不对返回参数错误。
func ParseCursor(s string) (createdAtMs, id int64, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	a, b, found := strings.Cut(s, "_")
	ms, err1 := strconv.ParseInt(a, 10, 64)
	n, err2 := strconv.ParseInt(b, 10, 64)
	if !found || err1 != nil || err2 != nil || ms <= 0 || n <= 0 {
		return 0, 0, xerr.Param("游标不合法")
	}
	return ms, n, nil
}
