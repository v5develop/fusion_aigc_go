package pipeline

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"aigc-local-life/pkg/aigateway"
)

// gateway 操作。
const (
	OpImageEdit     = "image.edit"
	OpImageGenerate = "image.generate"
)

// 参考图上限兜底（gateway 审计 2026-10-31）：GPT 16、Banana 14、Seedream 10、Qwen 3，其余 14。
// 运行时优先用 catalog 的 feature_limits.max_reference_images（GenOptions.MaxRefImages），这里只在取不到时兜底。
const (
	DefaultMaxRefImages  = 14
	GPTMaxRefImages      = 16
	SeedreamMaxRefImages = 10
	QwenMaxRefImages     = 3
)

// MaxRefImages 按 model group 取参考图上限（兜底值）。
func MaxRefImages(modelGroup string) int {
	g := strings.ToLower(modelGroup)
	switch {
	case strings.Contains(g, "seedream"):
		return SeedreamMaxRefImages
	case strings.Contains(g, "gpt"):
		return GPTMaxRefImages
	case strings.Contains(g, "qwen"):
		return QwenMaxRefImages
	}
	return DefaultMaxRefImages
}

// IsGPTGroup 是否 GPT 图片组（只有它接收 quality）。
func IsGPTGroup(modelGroup string) bool { return strings.Contains(strings.ToLower(modelGroup), "gpt") }

// IsSeedreamGroup 是否 Seedream 组。
func IsSeedreamGroup(modelGroup string) bool {
	return strings.Contains(strings.ToLower(modelGroup), "seedream")
}

// 图片质量（gateway quality：auto/low/medium/high；Atlas 部署缺省 low，所以要显式传）。
const (
	QualityHigh   = "high"
	QualityMedium = "medium"
)

// QualityFor 生图质量：只给 GPT 组下发（其余组不接收）。用户指定优先；商拍 high，其余 medium。
func QualityFor(modelGroup, kind, user string) string {
	if !IsGPTGroup(modelGroup) {
		return ""
	}
	switch q := strings.ToLower(strings.TrimSpace(user)); q {
	case "auto", "low", "medium", "high":
		return q
	}
	if kind == KindTryon {
		return QualityHigh
	}
	return QualityMedium
}

