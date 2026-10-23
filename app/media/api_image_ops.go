package media

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 图像四件套（契约 /v1/mediasvc/image/{watermark,grid,crop,mark}）：把 Java 自研的 Java2D 图像计算迁到媒体服务。
// 版式与取整逐项对齐 Java 现行实现：
//   - watermark ← ImageWatermarkService.apply + WatermarkProperties（右下角一组：logo 与文字）；
//   - grid      ← GridImageService.merge（自适应宫格：同行等高、整行居中、白底、最长边限制）；
//   - crop      ← HitVideoCropHandler（像素整数切块；另给"按行列均分"的便捷写法）；
//   - mark      ← IdentifyMarkedImage（红色圆 / 矩形 / 折线，JPEG 95）。
// 解码、缩放、合成都交给 ffmpeg（与 image/resize 同一套引擎，支持 PNG / JPEG / WebP），mark 的描边在 Go 里做。
// 输入一律 -noautorotate：Java 的 ImageIO 不读 EXIF 方向，坐标与排版以"编码像素"为准，才能与现行结果一致。
// 输出经 storeFile 落桶，按 purpose 选类别（intermediate → work，third_party → deliver，asset_library → asset），与 image/resize 一致。

const (
	// 水印默认值：对齐 Java WatermarkProperties（v1 的 logo_scale_percent=15、margin=40、text_size=56、t_85）
	wmDefaultLogoScalePercent = 15
	wmDefaultMargin           = 40
	wmDefaultTextSize         = 56
	wmDefaultOpacityPercent   = 85
	wmShadowOffset            = 2           // Java SHADOW_OFFSET
	wmShadowAlpha             = 0.6         // Java new Color(0,0,0,153)
	wmMaxTextRunes            = 200         // 展示水印是一行短文案
	wmFont                    = "Sans:bold" // Java Font.SANS_SERIF + BOLD；字体族由部署镜像的 fontconfig 决定（镜像装 fonts-noto-cjk）

	// 宫格默认值：对齐 Java GridImageService
	gridDefaultGap     = 20
	gridDefaultRowH    = 960
	gridDefaultMaxSide = 1920
	gridMaxImages      = 9

	maxCropBoxes  = 64
	maxMarkPoints = 64
)

// ---- 请求类型 ----

type imageWatermarkReq struct {
	Source SourceRef  `json:"source"`
	Logo   *SourceRef `json:"logo,omitempty"`
	Text   string     `json:"text,omitempty"`
	// 以下均可省略，省略取 Java WatermarkProperties 的默认值
	LogoScalePercent   int    `json:"logo_scale_percent,omitempty"`
	Margin             *int   `json:"margin,omitempty"`
	TextSize           int    `json:"text_size,omitempty"`
	TextOpacityPercent *int   `json:"text_opacity_percent,omitempty"`
	Purpose            string `json:"purpose"`
}

type imageGridReq struct {
	Sources []SourceRef `json:"sources"`
	// 以下均可省略，省略取 Java GridImageService 的常量
	Gap       *int   `json:"gap,omitempty"` // 0 合法（无间距），所以用指针区分省略
	RowHeight int    `json:"row_height,omitempty"`
	MaxSide   int    `json:"max_side,omitempty"`
	Purpose   string `json:"purpose"`
}

type cropBox struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type cropGrid struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

type imageCropReq struct {
	Source SourceRef `json:"source"`
	// Boxes 与 Grid 二选一。
	Boxes   []cropBox `json:"boxes,omitempty"`
	Grid    *cropGrid `json:"grid,omitempty"`
	Purpose string    `json:"purpose"`
}

type imageCropResp struct {
	Crops []MediaResult `json:"crops"`
}

type markPoint struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Radius int    `json:"radius,omitempty"`
	Color  string `json:"color,omitempty"`
}

type imageMarkReq struct {
	Source    SourceRef   `json:"source"`
	Marks     []markPoint `json:"marks"`
	Shape     string      `json:"shape,omitempty"`
	Color     string      `json:"color,omitempty"`
	LineWidth int         `json:"line_width,omitempty"`
	Format    string      `json:"format,omitempty"`
	Purpose   string      `json:"purpose"`
}

