package mediasvc

// 图像相关调用：conform / crop / grid / resize。
//
// 服务端真实契约（以 app/media 代码为准）与 docs/api/media-service.md、与任务设想的出入：
//   - image/conform 只有 rule{min_long_side,max_long_side,mime_types}：没有 quality；目标格式用 mime_types 表达，
//     且不需要缩放、格式已满足时原样落桶。ConformParams.Quality 因此被忽略。
//   - image/crop 请求是 boxes[{x,y,width,height}] 或 grid{rows,cols}，只输出 PNG，没有 label / pad / 最小短边 / format / quality。
//     所以 pad 在客户端按原图尺寸算好扩框（Probe 取宽高），最小短边与 jpeg 输出通过对结果再调 image/resize 完成。
//   - image/resize 的 format 必填（png|jpeg），不保比例，没有 quality。
//   - image/grid 响应是单个 MediaResult，输出 PNG；可选 gap / row_height / max_side。
//   - resources/urls 一次只签一个 rid，且必须带 store（见 resources.go），文档示例没有强调。
//   - 以上图像接口都要求 purpose；签名里没有该参数，这里统一用 intermediate（work 类私有桶），
//     需要入素材库时再 CopyToLibrary。
//   - 文档里字节上传写作 ingest/upload，请求体为原始字节，purpose 在 query 上（与实现一致）。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
)

// ConformParams 图片规整。长边落进 [LongEdgeMin, LongEdgeMax]（0 表示不限）后等比缩放。
type ConformParams struct {
	LongEdgeMin, LongEdgeMax int
	// Format 目标格式：jpeg / png / webp，空表示保持原格式。
	Format string
	// Quality 服务端 image/conform 不支持，当前被忽略（保留字段，便于服务端支持后接入）。
	Quality int
}

func mimeOfFormat(f string) string {
	switch strings.ToLower(strings.TrimPrefix(f, "image/")) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	}
	return ""
}

// Conform 把输入压到长边区间并按需转格式（POST /v1/mediasvc/image/conform）。
// 典型用法：Conform(rid, ConformParams{LongEdgeMax: 1600, Format: "jpeg"}, "")。
func (c *Client) Conform(ctx context.Context, rid string, p ConformParams, idem string) (Resource, error) {
	idem = pick(idem, "image/conform", rid, p.LongEdgeMin, p.LongEdgeMax, mimeOfFormat(p.Format), PurposeIntermediate)
	rule := map[string]any{"min_long_side": p.LongEdgeMin, "max_long_side": p.LongEdgeMax, "mime_types": []string{}}
	if m := mimeOfFormat(p.Format); m != "" {
		rule["mime_types"] = []string{m}
	}
	body, _ := json.Marshal(map[string]any{
		"source": map[string]string{"rid": rid}, "rule": rule, "purpose": PurposeIntermediate,
	})
	raw, err := c.post(ctx, rid, "/v1/mediasvc/image/conform", body, "application/json", idem)
	if err != nil {
		return Resource{}, err
	}
	return parseResource(raw)
}

// Resize 精确缩放到 w×h，不保比例（POST /v1/mediasvc/image/resize，输出 PNG）。
func (c *Client) Resize(ctx context.Context, rid string, w, h int, idem string) (Resource, error) {
	return c.resize(ctx, rid, w, h, "png", idem)
}

func (c *Client) resize(ctx context.Context, rid string, w, h int, format, idem string) (Resource, error) {
	idem = pick(idem, "image/resize", rid, w, h, format, PurposeIntermediate)
	body, _ := json.Marshal(map[string]any{
		"source": map[string]string{"rid": rid}, "width": w, "height": h, "format": format, "purpose": PurposeIntermediate,
	})
	raw, err := c.post(ctx, rid, "/v1/mediasvc/image/resize", body, "application/json", idem)
	if err != nil {
		return Resource{}, err
	}
	return parseResource(raw)
}

// GridParams 宫格参数，零值取服务端默认（gap 20、row_height 960、max_side 1920）。
type GridParams struct {
	Gap       *int // 0 合法（无间距），所以用指针区分省略
	RowHeight int
	MaxSide   int
}

// Grid 1–9 张图合成自适应宫格（POST /v1/mediasvc/image/grid，PNG 白底）。任一张取不到则整体失败。
func (c *Client) Grid(ctx context.Context, rids []string, p GridParams, idem string) (Resource, error) {
	if len(rids) < 1 || len(rids) > 9 {
		return Resource{}, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "宫格需要 1-9 张图"}
	}
	gap := -1
	if p.Gap != nil {
		gap = *p.Gap
	}
	idem = pick(idem, "image/grid", rids, gap, p.RowHeight, p.MaxSide, PurposeIntermediate)
	srcs := make([]map[string]string, len(rids))
	for i, r := range rids {
		srcs[i] = map[string]string{"rid": r}
	}
	req := map[string]any{"sources": srcs, "purpose": PurposeIntermediate}
	if p.Gap != nil {
		req["gap"] = *p.Gap
	}
	if p.RowHeight > 0 {
		req["row_height"] = p.RowHeight
	}
	if p.MaxSide > 0 {
		req["max_side"] = p.MaxSide
	}
	body, _ := json.Marshal(req)
	raw, err := c.post(ctx, rids[0], "/v1/mediasvc/image/grid", body, "application/json", idem)
	if err != nil {
		return Resource{}, err
	}
	return parseResource(raw)
}

// PixelBox 像素矩形（原图坐标）。Label 原样回传到对应 Resource.Label。
type PixelBox struct {
	X, Y, W, H int
	Label      string
}

