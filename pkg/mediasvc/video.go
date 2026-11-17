package mediasvc

// 视频做同款用到的媒体算子：区间切段、多段拼接（含占位段）、视频 / 图片文字叠加。
// 服务端实现见 app/media/api_video_replicate.go、api_text.go；限制（80 秒、6 段、16 个文字槽位……）在服务端强制，
// 客户端只做明显越界的前置检查，省一次往返。失败时 *Error 的 Check 是服务端的判据，业务侧据此翻译成用户文案。
//
// 其中 Cut 的签名满足 app/replicate/core.Cutter；*Client 可直接装配。
// 所有方法的源都是 rid（服务端不接受任意 URL）。产物都落中间产物私有桶（purpose=intermediate），需要入素材库再 CopyToLibrary。
// idem 为空时按入参派生（同入参重放得到同一个 rid，见 autoIdem）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

const (
	// 与服务端 app/media/api_video_replicate.go 一致
	MaxCutSegments    = 6
	MaxConcatItems    = 6
	MaxTextSlots      = 16
	MaxTextRunes      = 500
	MaxReplicateVideo = 80.0 // 秒
)

func badRequest(check, msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: msg, Check: check}
}

// TimeRange 区间（秒，毫秒精度）。
type TimeRange struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// CutSegment 一段切片结果。
type CutSegment struct {
	Resource
	Start float64 // 实际起点（秒）
	End   float64 // 实际终点（秒，超出时长不超过 0.05 秒时会被收到时长上）
}