// ---- 公共步骤 ----

// fetchImageSource 取回一张图并检查：必须是 PNG / JPEG / WebP（文件头），可解码，像素不超上限。
// sub 是 dir 下的子目录名（多输入时避免同名文件互相覆盖）。
func (s *Server) fetchImageSource(ctx context.Context, dir, sub string, src SourceRef) (string, MediaMeta, error) {
	d := filepath.Join(dir, sub)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", MediaMeta{}, internal(err)
	}
	path, err := s.sourceFn(src)(ctx, d)
	if err != nil {
		return "", MediaMeta{}, err
	}
	if !isImageFile(path) {
		return "", MediaMeta{}, newAPIError(codeSourceRejected, "image.not_image")
	}
	meta, err := s.probeMeta(ctx, path)
	if err != nil {
		return "", MediaMeta{}, err
	}
	if meta.Width < 1 || meta.Height < 1 {
		return "", MediaMeta{}, newAPIError(codeSourceRejected, "image.not_image")
	}
	return path, meta, nil
}

// imageFormatOf 只认 PNG / JPEG（水印沿用 Java SUPPORTED = png/jpg/jpeg）；其它格式返回空串。
func imageFormatOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := f.Read(head)
	head = head[:n]
	switch {
	case len(head) >= 8 && string(head[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "jpeg"
	}
	return ""
}

// imageFailure 图像计算失败：沿用 ffmpegFailure 的分类（中断 → 503，缺 ffmpeg → 500，其余 → 422 素材问题）。
func imageFailure(ctx context.Context, err error, check string) error {
	// 原始 ffmpeg 错误（含 stderr 尾部）只进日志，带追踪号；回给调用方的只有固定判据
	log.Printf("media 图像处理失败 trace_id=%s check=%s: %v", TraceIDFromContext(ctx), check, err)
	return ffmpegFailure(ctx, err, check)
}

// ---- image/watermark ----

func checkWatermarkReq(req imageWatermarkReq) error {
	if req.Logo == nil && strings.TrimSpace(req.Text) == "" {
		return invalid("request.watermark_empty") // logo 与文字都没有，画出来与原图一样
	}
	if req.Logo != nil {
		if err := checkSource(*req.Logo); err != nil {
			return err
		}
	}
	if n := utf8.RuneCountInString(req.Text); n > wmMaxTextRunes || strings.ContainsAny(req.Text, "\r\n\x00") {
		return invalid("request.text")
	}
	if req.LogoScalePercent < 0 || req.LogoScalePercent > 100 {
		return invalid("request.logo_scale_percent")
	}
	if req.Margin != nil && (*req.Margin < 0 || *req.Margin > 4096) {
		return invalid("request.margin")
	}
	if req.TextSize < 0 || req.TextSize > 512 {
		return invalid("request.text_size")
	}
	if req.TextOpacityPercent != nil && (*req.TextOpacityPercent < 0 || *req.TextOpacityPercent > 100) {
		return invalid("request.text_opacity_percent")
	}
	return nil
}

// imageWatermark 给图片加展示水印：右下角一组——logo 贴右下、文字在 logo 左侧（带 2px 阴影）。输出格式与输入一致（PNG / JPEG）。
func (s *Server) imageWatermark(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req imageWatermarkReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose), checkWatermarkReq(req)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, meta, err := s.fetchImageSource(ctx, dir, "src", req.Source)
		if err != nil {
			return nil, err
		}
		format := imageFormatOf(path)
		if format == "" {
			return nil, newAPIError(codeSourceRejected, "image.format_unsupported") // WebP 等：与 Java SUPPORTED 一致
		}
		spec := watermarkSpec{
			width: meta.Width, height: meta.Height, format: format, text: req.Text,
			logoScale: orDefault(req.LogoScalePercent, wmDefaultLogoScalePercent),
			margin:    wmDefaultMargin, textSize: orDefault(req.TextSize, wmDefaultTextSize), opacity: wmDefaultOpacityPercent,
		}
		if req.Margin != nil {
			spec.margin = *req.Margin
		}
		if req.TextOpacityPercent != nil {
			spec.opacity = *req.TextOpacityPercent
		}
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads(), "-noautorotate", "-i", path}
		if req.Logo != nil {
			logoPath, logoMeta, err := s.fetchImageSource(ctx, dir, "logo", *req.Logo)
			if err != nil {
				return nil, err
			}
			args = append(args, "-noautorotate", "-i", logoPath)
			spec.hasLogo, spec.logoW, spec.logoH = true, logoMeta.Width, logoMeta.Height
		}
		if strings.TrimSpace(req.Text) != "" {
			spec.textFile = workFile(dir, "wm.txt")
			if err := os.WriteFile(spec.textFile, []byte(req.Text), 0o600); err != nil {
				return nil, internal(err)
			}
		}
		ext := ".png"
		if format == "jpeg" {
			ext = ".jpg"
		}
		out := workFile(dir, "watermarked"+ext)
		args = append(args, "-filter_complex", spec.filter(), "-map", "[out]", "-frames:v", "1")
		if format == "jpeg" {
			args = append(args, "-q:v", "3")
		}
		args = append(args, "-update", "1", out)
		if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, imageFailure(ctx, err, "image.watermark_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// watermarkSpec 水印的版式参数。filter 只做字符串拼装，便于单测核对公式。
type watermarkSpec struct {
	width, height     int
	format            string
	hasLogo           bool
	logoW, logoH      int
	logoScale, margin int
	text, textFile    string
	textSize, opacity int
}

// logoSize 与 Java drawLogo 同一套整数公式：宽 = max(1, 图宽 × 百分比 / 100)，高 = max(1, logo 高 × 宽 / logo 宽)。
func (w watermarkSpec) logoSize() (int, int) {
	lw := max(1, w.width*w.logoScale/100)
	lh := max(1, w.logoH*lw/w.logoW)
	return lw, lh
}

func (w watermarkSpec) filter() string {
	var b strings.Builder
	cur := "base"
	if w.format == "jpeg" {
		// JPEG 没有 alpha：先铺白底（Java toDrawable：透明区域不能变黑块）
		fmt.Fprintf(&b, "color=c=white:s=%dx%d,format=rgba[bg];[0:v:0]format=rgba[fg];[bg][fg]overlay=format=auto[base]", w.width, w.height)
	} else {
		b.WriteString("[0:v:0]format=rgba[base]")
	}
	if w.hasLogo {
		// logo 贴右下角（底部与右边都留 margin）；文字排在它左边，见下面 drawtext 的 x
		lw, lh := w.logoSize()
		fmt.Fprintf(&b, ";[1:v:0]format=rgba,scale=%d:%d:flags=bilinear[lg];[%s][lg]overlay=x=W-w-%d:y=H-h-%d:format=auto[wl]", lw, lh, cur, w.margin, w.margin)
		cur = "wl"
	}
	if w.textFile != "" {
		// 位置对齐 Java drawText：x = max(margin, W - 文字宽 - margin)；基线 = max(ascent, H - margin)，drawtext 的 y 是行顶，故减 ascent。
		// 整层乘不透明度：白字 alpha = opacity，阴影 alpha = 0.6 × opacity（Java 先设 composite 再画 0.6 的黑）。
		op := float64(w.opacity) / 100
		// 文字先画在全透明画布上，再整层 over 到底图：直接在 rgba 底图上 drawtext 会把不透明像素的 alpha 也拉低（实测），
		// 而 Java 是在不透明画布上 SRC_OVER，结果仍不透明。层内用满不透明的白字，不透明度最后整层乘一次
		// （在层里写 fontcolor=white@0.85 会被 drawtext 的预乘与 overlay 各乘一遍，变成 0.72）。
		// 文字右边界：有 logo 时让到 logo 左侧（logo 宽 + 间距 margin）；否则贴右边
		rightInset := w.margin
		if w.hasLogo {
			lw, _ := w.logoSize()
			rightInset = w.margin + lw + w.margin
		}
		fmt.Fprintf(&b, ";color=c=white@0:s=%dx%d,format=rgba,drawtext=textfile=%s:expansion=none:font=%s:fontsize=%d:fontcolor=white:shadowx=%d:shadowy=%d:shadowcolor=black@%.2f:x=max(%d\\,w-text_w-%d):y=max(0\\,h-%d-ascent),colorchannelmixer=aa=%.4f[tl];[%s][tl]overlay=format=auto[wt]",
			w.width, w.height, filterQuote(w.textFile), filterQuote(wmFont), w.textSize, wmShadowOffset, wmShadowOffset, wmShadowAlpha, w.margin, rightInset, w.margin, op, cur)
		cur = "wt"
	}
	if w.format == "jpeg" {
		fmt.Fprintf(&b, ";[%s]format=yuvj420p[out]", cur)
	} else {
		fmt.Fprintf(&b, ";[%s]format=rgba[out]", cur)
	}
	return b.String()
}

// filterQuote 两层转义：先按"滤镜参数"层转义 \ ' :（值里的冒号不能被当成选项分隔符），
// 再按"滤镜图"层整体包进单引号（引号内的 ' 用 '\” 拼接）。
func filterQuote(v string) string {
	arg := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `:`, `\:`).Replace(v)
	return "'" + strings.ReplaceAll(arg, `'`, `'\''`) + "'"
}

