package media

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// image/mark：在图上画红色标记（对齐 Java IdentifyMarkedImage 的样式）。
// 解码交给 ffmpeg（PNG / JPEG / WebP，-noautorotate），描边在 Go 里按距离场抗锯齿。

const (
	markJPEGQuality = 95 // Java 0.95f
)

var markShapes = map[string]bool{"auto": true, "circle": true, "dot": true, "rect": true, "polygon": true}

// parseMarkColor 接受 #RGB / #RRGGBB（可省略 #）；空串是 Java 的纯红。
func parseMarkColor(v string) (color.NRGBA, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "#")
	if v == "" {
		return color.NRGBA{R: 255, A: 255}, true
	}
	if len(v) == 3 {
		v = string([]byte{v[0], v[0], v[1], v[1], v[2], v[2]})
	}
	if len(v) != 6 {
		return color.NRGBA{}, false
	}
	n, err := strconv.ParseUint(v, 16, 32)
	if err != nil {
		return color.NRGBA{}, false
	}
	return color.NRGBA{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n), A: 255}, true
}

func checkMarkReq(req imageMarkReq) error {
	if len(req.Marks) < 1 || len(req.Marks) > maxMarkPoints {
		return invalid("request.marks_count")
	}
	if req.Shape != "" && !markShapes[req.Shape] {
		return invalid("request.shape")
	}
	if _, ok := parseMarkColor(req.Color); !ok {
		return invalid("request.color")
	}
	for _, m := range req.Marks {
		if _, ok := parseMarkColor(m.Color); !ok {
			return invalid("request.color")
		}
		if m.Radius < 0 || m.Radius > 8192 {
			return invalid("request.radius")
		}
	}
	if req.LineWidth < 0 || req.LineWidth > 256 {
		return invalid("request.line_width")
	}
	if req.Format != "" && req.Format != "jpeg" && req.Format != "png" {
		return invalid("request.format")
	}
	if req.Shape == "rect" && len(req.Marks) != 2 {
		return invalid("request.rect_needs_two_marks")
	}
	if req.Shape == "polygon" && len(req.Marks) < 3 {
		return invalid("request.polygon_needs_three_marks")
	}
	return nil
}

// resolveShape 与 Java 同一规则：1 点 → 圆，2 点 → 矩形，≥3 点 → 闭合折线。
func resolveShape(shape string, n int) string {
	if shape != "" && shape != "auto" {
		return shape
	}
	switch {
	case n == 1:
		return "circle"
	case n == 2:
		return "rect"
	default:
		return "polygon"
	}
}