// CropParams 切图参数。
type CropParams struct {
	// Pad 四周留白比例：相对框自身宽、高向外各扩 Pad（0.04 即每边 4%），扩后不超出原图。
	Pad float64
	// MinShortEdge 短边不足时按比例放大到该值（如 256）；0 不放大。
	MinShortEdge int
	// Format 输出格式 png（默认）/ jpeg。服务端 crop 只出 PNG，jpeg 经一次 resize 重编码。
	Format string
	// Quality 服务端不支持，被忽略。
	Quality int
}

// PadBox 按 pad 比例向四周扩框，并裁回 [0,imgW]×[0,imgH]。输入框越界也会先被裁进图内。
// 结果宽高至少 1 像素（框完全在图外时退化为图内最近的 1×1）。
func PadBox(b PixelBox, imgW, imgH int, pad float64) PixelBox {
	if pad < 0 {
		pad = 0
	}
	px := int(math.Round(float64(b.W) * pad))
	py := int(math.Round(float64(b.H) * pad))
	x0, y0 := b.X-px, b.Y-py
	x1, y1 := b.X+b.W+px, b.Y+b.H+py
	x0, x1 = clampInt(x0, 0, imgW-1), clampInt(x1, 1, imgW)
	y0, y1 = clampInt(y0, 0, imgH-1), clampInt(y1, 1, imgH)
	if x1 <= x0 {
		x1 = x0 + 1
	}
	if y1 <= y0 {
		y1 = y0 + 1
	}
	return PixelBox{X: x0, Y: y0, W: x1 - x0, H: y1 - y0, Label: b.Label}
}

// BoxFromNorm 把归一化坐标（相对原图，各分量先裁到 [0,1]）换算成像素框。
func BoxFromNorm(x, y, w, h float64, imgW, imgH int) PixelBox {
	cl := func(v float64) float64 { return math.Min(1, math.Max(0, v)) }
	x0, y0 := cl(x), cl(y)
	x1, y1 := cl(x+w), cl(y+h)
	return PadBox(PixelBox{
		X: int(math.Round(x0 * float64(imgW))), Y: int(math.Round(y0 * float64(imgH))),
		W: int(math.Round((x1 - x0) * float64(imgW))), H: int(math.Round((y1 - y0) * float64(imgH))),
	}, imgW, imgH, 0)
}

// UpscaleSize 短边不足 minShort 时按比例放大，返回目标宽高与倍数；无需放大返回原尺寸与 1。
func UpscaleSize(w, h, minShort int) (int, int, float64) {
	short := min(w, h)
	if minShort <= 0 || short <= 0 || short >= minShort {
		return w, h, 1
	}
	scale := float64(minShort) / float64(short)
	return max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale))), scale
}

func clampInt(v, lo, hi int) int { return min(hi, max(lo, v)) }

// maxResizeSide 与服务端 image/resize 的单边上限一致。
const maxResizeSide = 16384

// Crop 按像素框切图，返回与 boxes 顺序一致的 Resource。
//
// 步骤：Probe 取原图宽高 → 每框 PadBox 扩框 → image/crop（PNG）→ 对短边 < MinShortEdge 的结果
// （或 Format=jpeg 的全部结果）调 image/resize 放大/重编码。每个 resize 的幂等键由 crop 结果 rid 与目标尺寸派生。
func (c *Client) Crop(ctx context.Context, rid string, boxes []PixelBox, p CropParams, idem string) ([]Resource, error) {
	if len(boxes) == 0 {
		return nil, nil
	}
	if len(boxes) > 64 {
		return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "单次最多切 64 个框"}
	}
	info, err := c.Probe(ctx, rid)
	if err != nil {
		return nil, err
	}
	if info.Width < 1 || info.Height < 1 {
		return nil, &Error{Status: http.StatusUnprocessableEntity, Code: "source_rejected", Message: "无法取得原图尺寸"}
	}
	rects := make([]PixelBox, len(boxes))
	wire := make([]map[string]int, len(boxes))
	for i, b := range boxes {
		rects[i] = PadBox(b, info.Width, info.Height, p.Pad)
		wire[i] = map[string]int{"x": rects[i].X, "y": rects[i].Y, "width": rects[i].W, "height": rects[i].H}
	}
	idem = pick(idem, "image/crop", rid, wire, PurposeIntermediate)
	body, _ := json.Marshal(map[string]any{"source": map[string]string{"rid": rid}, "boxes": wire, "purpose": PurposeIntermediate})
	raw, err := c.post(ctx, rid, "/v1/mediasvc/image/crop", body, "application/json", idem)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Crops []mediaResult `json:"crops"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("媒体服务返回无法解析: %w", err)
	}
	if len(resp.Crops) != len(boxes) {
		return nil, fmt.Errorf("媒体服务返回 %d 张切图，期望 %d", len(resp.Crops), len(boxes))
	}
	format := "png"
	if f := mimeOfFormat(p.Format); f == "image/jpeg" {
		format = "jpeg"
	}
	out := make([]Resource, len(boxes))
	for i, m := range resp.Crops {
		res := m.resource()
		res.Label = boxes[i].Label
		tw, th, scale := UpscaleSize(rects[i].W, rects[i].H, p.MinShortEdge)
		if tw > maxResizeSide || th > maxResizeSide {
			return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "放大后超过单边上限"}
		}
		if scale > 1 || format == "jpeg" {
			rr, err := c.resize(ctx, res.RID, tw, th, format, "")
			if err != nil {
				return nil, err
			}
			rr.Label = res.Label
			rr.Parents = []string{res.RID}
			res = rr
		}
		out[i] = res
	}
	return out, nil
}
