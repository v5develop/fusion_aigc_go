package logic

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/pkg/xerr"
)

// refMedia 一张参考图 / 视频。
type refMedia struct {
	URL     string
	Handle  string // 原始 handle（image1 / garment …）
	NodeID  string
	AssetID string
}

// runSpec 归一后的执行请求：分派与计费都只看它。
type runSpec struct {
	Kind       model.RunKind
	Backend    model.Backend
	Model      config.ModelSpec // image / video
	ModelGroup string
	Prompt     string
	Images     []refMedia // image 口（含别名，不含视频首尾帧口）按 index 排序
	First      string     // 视频 image1 / first
	Last       string     // 视频 image2 / last
	Videos     []string
	Count      int
	Ratio      string
	Size       string // 图 1K/2K/4K；视频 720p/1080p/4K
	Quality    string
	DurationS  int
	Sound      bool
	Params     map[string]any
	EntryCode  string
	AssetIDs   []string // 上游素材（入库 relatedIds）
	Warnings   []string // 参数归一告警（Seedream 4K → 2K 等）
}

// ToolKind 工具名 → 执行类型（mask 是局部重绘）。
func ToolKind(tool string) (model.RunKind, bool) {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "crop":
		return model.KindToolCrop, true
	case "upscale":
		return model.KindToolUpscale, true
	case "outpaint":
		return model.KindToolOutpaint, true
	case "mask", "inpaint":
		return model.KindToolInpaint, true
	case "i2p":
		return model.KindToolI2P, true
	case "tryon":
		return model.KindToolTryon, true
	}
	return "", false
}

// ResolveKind 请求的 kind（image / video / tool + tool，或直接写 tool_crop 等）→ 执行类型。
func ResolveKind(kind, tool string) (model.RunKind, error) {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "image", "":
		if k == "" && tool != "" {
			break
		}
		return model.KindImage, nil
	case "video":
		return model.KindVideo, nil
	case "tool":
	default:
		if rk := model.RunKind(k); rk.Valid() {
			return rk, nil
		}
		return "", xerr.Param("kind 只支持 image / video / tool")
	}
	if rk, ok := ToolKind(tool); ok {
		return rk, nil
	}
	return "", xerr.Param("不支持的工具：" + tool)
}

// qualityOf 原型中文 低/中/高 也收。
func qualityOf(q string) string {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case "low", "低":
		return "low"
	case "medium", "中", "mid":
		return "medium"
	case "high", "高":
		return "high"
	}
	return ""
}

// durationOf "4s" / "4" / "4秒" → 4。
func durationOf(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "s"), "秒")
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func inList(v string, list []string) (string, bool) {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return x, true
		}
	}
	return "", false
}

// sortRefs 按 (handle 组, index, 原顺序) 稳定排序。
func sortRefs(refs []*canvas.RunReference) []*canvas.RunReference {
	out := append([]*canvas.RunReference(nil), refs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Index, out[j].Index
		if a <= 0 {
			a = math.MaxInt32
		}
		if b <= 0 {
			b = math.MaxInt32
		}
		return a < b
	})
	return out
}

// parseParams 工具参数 JSON（接口字段 params）。
func parseParams(s string) (map[string]any, error) {
	m := map[string]any{}
	if strings.TrimSpace(s) == "" {
		return m, nil
	}
	if err := decodeUseNumber([]byte(s), &m); err != nil || m == nil {
		return nil, xerr.Param("params 必须是 JSON 对象")
	}
	return m, nil
}

// candidateLookup 上游节点（同画布）最近一次成功 run 的第 i 个候选；找不到返回空串。
type candidateLookup func(nodeID string, index int) string