// imageMark 在图上画标记，输出 JPEG(95)（默认，同 Java）或 PNG。
// 坐标是像素（原点左上角），超出图像的点夹到 [0, 宽-1] × [0, 高-1]（同 Java 的 clamp）。
func (s *Server) imageMark(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req imageMarkReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose), checkMarkReq(req)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, _, err := s.fetchImageSource(ctx, dir, "src", req.Source)
		if err != nil {
			return nil, err
		}
		decoded := workFile(dir, "decoded.png")
		if err := s.svc.runLight(ctx, s.svc.ffmpeg(), "-nostdin", "-y", "-threads", s.svc.threads(), "-noautorotate", "-i", path,
			"-map", "0:v:0", "-frames:v", "1", "-pix_fmt", "rgba", "-update", "1", decoded); err != nil {
			return nil, imageFailure(ctx, err, "image.mark_decode_failed")
		}
		format := req.Format
		if format == "" {
			format = "jpeg"
		}
		ext := ".jpg"
		if format == "png" {
			ext = ".png"
		}
		out := workFile(dir, "marked"+ext)
		err = s.svc.computeLight(ctx, func() error { return renderMarks(decoded, out, format, req) })
		if err != nil {
			return nil, err
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

// renderMarks 读 PNG，铺白底（JPEG 与 Java 一致；PNG 保留透明度），画标记，写出。
func renderMarks(in, out, format string, req imageMarkReq) error {
	f, err := os.Open(in)
	if err != nil {
		return internal(err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return newAPIError(codeSourceRejected, "image.mark_decode_failed")
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
	if format == "jpeg" {
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		draw.Draw(canvas, canvas.Bounds(), src, b.Min, draw.Over)
	} else {
		draw.Draw(canvas, canvas.Bounds(), src, b.Min, draw.Src)
	}

	short := min(w, h)
	shape := resolveShape(req.Shape, len(req.Marks))
	base, _ := parseMarkColor(req.Color)
	pts := make([]markPt, len(req.Marks))
	for i, m := range req.Marks {
		c := base
		if m.Color != "" {
			c, _ = parseMarkColor(m.Color)
		}
		radius := m.Radius
		if radius == 0 {
			radius = max(20, int(float64(short)*0.03)) // Java：max(20, int(短边 × 0.03))
		}
		pts[i] = markPt{x: float64(min(max(m.X, 0), w-1)), y: float64(min(max(m.Y, 0), h-1)), r: float64(radius), c: c}
	}
	switch shape {
	case "circle", "dot":
		for _, p := range pts {
			lw := float64(req.LineWidth)
			if lw == 0 {
				lw = float64(max(3, int(p.r*0.12))) // Java：max(3, int(半径 × 0.12))
			}
			if shape == "dot" {
				fillDisc(canvas, p.x, p.y, p.r, p.c)
			} else {
				strokeRing(canvas, p.x, p.y, p.r, lw, p.c)
			}
		}
	default:
		lw := float64(req.LineWidth)
		if lw == 0 {
			lw = float64(max(3, int(float64(short)*0.005))) // Java：max(3, int(短边 × 0.005))
		}
		c := pts[0].c
		if shape == "rect" {
			x0, x1 := math.Min(pts[0].x, pts[1].x), math.Max(pts[0].x, pts[1].x)
			y0, y1 := math.Min(pts[0].y, pts[1].y), math.Max(pts[0].y, pts[1].y)
			corners := []markPt{{x: x0, y: y0}, {x: x1, y: y0}, {x: x1, y: y1}, {x: x0, y: y1}}
			for i := range corners {
				strokeSegment(canvas, corners[i], corners[(i+1)%4], lw, c)
			}
		} else {
			for i := range pts {
				strokeSegment(canvas, pts[i], pts[(i+1)%len(pts)], lw, c)
			}
		}
	}

	o, err := os.Create(out)
	if err != nil {
		return internal(err)
	}
	defer o.Close()
	if format == "jpeg" {
		err = jpeg.Encode(o, canvas, &jpeg.Options{Quality: markJPEGQuality})
	} else {
		err = png.Encode(o, canvas)
	}
	if err != nil {
		return internal(err)
	}
	return o.Close()
}

type markPt struct {
	x, y, r float64
	c       color.NRGBA
}

// blend 把颜色按覆盖率 a（0..1）混进目标像素。
func blend(img *image.NRGBA, x, y int, c color.NRGBA, a float64) {
	if a <= 0 || !image.Pt(x, y).In(img.Rect) {
		return
	}
	a = math.Min(a, 1) * float64(c.A) / 255
	i := img.PixOffset(x, y)
	p := img.Pix[i : i+4 : i+4]
	// 目标可能带 alpha（PNG 输出）：按 over 合成
	da := float64(p[3]) / 255
	oa := a + da*(1-a)
	if oa <= 0 {
		return
	}
	mix := func(s, d uint8) uint8 {
		return uint8(math.Round((float64(s)*a + float64(d)*da*(1-a)) / oa))
	}
	p[0], p[1], p[2], p[3] = mix(c.R, p[0]), mix(c.G, p[1]), mix(c.B, p[2]), uint8(math.Round(oa*255))
}

// paint 遍历包围盒内每个像素中心，cov 返回该点的覆盖率。
func paint(img *image.NRGBA, x0, y0, x1, y1 float64, c color.NRGBA, cov func(px, py float64) float64) {
	b := img.Rect
	ix0, iy0 := max(b.Min.X, int(math.Floor(x0))), max(b.Min.Y, int(math.Floor(y0)))
	ix1, iy1 := min(b.Max.X-1, int(math.Ceil(x1))), min(b.Max.Y-1, int(math.Ceil(y1)))
	for y := iy0; y <= iy1; y++ {
		for x := ix0; x <= ix1; x++ {
			blend(img, x, y, c, cov(float64(x)+0.5, float64(y)+0.5))
		}
	}
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// 点 (x, y) 指像素格 (x, y)，几何中心取 (x+0.5, y+0.5)。
func fillDisc(img *image.NRGBA, cx, cy, r float64, c color.NRGBA) {
	cx, cy = cx+0.5, cy+0.5
	paint(img, cx-r-1, cy-r-1, cx+r+1, cy+r+1, c, func(px, py float64) float64 {
		return clamp01(r - math.Hypot(px-cx, py-cy) + 0.5)
	})
}

func strokeRing(img *image.NRGBA, cx, cy, r, lw float64, c color.NRGBA) {
	cx, cy = cx+0.5, cy+0.5
	half := lw / 2
	paint(img, cx-r-half-1, cy-r-half-1, cx+r+half+1, cy+r+half+1, c, func(px, py float64) float64 {
		return clamp01(half - math.Abs(math.Hypot(px-cx, py-cy)-r) + 0.5)
	})
}

func strokeSegment(img *image.NRGBA, a, b markPt, lw float64, c color.NRGBA) {
	ax, ay, bx, by := a.x+0.5, a.y+0.5, b.x+0.5, b.y+0.5
	half := lw / 2
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	paint(img, math.Min(ax, bx)-half-1, math.Min(ay, by)-half-1, math.Max(ax, bx)+half+1, math.Max(ay, by)+half+1, c,
		func(px, py float64) float64 {
			t := 0.0
			if l2 > 0 {
				t = clamp01(((px-ax)*dx + (py-ay)*dy) / l2)
			}
			return clamp01(half - math.Hypot(px-(ax+t*dx), py-(ay+t*dy)) + 0.5)
		})
}