// NormalizeForGroup 按 model group 修正分辨率：Seedream 1K 下非 1:1 比例会被拒、4K 会被静默降 2K，
// 所以 4k → 2k，1k 且比例不是 1:1 → 2k，并返回告警。返回小写分辨率。
func NormalizeForGroup(modelGroup, resolution, aspectRatio string) (string, []string, error) {
	res, err := NormalizeResolution(resolution)
	if err != nil {
		return "", nil, err
	}
	if !IsSeedreamGroup(modelGroup) {
		return res, nil, nil
	}
	ar := strings.TrimSpace(aspectRatio)
	switch {
	case res == "4k":
		return "2k", []string{"Seedream 不支持 4K（会被降为 2K），已按 2K 生成"}, nil
	case res == "1k" && ar != "1:1":
		return "2k", []string{fmt.Sprintf("Seedream 1K 只支持 1:1（当前 %s），已按 2K 生成", orStr(ar, "未指定"))}, nil
	}
	return res, nil, nil
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// StandardAspectRatios 各图片组都支持的比例档（用于按输入图比例取最近档）。
var StandardAspectRatios = []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"}

// NearestAspectRatio 宽高 → 最近的标准比例（按 log 比值距离）；宽高未知返回空串。
func NearestAspectRatio(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	target := math.Log(float64(w) / float64(h))
	best, bestD := "", math.MaxFloat64
	for _, ar := range StandardAspectRatios {
		var a, b float64
		if _, err := fmt.Sscanf(ar, "%f:%f", &a, &b); err != nil || b == 0 {
			continue
		}
		if d := math.Abs(math.Log(a/b) - target); d < bestD {
			best, bestD = ar, d
		}
	}
	return best
}

// GenOptions 生图请求的可选参数，零值即默认行为。
type GenOptions struct {
	// Quality auto/low/medium/high；非 GPT 组会被忽略（用 QualityFor 取）
	Quality string
	// MaxRefImages >0 时覆盖兜底的参考图上限（取自 catalog feature_limits.max_reference_images）
	MaxRefImages int
}

func (o GenOptions) maxRefs(modelGroup string) int {
	if o.MaxRefImages > 0 {
		return o.MaxRefImages
	}
	return MaxRefImages(modelGroup)
}

func (o GenOptions) quality(modelGroup string) string {
	if !IsGPTGroup(modelGroup) {
		return ""
	}
	return o.Quality
}

// Refs 字符串引用 → MediaRef（不带 sha256）。
func Refs(uris []string) []aigateway.MediaRef {
	out := make([]aigateway.MediaRef, 0, len(uris))
	for _, u := range uris {
		out = append(out, aigateway.MediaRef{URI: u})
	}
	return out
}

// 前端模型档（config.DefaultModelGroups 的键）。
const (
	ModelGeneral  = "通用"
	ModelGPT      = "GPT"
	ModelBanana   = "Banana"
	ModelSeedream = "Seedream"
)

// modelAliases 小写别名 → 档名（商拍页中文名 / 英文 id、姿态页 nano / seedream 都认）。
var modelAliases = map[string]string{
	"通用": ModelGeneral, "general": ModelGeneral, "default": ModelGeneral,
	"gpt": ModelGPT, "gpt-image-2": ModelGPT, "gpt-image": ModelGPT,
	"banana": ModelBanana, "nano-banana": ModelBanana, "nano-banana-2.1": ModelBanana, "nano": ModelBanana,
	"seedream": ModelSeedream, "seedream-5-pro": ModelSeedream,
}

// ErrUnknownModel 不认识的模型档。
var ErrUnknownModel = errors.New("不支持的模型")

// MapModel 前端模型档 → gateway model group。空 = 通用。
// 先按别名归一到档名，再在 groups 里查（键大小写不敏感）；display 本身就是 groups 里的某个 model group 码时原样返回。
func MapModel(display string, groups map[string]string) (string, error) {
	name := strings.TrimSpace(display)
	if name == "" {
		name = ModelGeneral
	}
	if canon, ok := modelAliases[strings.ToLower(name)]; ok {
		name = canon
	}
	for k, g := range groups {
		if strings.EqualFold(k, name) && g != "" {
			return g, nil
		}
	}
	for _, g := range groups {
		if g != "" && g == strings.TrimSpace(display) {
			return g, nil
		}
	}
	return "", fmt.Errorf("%w：%s", ErrUnknownModel, display)
}

// NormalizeResolution 分辨率归一为小写 1k / 2k / 4k（入库与回显口径，与 pb 注释一致）；空 = 1k。
func NormalizeResolution(s string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(s))
	switch r {
	case "":
		return "1k", nil
	case "1k", "2k", "4k":
		return r, nil
	}
	return "", fmt.Errorf("分辨率只支持 1k / 2k / 4k：%s", s)
}

// GatewayResolution gateway 契约要求大写 K（^[0-9]{1,2}K$）。
func GatewayResolution(s string) (string, error) {
	r, err := NormalizeResolution(s)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(r), nil
}

var labelKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// MaxLabelValueLen label 值长度上限（gateway 契约 ≤256）。
const MaxLabelValueLen = 256

func checkLabels(labels map[string]string) (map[string]string, error) {
	if len(labels) > 16 {
		return nil, fmt.Errorf("labels 最多 16 对")
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if !labelKeyRe.MatchString(k) {
			return nil, fmt.Errorf("label 键不合法：%q", k)
		}
		if len(v) > MaxLabelValueLen {
			return nil, fmt.Errorf("label %q 的值超过 %d 字节", k, MaxLabelValueLen)
		}
		out[k] = v
	}
	return out, nil
}