// ---- image/grid ----

type gridPlacement struct {
	idx, x, y, w, h int
}

// gridLayout 自适应宫格排版（GridImageService.merge 的整数运算原样移植）。sizes 为各图原始宽高。
// 返回画布宽高、各图的落点与缩放后尺寸、最终输出宽高（最长边超过 maxSide 时等比缩小，取整同 Java：int(边 × scale)）。
func gridLayout(sizes [][2]int, rowH, gap, maxSide int) (canvasW, canvasH int, places []gridPlacement, outW, outH int) {
	n := len(sizes)
	scaled := func(i int) (int, int) { // resizeToHeight
		return max(1, int(int64(sizes[i][0])*int64(rowH)/int64(sizes[i][1]))), rowH
	}
	switch {
	case n == 1:
		canvasW, canvasH = sizes[0][0], sizes[0][1] // 单图不拼格，只限制最长边
		places = []gridPlacement{{0, 0, 0, canvasW, canvasH}}
	case n == 2:
		w0, _ := scaled(0)
		w1, _ := scaled(1)
		canvasW, canvasH = w0+w1+gap, rowH
		places = []gridPlacement{{0, 0, 0, w0, rowH}, {1, w0 + gap, 0, w1, rowH}}
	case n == 3:
		wt, _ := scaled(0)
		w1, _ := scaled(1)
		w2, _ := scaled(2)
		bottomW := w1 + w2 + gap
		canvasW, canvasH = max(wt, bottomW), rowH*2+gap
		bx := (canvasW - bottomW) / 2
		places = []gridPlacement{
			{0, (canvasW - wt) / 2, 0, wt, rowH},
			{1, bx, rowH + gap, w1, rowH},
			{2, bx + w1 + gap, rowH + gap, w2, rowH},
		}
	default:
		var rows [][]int // 4 张特例 2+2；其余每行至多 3 张
		if n == 4 {
			rows = [][]int{{0, 1}, {2, 3}}
		} else {
			for i := 0; i < n; i += 3 {
				row := []int{}
				for j := i; j < min(i+3, n); j++ {
					row = append(row, j)
				}
				rows = append(rows, row)
			}
		}
		widths := make([]int, len(rows))
		for r, row := range rows {
			rw := 0
			for _, idx := range row {
				w, _ := scaled(idx)
				rw += w + gap
			}
			widths[r] = rw - gap
			canvasW = max(canvasW, widths[r])
		}
		canvasH = rowH*len(rows) + gap*(len(rows)-1)
		y := 0
		for r, row := range rows {
			x := (canvasW - widths[r]) / 2
			for _, idx := range row {
				w, _ := scaled(idx)
				places = append(places, gridPlacement{idx, x, y, w, rowH})
				x += w + gap
			}
			y += rowH + gap
		}
	}
	canvasW, canvasH = max(1, canvasW), max(1, canvasH)
	outW, outH = canvasW, canvasH
	if longest := max(canvasW, canvasH); longest > maxSide {
		scale := float64(maxSide) / float64(longest)
		outW, outH = max(1, int(float64(canvasW)*scale)), max(1, int(float64(canvasH)*scale))
	}
	return
}