// CutRanges 按区间切段（POST /v1/mediasvc/video/cut）：重编码，起点不要求在关键帧上；一次 1~6 段，按入参顺序返回。
// 区间可以重叠（衔接窗口）。越界返回 422 source_too_short（Check=video.cut_out_of_range）。
func (c *Client) CutRanges(ctx context.Context, rid string, ranges []TimeRange, idem string) ([]CutSegment, error) {
	if len(ranges) < 1 || len(ranges) > MaxCutSegments {
		return nil, badRequest("request.segments_count", "切段数量需要 1-6")
	}
	for _, r := range ranges {
		if !(r.End > r.Start) || r.Start < 0 {
			return nil, badRequest("request.segment_range", "切段区间不合法")
		}
	}
	idem = pick(idem, "video/cut", rid, ranges, PurposeIntermediate)
	body, _ := json.Marshal(map[string]any{"source": map[string]string{"rid": rid}, "segments": ranges, "purpose": PurposeIntermediate})
	raw, err := c.post(ctx, rid, "/v1/mediasvc/video/cut", body, "application/json", idem)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Segments []struct {
			mediaResult
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	if len(resp.Segments) != len(ranges) {
		return nil, fmt.Errorf("媒体服务返回 %d 段，期望 %d", len(resp.Segments), len(ranges))
	}
	out := make([]CutSegment, len(ranges))
	for i, s := range resp.Segments {
		if s.Object.RID == "" {
			return nil, fmt.Errorf("媒体服务返回的切段缺少 rid")
		}
		out[i] = CutSegment{Resource: s.resource(), Start: s.Start, End: s.End}
	}
	return out, nil
}

// Cut 切出 [t0, t1] 一段，返回它的 rid（满足 core.Cutter）。
func (c *Client) Cut(ctx context.Context, sourceRid string, t0, t1 float64, idem string) (string, error) {
	segs, err := c.CutRanges(ctx, sourceRid, []TimeRange{{Start: t0, End: t1}}, idem)
	if err != nil {
		return "", err
	}
	return segs[0].RID, nil
}

// Placeholder 缺失段的占位画面：纯色底 + 居中文字（失败段预览版用）。
type Placeholder struct {
	Duration   float64 `json:"duration"` // 秒，0.1~80
	Text       string  `json:"text,omitempty"`
	FontRid    string  `json:"font_rid,omitempty"` // 含中文必须给（font 摄入得到的 rid）
	Size       int     `json:"size,omitempty"`
	Color      string  `json:"color,omitempty"`      // 文字色 #RRGGBB，默认白
	Background string  `json:"background,omitempty"` // 底色，默认深灰
}

// ConcatItem 拼接的一段：RID 与 Placeholder 二选一。
type ConcatItem struct {
	RID         string
	Placeholder *Placeholder
}

// ConcatParams 拼接参数。全部可省略：规格取第一个真实段；全是占位时必须给 Width/Height。
type ConcatParams struct {
	Width, Height int
	Fps           int
	SampleRate    int
	// DropAudio 去除音轨。默认保留：缺音轨的段（含占位）补静音，保证时间轴对齐。
	DropAudio bool
}

// ConcatEntry 成片时间轴上的一项。
type ConcatEntry struct {
	Index    int     `json:"index"`
	Kind     string  `json:"kind"` // source | placeholder
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
}

// ConcatResult 拼接结果：成片与时间轴。
type ConcatResult struct {
	Resource
	Timeline []ConcatEntry
}

// ConcatItems 有序拼接（POST /v1/mediasvc/video/concat）：1~6 项、总时长 ≤ 85 秒（80 秒 + 衔接重叠余量）；
// 规格不一致时统一到第一个真实段（或 p 指定的规格），不足画面黑边补齐、不裁切。
func (c *Client) ConcatItems(ctx context.Context, items []ConcatItem, p ConcatParams, idem string) (ConcatResult, error) {
	if len(items) < 1 || len(items) > MaxConcatItems {
		return ConcatResult{}, badRequest("request.segments_count", "拼接段数需要 1-6")
	}
	wire := make([]map[string]any, len(items))
	routeKey := ""
	for i, it := range items {
		switch {
		case it.RID != "" && it.Placeholder == nil:
			wire[i] = map[string]any{"source": map[string]string{"rid": it.RID}}
			if routeKey == "" {
				routeKey = it.RID
			}
		case it.RID == "" && it.Placeholder != nil:
			wire[i] = map[string]any{"placeholder": it.Placeholder}
		default:
			return ConcatResult{}, badRequest("request.item_exactly_one", "每段需要 rid 或占位二选一")
		}
	}
	if routeKey == "" { // 全是占位：随便挑一个实例
		routeKey = "concat"
		if f := items[0].Placeholder.FontRid; f != "" {
			routeKey = f
		}
	}
	req := map[string]any{"items": wire, "purpose": PurposeIntermediate}
	if p.Width > 0 || p.Height > 0 {
		req["width"], req["height"] = p.Width, p.Height
	}
	if p.Fps > 0 {
		req["fps"] = p.Fps
	}
	if p.SampleRate > 0 {
		req["sample_rate"] = p.SampleRate
	}
	if p.DropAudio {
		req["audio"] = "drop"
	}
	idem = pick(idem, "video/concat", req)
	body, _ := json.Marshal(req)
	raw, err := c.post(ctx, routeKey, "/v1/mediasvc/video/concat", body, "application/json", idem)
	if err != nil {
		return ConcatResult{}, err
	}
	var resp struct {
		mediaResult
		Timeline []ConcatEntry `json:"timeline"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ConcatResult{}, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	if resp.Object.RID == "" {
		return ConcatResult{}, fmt.Errorf("媒体服务返回缺少 rid")
	}
	return ConcatResult{Resource: resp.resource(), Timeline: resp.Timeline}, nil
}

// Concat 按顺序拼接若干 rid，返回成片 rid。
func (c *Client) Concat(ctx context.Context, rids []string, p ConcatParams, idem string) (Resource, error) {
	items := make([]ConcatItem, len(rids))
	for i, r := range rids {
		items[i] = ConcatItem{RID: r}
	}
	res, err := c.ConcatItems(ctx, items, p, idem)
	return res.Resource, err
}

// MaxMuxRanges 贴回原声一次最多的区间数（与服务端 app/media/api_video_mux_audio.go 一致）。
const MaxMuxRanges = 16

// MuxResult 贴回原声的结果：成片与实际口径。
type MuxResult struct {
	Resource
	AudioSeconds  float64 // 原片真正提供的声音秒数
	PaddedSeconds float64 // 补静音的秒数（成片比原片声音长、或区间落在原片之外）
}

// MuxAudio 把原片（sourceRid）里 ranges 各区间的音频按顺序相接，对齐到视频（videoRid）的时间轴后替换它的音轨
// （POST /v1/mediasvc/video/mux-audio；视频流拷贝，不重编码）。比视频短的末尾补静音（不循环），比视频长的截断；
// 视频原有音轨丢弃。原片没有音轨 → 422（Check=audio.no_audio_stream），调用方应回落到不贴回。
func (c *Client) MuxAudio(ctx context.Context, videoRid, sourceRid string, ranges []TimeRange, idem string) (MuxResult, error) {
	if len(ranges) < 1 || len(ranges) > MaxMuxRanges {
		return MuxResult{}, badRequest("request.ranges_count", "贴回原声的区间数需要 1-16")
	}
	for _, r := range ranges {
		if !(r.End > r.Start) || r.Start < 0 {
			return MuxResult{}, badRequest("request.segment_range", "区间不合法")
		}
	}
	req := map[string]any{"video": map[string]string{"rid": videoRid}, "audio": map[string]string{"rid": sourceRid},
		"ranges": ranges, "purpose": PurposeIntermediate}
	idem = pick(idem, "video/mux-audio", req)
	body, _ := json.Marshal(req)
	raw, err := c.post(ctx, videoRid, "/v1/mediasvc/video/mux-audio", body, "application/json", idem)
	if err != nil {
		return MuxResult{}, err
	}
	var resp struct {
		mediaResult
		AudioSeconds  float64 `json:"audio_seconds"`
		PaddedSeconds float64 `json:"padded_seconds"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return MuxResult{}, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	if resp.Object.RID == "" {
		return MuxResult{}, fmt.Errorf("媒体服务返回缺少 rid")
	}
	return MuxResult{Resource: resp.resource(), AudioSeconds: resp.AudioSeconds, PaddedSeconds: resp.PaddedSeconds}, nil
}

// 改画幅（Reframe）的画幅与模式。
const (
	ReframePad  = "pad"  // 原画面等比缩放居中，空位用同画面高斯模糊铺满
	ReframeCrop = "crop" // 铺满目标画幅后居中裁切
)

// ReframeRatios 支持的目标画幅（与服务端 app/media/api_video_reframe.go 一致）。
var ReframeRatios = []string{"9:16", "1:1", "16:9", "3:4", "4:3"}

// Reframe 把成片改成目标画幅（POST /v1/mediasvc/video/reframe，新 rid）：输出长边 = 源长边（上限 3840）、宽高偶数、
// 音轨原样拷贝。mode 空 = pad。画幅 / 模式不合法在客户端直接拒（Check=request.ratio / request.mode）。
func (c *Client) Reframe(ctx context.Context, rid, ratio, mode, idem string) (Resource, error) {
	if !slices.Contains(ReframeRatios, ratio) {
		return Resource{}, badRequest("request.ratio", "不支持的画幅")
	}
	if mode == "" {
		mode = ReframePad
	}
	if mode != ReframePad && mode != ReframeCrop {
		return Resource{}, badRequest("request.mode", "改画幅模式只能是 pad 或 crop")
	}
	req := map[string]any{"source": map[string]string{"rid": rid}, "ratio": ratio, "mode": mode, "purpose": PurposeIntermediate}
	idem = pick(idem, "video/reframe", req)
	body, _ := json.Marshal(req)
	raw, err := c.post(ctx, rid, "/v1/mediasvc/video/reframe", body, "application/json", idem)
	if err != nil {
		return Resource{}, err
	}
	var resp mediaResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Resource{}, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	if resp.Object.RID == "" {
		return Resource{}, fmt.Errorf("媒体服务返回缺少 rid")
	}
	return resp.resource(), nil
}

// TextBox 文字框，相对画面 0~1。
type TextBox struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// TextStroke 描边（像素）。
type TextStroke struct {
	Color string  `json:"color,omitempty"`
	Width float64 `json:"width"`
}

// TextShadow 阴影偏移（像素）；Dx、Dy 都为 0 取 2、2。
type TextShadow struct {
	Color string `json:"color,omitempty"`
	Dx    int    `json:"dx,omitempty"`
	Dy    int    `json:"dy,omitempty"`
}

// TextSlot 一个文字槽位（视频与图片同一份规格）。自动折行、按框缩放、超长以「…」省略，保证不溢出框。
type TextSlot struct {
	Text       string      `json:"text"`
	Box        TextBox     `json:"box"`
	FontRid    string      `json:"font_rid,omitempty"` // 含中文必须给
	Size       int         `json:"size,omitempty"`     // 最大字号（像素）；0 = 按框高自适应
	MinSize    int         `json:"min_size,omitempty"`
	Color      string      `json:"color,omitempty"`  // #RGB / #RRGGBB / #RRGGBBAA，默认白
	Align      string      `json:"align,omitempty"`  // left|center|right，默认 center
	Valign     string      `json:"valign,omitempty"` // top|middle|bottom，默认 middle
	Stroke     *TextStroke `json:"stroke,omitempty"`
	Shadow     *TextShadow `json:"shadow,omitempty"`
	MaxLines   int         `json:"max_lines,omitempty"`
	LineHeight float64     `json:"line_height,omitempty"` // 行高倍数，默认 1.2
	// Start/End 仅视频：显示起止（秒）；都为 nil = 全程。
	Start *float64 `json:"start,omitempty"`
	End   *float64 `json:"end,omitempty"`
}

func checkSlots(slots []TextSlot) error {
	if len(slots) < 1 || len(slots) > MaxTextSlots {
		return badRequest("request.slots_count", "文字槽位数量需要 1-16")
	}
	return nil
}

// OverlayText 视频叠字（POST /v1/mediasvc/video/overlay-text）：重编码，保留音轨。
func (c *Client) OverlayText(ctx context.Context, rid string, slots []TextSlot, idem string) (Resource, error) {
	return c.textOp(ctx, "/v1/mediasvc/video/overlay-text", "video/overlay-text", rid, slots, idem)
}

// ImageText 图片叠字（POST /v1/mediasvc/image/text）：输出与输入同格式（PNG / JPEG）。
func (c *Client) ImageText(ctx context.Context, rid string, slots []TextSlot, idem string) (Resource, error) {
	return c.textOp(ctx, "/v1/mediasvc/image/text", "image/text", rid, slots, idem)
}

func (c *Client) textOp(ctx context.Context, path, method, rid string, slots []TextSlot, idem string) (Resource, error) {
	if err := checkSlots(slots); err != nil {
		return Resource{}, err
	}
	idem = pick(idem, method, rid, slots, PurposeIntermediate)
	body, _ := json.Marshal(map[string]any{"source": map[string]string{"rid": rid}, "slots": slots, "purpose": PurposeIntermediate})
	raw, err := c.post(ctx, rid, path, body, "application/json", idem)
	if err != nil {
		return Resource{}, err
	}
	return parseResource(raw)
}