// buildSpec 校验并归一执行请求。refs 的图片地址：带 candidate_index 时优先取服务端记录的上游候选。
func (c *core) buildSpec(req *canvas.RunNodeRequest, lookup candidateLookup) (*runSpec, error) {
	kind, err := ResolveKind(req.Kind, req.Tool)
	if err != nil {
		return nil, err
	}
	sp := &runSpec{Kind: kind, Backend: model.BackendOf(kind), Count: 1, Sound: req.Sound}
	if sp.Params, err = parseParams(req.ToolParams); err != nil {
		return nil, err
	}
	// 引用：提示词口文本、各图片 / 视频口
	var promptRef string
	for _, r := range sortRefs(req.References) {
		h := NormalizeHandle(r.Handle)
		if r.AssetId != "" {
			sp.AssetIDs = append(sp.AssetIDs, r.AssetId)
		}
		switch h {
		case HandlePrompt:
			promptRef = strings.TrimSpace(r.Text)
		case HandleImage:
			u := strings.TrimSpace(r.Image)
			if r.CandidateIndex != nil && lookup != nil {
				if v := lookup(r.NodeId, int(*r.CandidateIndex)); v != "" {
					u = v
				}
			}
			if u == "" {
				return nil, xerr.Param(fmt.Sprintf("引用 %s 缺少图片地址（上游还没有结果？）", r.NodeId))
			}
			if !isHTTP(u) {
				return nil, xerr.Param(fmt.Sprintf("引用 %s 的图片必须是 http(s) 地址，请先上传", r.NodeId))
			}
			m := refMedia{URL: u, Handle: r.Handle, NodeID: r.NodeId, AssetID: r.AssetId}
			switch {
			case kind == model.KindVideo && (r.Handle == "image1" || r.Handle == "first"):
				sp.First = u
			case kind == model.KindVideo && (r.Handle == "image2" || r.Handle == "last"):
				sp.Last = u
			default:
				sp.Images = append(sp.Images, m)
			}
		case HandleVideo:
			u := strings.TrimSpace(r.Video)
			if !isHTTP(u) {
				return nil, xerr.Param(fmt.Sprintf("引用 %s 的视频必须是 http(s) 地址", r.NodeId))
			}
			sp.Videos = append(sp.Videos, u)
		}
	}
	own := strings.TrimSpace(req.Prompt)
	switch {
	case promptRef != "" && own != "" && promptRef != own:
		sp.Prompt = promptRef + "\n" + own
	case promptRef != "":
		sp.Prompt = promptRef
	default:
		sp.Prompt = own
	}
	if utf8.RuneCountInString(sp.Prompt) > c.s.Config.Limits.MaxPromptLen {
		return nil, xerr.Param(fmt.Sprintf("提示词最多 %d 字", c.s.Config.Limits.MaxPromptLen))
	}
	ec := c.s.Config.EntryCodes
	switch kind {
	case model.KindImage, model.KindVideo:
		mk := "image"
		if kind == model.KindVideo {
			mk = "video"
		}
		m, ok := c.s.Config.FindModel(req.Model, req.ModelId, mk)
		if !ok {
			return nil, xerr.Param(fmt.Sprintf("不支持的模型：%s", firstNonEmpty(req.ModelId, req.Model)))
		}
		sp.Model, sp.ModelGroup = m, m.ModelGroup
		if err := c.specModelParams(sp, req); err != nil {
			return nil, err
		}
		sp.EntryCode = ec.Image
		if kind == model.KindVideo {
			sp.EntryCode = ec.Video
		}
	case model.KindToolI2P:
		sp.ModelGroup, sp.EntryCode = c.s.Config.I2P.ModelGroup, ec.I2P
		if len(sp.Images) == 0 {
			if u := paramStr(sp.Params, "image_url"); isHTTP(u) {
				sp.Images = append(sp.Images, refMedia{URL: u})
			}
		}
		if len(sp.Images) == 0 {
			return nil, xerr.Param("反推提示词需要连接一张图片")
		}
	case model.KindToolCrop:
		sp.ModelGroup, sp.EntryCode = "media.image-crop", ec.Crop
		if err := needOneImage(sp, "裁剪"); err != nil {
			return nil, err
		}
		if _, err := CropBox(sp.Params); err != nil {
			return nil, err
		}
	case model.KindToolUpscale:
		sp.ModelGroup = "java.ai-image-upscaler"
		if err := needOneImage(sp, "高清放大"); err != nil {
			return nil, err
		}
		if _, err := upscaleResolution(sp.Params, req.Size); err != nil {
			return nil, err
		}
	case model.KindToolOutpaint:
		sp.ModelGroup = "java.ai-ratio-convert"
		if err := needOneImage(sp, "扩图"); err != nil {
			return nil, err
		}
		sp.Ratio = firstNonEmpty(paramStr(sp.Params, "ratio"), paramStr(sp.Params, "aspect_ratio"), req.Ratio)
		w, h := toInt(sp.Params["width"]), toInt(sp.Params["height"])
		if (w > 0) != (h > 0) {
			return nil, xerr.Param("扩图的 width 与 height 须成对给")
		}
		if w == 0 && !ratioRe(sp.Ratio) {
			return nil, xerr.Param("扩图需要目标比例（如 16:9）")
		}
	case model.KindToolInpaint:
		sp.ModelGroup = "java.ai-inpaint"
		if err := needOneImage(sp, "局部重绘"); err != nil {
			return nil, err
		}
		if p := paramStr(sp.Params, "mask_prompt"); p != "" {
			sp.Prompt = p
		}
		if strings.TrimSpace(sp.Prompt) == "" {
			return nil, xerr.Param("局部重绘需要填写修改要求")
		}
		if !hasMask(sp.Params) {
			return nil, xerr.Param("局部重绘需要蒙版（mask_url / mask_data_url / mask_strokes / bbox 任一）")
		}
	case model.KindToolTryon:
		sp.ModelGroup = "studio.tryon"
		if _, _, err := tryonInputs(sp); err != nil {
			return nil, err
		}
	}
	return sp, nil
}