// ImageEditInput image.edit 的 input（gateway contract/operations.py ImageEditInput 的子集）。
type ImageEditInput struct {
	Prompt      string               `json:"prompt"`
	Images      []aigateway.MediaRef `json:"images"` // 顺序保留，第一张为主图
	N           int                  `json:"n"`
	AspectRatio string               `json:"aspect_ratio,omitempty"`
	Resolution  string               `json:"resolution,omitempty"`
	Quality     string               `json:"quality,omitempty"` // 只给 GPT 组
}

// ImageGenerateInput image.generate 的 input。
type ImageGenerateInput struct {
	Prompt      string `json:"prompt"`
	N           int    `json:"n"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	Quality     string `json:"quality,omitempty"` // 只给 GPT 组
}

// MaxPromptRunes gateway prompt 上限 32000。
const MaxPromptRunes = 32000

// BuildImageEditRequest 构造 image.edit：images 为 URL 或 mr:// 引用（字符串形式，不带 sha256），其余同 BuildImageEditRequestRefs。
func BuildImageEditRequest(kind, modelGroup, prompt string, images []string, resolution, aspectRatio string, labels map[string]string) (aigateway.CallRequest, []string, error) {
	return BuildImageEditRequestRefs(kind, modelGroup, prompt, Refs(images), resolution, aspectRatio, labels, GenOptions{})
}

// BuildImageEditRequestRefs 构造 image.edit：images 优先 mr://{rid} 并带 sha256，图 1 在前；
// 分辨率按组修正（NormalizeForGroup）；超过参考图上限时从尾部截断；然后按实际张数在 prompt 首行加图序声明（正文不动）。
// kind 为 tryon / pose / imagegen；labels 键须符合 gateway 规则（≤16 对，值 ≤256），会自动补 kind。返回告警（只用于日志 / meta）。
func BuildImageEditRequestRefs(kind, modelGroup, prompt string, images []aigateway.MediaRef, resolution, aspectRatio string, labels map[string]string, opt GenOptions) (aigateway.CallRequest, []string, error) {
	var warns []string
	if strings.TrimSpace(prompt) == "" {
		return aigateway.CallRequest{}, nil, errors.New("提示词为空")
	}
	if modelGroup == "" {
		return aigateway.CallRequest{}, nil, errors.New("缺少 model group")
	}
	var refs []aigateway.MediaRef
	for _, r := range images {
		if r.URI = strings.TrimSpace(r.URI); r.URI != "" {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return aigateway.CallRequest{}, nil, errors.New("image.edit 至少需要 1 张参考图")
	}
	if lim := opt.maxRefs(modelGroup); len(refs) > lim {
		warns = append(warns, fmt.Sprintf("参考图 %d 张超过 %s 上限 %d，已截断", len(refs), modelGroup, lim))
		refs = refs[:lim]
	}
	ar := strings.TrimSpace(aspectRatio)
	r, w, err := NormalizeForGroup(modelGroup, resolution, ar)
	if err != nil {
		return aigateway.CallRequest{}, nil, err
	}
	warns = append(warns, w...)
	full := WithImageOrderLine(prompt, kind, len(refs))
	if len([]rune(full)) > MaxPromptRunes {
		return aigateway.CallRequest{}, nil, fmt.Errorf("提示词超过 %d 字", MaxPromptRunes)
	}
	in := ImageEditInput{Prompt: full, Images: refs, N: 1, AspectRatio: ar, Resolution: strings.ToUpper(r), Quality: opt.quality(modelGroup)}
	lb, err := withKindLabel(kind, labels)
	if err != nil {
		return aigateway.CallRequest{}, nil, err
	}
	return aigateway.CallRequest{
		Operation: OpImageEdit, ModelGroup: modelGroup, Input: in, Labels: lb,
		MediaPurpose: aigateway.MediaPurposeIntermediate,
	}, warns, nil
}

// BuildImageGenerateRequest 图片生成无参考图时的 image.generate（prompt 原样，不加图序）。
func BuildImageGenerateRequest(modelGroup, prompt, resolution, aspectRatio string, labels map[string]string) (aigateway.CallRequest, error) {
	req, _, err := BuildImageGenerateRequestOpt(modelGroup, prompt, resolution, aspectRatio, labels, GenOptions{})
	return req, err
}

// BuildImageGenerateRequestOpt 同 BuildImageGenerateRequest，带 quality 与分辨率按组修正的告警。
func BuildImageGenerateRequestOpt(modelGroup, prompt, resolution, aspectRatio string, labels map[string]string, opt GenOptions) (aigateway.CallRequest, []string, error) {
	if strings.TrimSpace(prompt) == "" {
		return aigateway.CallRequest{}, nil, errors.New("提示词为空")
	}
	if modelGroup == "" {
		return aigateway.CallRequest{}, nil, errors.New("缺少 model group")
	}
	if len([]rune(prompt)) > MaxPromptRunes {
		return aigateway.CallRequest{}, nil, fmt.Errorf("提示词超过 %d 字", MaxPromptRunes)
	}
	ar := strings.TrimSpace(aspectRatio)
	res, warns, err := NormalizeForGroup(modelGroup, resolution, ar)
	if err != nil {
		return aigateway.CallRequest{}, nil, err
	}
	lb, err := withKindLabel(KindImageGen, labels)
	if err != nil {
		return aigateway.CallRequest{}, nil, err
	}
	return aigateway.CallRequest{
		Operation: OpImageGenerate, ModelGroup: modelGroup,
		Input:  ImageGenerateInput{Prompt: prompt, N: 1, AspectRatio: ar, Resolution: strings.ToUpper(res), Quality: opt.quality(modelGroup)},
		Labels: lb, MediaPurpose: aigateway.MediaPurposeIntermediate,
	}, warns, nil
}

// BuildImageGenRequest 图片生成：有参考图走 image.edit（加图序），没有走 image.generate。
func BuildImageGenRequest(modelGroup, prompt string, refs []string, resolution, aspectRatio string, labels map[string]string) (aigateway.CallRequest, []string, error) {
	return BuildImageGenRequestRefs(modelGroup, prompt, Refs(refs), resolution, aspectRatio, labels, GenOptions{})
}

// BuildImageGenRequestRefs 同 BuildImageGenRequest，参考图带 sha256，并带 GenOptions。
func BuildImageGenRequestRefs(modelGroup, prompt string, refs []aigateway.MediaRef, resolution, aspectRatio string, labels map[string]string, opt GenOptions) (aigateway.CallRequest, []string, error) {
	if len(refs) == 0 {
		return BuildImageGenerateRequestOpt(modelGroup, prompt, resolution, aspectRatio, labels, opt)
	}
	return BuildImageEditRequestRefs(KindImageGen, modelGroup, prompt, refs, resolution, aspectRatio, labels, opt)
}

func withKindLabel(kind string, labels map[string]string) (map[string]string, error) {
	lb, err := checkLabels(labels)
	if err != nil {
		return nil, err
	}
	if _, ok := lb["kind"]; !ok && kind != "" {
		if len(lb) >= 16 {
			return lb, nil
		}
		lb["kind"] = kind
	}
	return lb, nil
}

// OutputImage 一张成品。RID 为媒体服务资源（转存用），URL 是 24 小时签名地址（不要入库）。
type OutputImage struct {
	RID  string
	URL  string
	W, H int
}

// ParseOutputs 取成功调用的产物与成本（人民币）。只取成功的 outputs[].media 与 result.image；
// 成本优先 cost.amount_cny，缺失且币种是 CNY 时用 amount；解析不了按 0。
func ParseOutputs(call *aigateway.Call) ([]OutputImage, float64) {
	if call == nil {
		return nil, 0
	}
	var out []OutputImage
	if call.Result != nil {
		for _, m := range call.Result.Media() {
			out = append(out, OutputImage{RID: m.Rid, URL: m.URL, W: m.Width, H: m.Height})
		}
	}
	return out, CostCNY(call.Cost)
}

// CostCNY gateway 成本 → 人民币。
func CostCNY(c *aigateway.Cost) float64 {
	if c == nil {
		return 0
	}
	s := c.AmountCNY
	if s == "" && strings.EqualFold(c.Currency, "CNY") {
		s = c.Amount
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