func checkGridReq(req imageGridReq) error {
	if len(req.Sources) < 1 || len(req.Sources) > gridMaxImages {
		return invalid("request.sources_count")
	}
	for _, src := range req.Sources {
		if err := checkSource(src); err != nil {
			return err
		}
	}
	if req.Gap != nil && (*req.Gap < 0 || *req.Gap > 512) {
		return invalid("request.gap")
	}
	if req.RowHeight != 0 && (req.RowHeight < 16 || req.RowHeight > 4096) {
		return invalid("request.row_height")
	}
	if req.MaxSide != 0 && (req.MaxSide < 64 || req.MaxSide > maxResizeSide) {
		return invalid("request.max_side")
	}
	return nil
}

// imageGrid 把 1..9 张图合成自适应宫格图（PNG，RGB，白底）。
// 与 Java 不同：任何一张取不到或不是图片都整体失败（422），不静默跳过——调用方拿到的 rid 必须对应完整的输入。
func (s *Server) imageGrid(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req imageGridReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkPurpose(req.Purpose), checkGridReq(req)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		paths := make([]string, len(req.Sources))
		sizes := make([][2]int, len(req.Sources))
		for i, src := range req.Sources {
			p, meta, err := s.fetchImageSource(ctx, dir, fmt.Sprintf("in-%d", i), src)
			if err != nil {
				return nil, err
			}
			paths[i], sizes[i] = p, [2]int{meta.Width, meta.Height}
		}
		gap, rowH, maxSide := gridDefaultGap, orDefault(req.RowHeight, gridDefaultRowH), orDefault(req.MaxSide, gridDefaultMaxSide)
		if req.Gap != nil {
			gap = *req.Gap
		}
		cw, ch, places, ow, oh := gridLayout(sizes, rowH, gap, maxSide)
		if limit := s.svc.cfg.MaxSourcePixels; limit > 0 && int64(cw)*int64(ch) > limit {
			return nil, newAPIError(codeSourceRejected, "image.target_pixels_exceeded")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "color=c=white:s=%dx%d,format=rgba[b0]", cw, ch)
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads()}
		for _, p := range paths {
			args = append(args, "-noautorotate", "-i", p)
		}
		for k, pl := range places {
			if pl.w == sizes[pl.idx][0] && pl.h == sizes[pl.idx][1] {
				fmt.Fprintf(&b, ";[%d:v:0]format=rgba[s%d]", pl.idx, k)
			} else {
				fmt.Fprintf(&b, ";[%d:v:0]scale=%d:%d:flags=bicubic,format=rgba[s%d]", pl.idx, pl.w, pl.h, k)
			}
			fmt.Fprintf(&b, ";[b%d][s%d]overlay=%d:%d:format=auto[b%d]", k, k, pl.x, pl.y, k+1)
		}
		last := fmt.Sprintf("b%d", len(places))
		if ow != cw || oh != ch {
			fmt.Fprintf(&b, ";[%s]scale=%d:%d:flags=bicubic,format=rgb24[out]", last, ow, oh)
		} else {
			fmt.Fprintf(&b, ";[%s]format=rgb24[out]", last)
		}
		out := workFile(dir, "grid.png")
		args = append(args, "-filter_complex", b.String(), "-map", "[out]", "-frames:v", "1", "-update", "1", out)
		if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, imageFailure(ctx, err, "image.grid_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

// ---- image/crop ----

// cropRects 把请求解析成像素矩形，并按图片实际宽高检查边界。
//   - boxes：左上角 (x, y) + 宽高，像素，原点在左上角，必须完整落在图内；
//   - grid：rows × cols 均分，行优先；单格 = floor(W/cols) × floor(H/rows)，余数像素丢弃（HitVideoCropHandler 的 W/2、H/2 同口径）。
func cropRects(req imageCropReq, w, h int) ([]cropBox, error) {
	if req.Grid != nil {
		g := req.Grid
		cw, ch := w/g.Cols, h/g.Rows
		if cw < 1 || ch < 1 {
			return nil, invalid("request.crop_grid_too_fine")
		}
		rects := make([]cropBox, 0, g.Rows*g.Cols)
		for r := 0; r < g.Rows; r++ {
			for c := 0; c < g.Cols; c++ {
				rects = append(rects, cropBox{X: c * cw, Y: r * ch, Width: cw, Height: ch})
			}
		}
		return rects, nil
	}
	for _, b := range req.Boxes {
		if b.X+b.Width > w || b.Y+b.Height > h {
			return nil, invalid("request.crop_out_of_bounds")
		}
	}
	return req.Boxes, nil
}

func checkCropReq(req imageCropReq) error {
	if (len(req.Boxes) > 0) == (req.Grid != nil) {
		return invalid("request.crop_boxes_or_grid") // 二选一
	}
	if req.Grid != nil {
		g := req.Grid
		if g.Rows < 1 || g.Cols < 1 || g.Rows*g.Cols > maxCropBoxes || g.Rows > maxResizeSide || g.Cols > maxResizeSide {
			return invalid("request.crop_grid")
		}
		return nil
	}
	if len(req.Boxes) > maxCropBoxes {
		return invalid("request.crop_boxes_count")
	}
	for _, b := range req.Boxes {
		if b.X < 0 || b.Y < 0 || b.Width < 1 || b.Height < 1 || b.X > maxResizeSide*4 || b.Y > maxResizeSide*4 {
			return invalid("request.crop_box")
		}
	}
	return nil
}

// imageCrop 按像素矩形或行列均分裁出多张 PNG，按请求顺序返回各自的 rid。
func (s *Server) imageCrop(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req imageCropReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose), checkCropReq(req)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, meta, err := s.fetchImageSource(ctx, dir, "src", req.Source)
		if err != nil {
			return nil, err
		}
		rects, err := cropRects(req, meta.Width, meta.Height)
		if err != nil {
			return nil, err
		}
		// 一次解码、多路输出
		var b strings.Builder
		fmt.Fprintf(&b, "[0:v:0]split=%d", len(rects))
		for i := range rects {
			fmt.Fprintf(&b, "[c%d]", i)
		}
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads(), "-noautorotate", "-i", path}
		outs := make([]string, len(rects))
		for i, rc := range rects {
			fmt.Fprintf(&b, ";[c%d]crop=%d:%d:%d:%d[o%d]", i, rc.Width, rc.Height, rc.X, rc.Y, i)
			outs[i] = workFile(dir, "crop-"+strconv.Itoa(i)+".png")
		}
		args = append(args, "-filter_complex", b.String())
		for i := range rects {
			args = append(args, "-map", fmt.Sprintf("[o%d]", i), "-frames:v", "1", "-update", "1", outs[i])
		}
		if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, imageFailure(ctx, err, "image.crop_failed")
		}
		base := idemKeyOf(ctx)
		resp := imageCropResp{Crops: make([]MediaResult, 0, len(outs))}
		for i, out := range outs {
			// 每张裁剪图要有各自的 rid：rid 由幂等键派生，所以按序号派生子键（同一键重放得到同一组 rid）
			res, err := s.storeFile(withIdemKey(ctx, base+"#crop"+strconv.Itoa(i)), req.Purpose, out)
			if err != nil {
				return nil, err
			}
			resp.Crops = append(resp.Crops, res)
		}
		return resp, nil
	})
}