func (c *core) specModelParams(sp *runSpec, req *canvas.RunNodeRequest) error {
	m := sp.Model
	sp.Ratio = strings.TrimSpace(req.Ratio)
	switch strings.ToLower(sp.Ratio) {
	case "", "auto", "自适应", "adaptive":
		sp.Ratio = ""
	default:
		if len(m.Ratios) > 0 {
			v, ok := inList(sp.Ratio, m.Ratios)
			if !ok {
				return xerr.Param(fmt.Sprintf("%s 不支持比例 %s", m.Model, sp.Ratio))
			}
			sp.Ratio = v
		}
	}
	sp.Size = strings.TrimSpace(req.Size)
	if sp.Size == "" && len(m.Sizes) > 0 {
		sp.Size = m.Sizes[0]
	}
	if m.Kind != "video" {
		// 先按模型组归一再校验档位：Seedream 4K 按 2K 受理（不拒），冻结 / 结算 / gateway 请求都用归一值
		sp.Size, sp.Warnings = normalizeImageSize(sp.ModelGroup, sp.Size)
	}
	if sp.Size != "" && len(m.Sizes) > 0 {
		v, ok := inList(sp.Size, m.Sizes)
		if !ok {
			return xerr.Param(fmt.Sprintf("%s 不支持分辨率 %s", m.Model, sp.Size))
		}
		sp.Size = v
	}
	if m.Kind == "video" {
		sp.Count = 1
		sp.DurationS = durationOf(req.Duration)
		if sp.DurationS == 0 && len(m.Durations) > 0 {
			sp.DurationS = durationOf(m.Durations[0])
		}
		if len(m.Durations) > 0 {
			if _, ok := inList(fmt.Sprintf("%ds", sp.DurationS), m.Durations); !ok {
				return xerr.Param(fmt.Sprintf("%s 不支持时长 %s", m.Model, req.Duration))
			}
		}
		if sp.Prompt == "" && sp.First == "" && len(sp.Images) == 0 {
			return xerr.Param("视频生成需要提示词或首帧图")
		}
		maxVid := max(m.MaxReferenceVideos, 0)
		if g, ok := gatewayGroups.get(m.ModelGroup); ok && g.FeatureLimits.MaxReferenceVideos != nil {
			maxVid = *g.FeatureLimits.MaxReferenceVideos
		}
		if len(sp.Videos) > maxVid {
			return xerr.Param(fmt.Sprintf("%s 最多 %d 个参考视频", m.Model, maxVid))
		}
		if sp.Last != "" && sp.First == "" {
			return xerr.Param("只有尾帧（图片 2）不能生成，请同时连接首帧（图片 1）")
		}
		// gateway 审计：首尾帧不能与 reference_* 同用；vid.seedance-2.5 不支持首尾帧，换 FrameModelGroup
		if sp.First != "" || sp.Last != "" {
			if len(sp.Images) > 0 || len(sp.Videos) > 0 {
				return xerr.Param("首尾帧（图片 1 / 图片 2）不能与参考图、参考视频同时使用")
			}
			if strings.EqualFold(sp.ModelGroup, "vid.seedance-2.5") && c.s.Config.FrameModelGroup != "" {
				sp.ModelGroup = c.s.Config.FrameModelGroup
			}
		}
	} else {
		sp.Quality = qualityOf(req.Quality)
		if len(m.Qualities) == 0 {
			sp.Quality = ""
		}
		sp.Count = int(req.Count)
		if sp.Count == 0 {
			sp.Count = 1
		}
		limit := m.CountMax
		if limit <= 0 {
			limit = 4
		}
		if sp.Count < 1 || sp.Count > limit {
			return xerr.Param(fmt.Sprintf("count 取值 1–%d", limit))
		}
		if strings.TrimSpace(sp.Prompt) == "" {
			return xerr.Param("提示词为空")
		}
	}
	if lim := c.catalogImageRefs(m); lim > 0 && len(sp.Images)+boolInt(sp.First != "")+boolInt(sp.Last != "") > lim {
		return xerr.Param(fmt.Sprintf("%s 参考图最多 %d 张", m.Model, lim))
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func needOneImage(sp *runSpec, label string) error {
	if len(sp.Images) == 0 {
		if u := paramStr(sp.Params, "image_url"); isHTTP(u) {
			sp.Images = append(sp.Images, refMedia{URL: u})
		}
	}
	if len(sp.Images) == 0 {
		return xerr.Param(label + "需要连接一张图片")
	}
	if len(sp.Images) > 1 {
		sp.Images = sp.Images[:1]
	}
	return nil
}

func ratioRe(r string) bool {
	a, b, ok := strings.Cut(r, ":")
	if !ok {
		return false
	}
	x, e1 := strconv.Atoi(a)
	y, e2 := strconv.Atoi(b)
	return e1 == nil && e2 == nil && x > 0 && y > 0 && x < 1000 && y < 1000
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

func paramStr(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		switch x := v.(type) {
		case string:
			return strings.TrimSpace(x)
		case json.Number:
			return x.String()
		}
	}
	return ""
}

// upscaleResolution 放大档位：params.scale 2→2k、4→4k；或 params.resolution / 请求 size（1k/2k/4k）；默认 2k。
func upscaleResolution(p map[string]any, size string) (string, error) {
	if s := toInt(p["scale"]); s > 0 {
		switch s {
		case 2:
			return "2k", nil
		case 4:
			return "4k", nil
		}
		return "", xerr.Param("放大倍数只支持 2 / 4")
	}
	r := strings.ToLower(firstNonEmpty(paramStr(p, "resolution"), size))
	switch r {
	case "":
		return "2k", nil
	case "1k", "2k", "4k":
		return r, nil
	}
	return "", xerr.Param("放大分辨率只支持 1k / 2k / 4k")
}

func hasMask(p map[string]any) bool {
	if isHTTP(paramStr(p, "mask_url")) || paramStr(p, "mask_rid") != "" || strings.HasPrefix(strings.ToLower(paramStr(p, "mask_data_url")), "data:image/") {
		return true
	}
	if s, ok := p["mask_strokes"].([]any); ok && len(s) > 0 {
		return true
	}
	if b, ok := p["bbox"].([]any); ok && len(b) == 4 {
		return true
	}
	return false
}

// tryonInputs 试穿的服装图与模特图：params.garment_image / params.models[].image 优先，
// 否则按引用口 garment / models 取；都没标口时第一张当服装、其余当模特。
func tryonInputs(sp *runSpec) (string, []string, error) {
	garment := paramStr(sp.Params, "garment_image")
	var models []string
	if ms, ok := sp.Params["models"].([]any); ok {
		for _, x := range ms {
			switch v := x.(type) {
			case map[string]any:
				if u := paramStr(v, "image"); u != "" {
					models = append(models, u)
				}
			case string:
				models = append(models, strings.TrimSpace(v))
			}
		}
	}
	var untagged []string
	for _, r := range sp.Images {
		switch r.Handle {
		case "garment":
			if garment == "" {
				garment = r.URL
			}
		case "models":
			models = append(models, r.URL)
		default:
			untagged = append(untagged, r.URL)
		}
	}
	if garment == "" && len(untagged) > 0 {
		garment, untagged = untagged[0], untagged[1:]
	}
	if len(models) == 0 {
		models = untagged
	}
	if !isHTTP(garment) {
		return "", nil, xerr.Param("试穿需要 1 张服装图")
	}
	if len(models) == 0 || len(models) > MaxTryonModels {
		return "", nil, xerr.Param(fmt.Sprintf("试穿需要 1–%d 张模特图", MaxTryonModels))
	}
	for _, u := range models {
		if !isHTTP(u) {
			return "", nil, xerr.Param("模特图必须是 http(s) 地址")
		}
	}
	return garment, models, nil
}

// NormBox 归一化框（0–1）。
type NormBox struct{ X, Y, W, H float64 }

// CropBox 裁剪框：params {x,y,w,h}（0–1）或 params.crop 同形；shape=circle 取外接矩形（即框本身）；
// shape=brush 取 brush/cropBrush 笔迹 [{x,y,r}] 的外接矩形。
func CropBox(p map[string]any) (NormBox, error) {
	src := p
	if c, ok := p["crop"].(map[string]any); ok {
		src = c
	}
	shape := strings.ToLower(paramStr(src, "shape"))
	if shape == "" {
		shape = strings.ToLower(paramStr(p, "shape"))
	}
	if shape == "brush" {
		strokes, _ := p["brush"].([]any)
		if len(strokes) == 0 {
			strokes, _ = p["cropBrush"].([]any)
		}
		if b, ok := strokeBounds(strokes); ok {
			return b, nil
		}
		return NormBox{}, xerr.Param("笔刷裁剪缺少笔迹")
	}
	x, ok1 := toFloat(src["x"])
	y, ok2 := toFloat(src["y"])
	w, ok3 := toFloat(src["w"])
	h, ok4 := toFloat(src["h"])
	if !(ok1 && ok2 && ok3 && ok4) {
		return NormBox{}, xerr.Param("裁剪需要 x / y / w / h（相对原图 0–1）")
	}
	b := clampBox(NormBox{x, y, w, h})
	if b.W <= 0 || b.H <= 0 {
		return NormBox{}, xerr.Param("裁剪框为空")
	}
	return b, nil
}

func clampBox(b NormBox) NormBox {
	cl := func(v float64) float64 { return math.Min(1, math.Max(0, v)) }
	x0, y0 := cl(b.X), cl(b.Y)
	x1, y1 := cl(b.X+b.W), cl(b.Y+b.H)
	return NormBox{x0, y0, x1 - x0, y1 - y0}
}

// strokeBounds 笔迹 [{x,y,r}]（0–1）的外接矩形。
func strokeBounds(strokes []any) (NormBox, bool) {
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	n := 0
	for _, s := range strokes {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		x, ok1 := toFloat(m["x"])
		y, ok2 := toFloat(m["y"])
		if !ok1 || !ok2 {
			continue
		}
		r, _ := toFloat(m["r"])
		minX, minY = math.Min(minX, x-r), math.Min(minY, y-r)
		maxX, maxY = math.Max(maxX, x+r), math.Max(maxY, y+r)
		n++
	}
	if n == 0 {
		return NormBox{}, false
	}
	b := clampBox(NormBox{minX, minY, maxX - minX, maxY - minY})
	return b, b.W > 0 && b.H > 0
}

// isSeedreamGroup 是否需要「统一 2K」规则的 Seedream 5 Pro 模型组（img.seedream-5-pro*）。
// Seedream 5.5（img.seedream-5.5）1K / 2K / 4K 各比例都有预设尺寸，不套这条降档规则（与 studio 换代同口径）。
func isSeedreamGroup(group string) bool {
	return strings.Contains(strings.ToLower(group), "seedream-5-pro")
}

// normalizeImageSize 按模型组归一图片分辨率。Seedream 1K 下非 1:1 比例会被拒、4K 会被静默降 2K（studio 的
// pipeline.NormalizeForGroup 同一结论；internal 包跨服务不能直接引用），画布目录对 Seedream 只给 2K，
// 所以 1K / 4K / 空 → 2K（1K、4K 带告警）。这是画布改版前 gateway 请求就在用的规则，现在提前到受理阶段，计费与生成一致。
// 返回大写档位（与目录 sizes / 计费 resolution 一致）；非 Seedream 原样返回。
func normalizeImageSize(group, size string) (string, []string) {
	if !isSeedreamGroup(group) {
		return size, nil
	}
	switch strings.ToUpper(strings.TrimSpace(size)) {
	case "", "2K":
		return "2K", nil
	case "4K":
		return "2K", []string{"Seedream 不支持 4K（会被降为 2K），已按 2K 生成并计费"}
	case "1K":
		return "2K", []string{"Seedream 1K 只支持 1:1、画布统一按 2K，已按 2K 生成并计费"}
	}
	return size, nil
}

// normalizeRunSize RunNode 受理时就把请求里的图片分辨率按模型组归一（写回 req.Size），返回告警。
// 在算 request 快照与幂等比对之前调用：快照里存归一值，同一个请求重放时归一结果相同，幂等仍成立。
// 解析不了（kind / 模型不对）就不动，留给 buildSpec 报参数错误。
func (c *core) normalizeRunSize(req *canvas.RunNodeRequest) []string {
	if req == nil {
		return nil
	}
	if kind, err := ResolveKind(req.Kind, req.Tool); err != nil || kind != model.KindImage {
		return nil
	}
	m, ok := c.s.Config.FindModel(req.Model, req.ModelId, "image")
	if !ok || !isSeedreamGroup(m.ModelGroup) {
		return nil
	}
	size := strings.TrimSpace(req.Size)
	if size == "" && len(m.Sizes) > 0 {
		size = m.Sizes[0]
	}
	got, warns := normalizeImageSize(m.ModelGroup, size)
	if len(warns) > 0 {
		req.Size = got
	}
	return warns
}
