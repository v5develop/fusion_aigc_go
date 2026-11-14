package media

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// 文字层（视频 /video/overlay-text、图片 /image/text、占位画面共用同一个渲染内核）。
//
// 为什么不用 ffmpeg drawtext：自动折行、按框缩放、超长省略都需要先量字，drawtext 量不了；而且本机/部分镜像的 ffmpeg 不带 libfreetype。
// 这里用 Go 解析调用方登记的字体（font 摄入得到的 rid，支持 TrueType / OpenType / TTC，不支持 WOFF），
// 按框排版后画成只有「框那么大」的透明 PNG 层，再交给 ffmpeg 的 overlay 合成（视频按 start/end 开关）。
// 排版结果保证不溢出框：先在 [min_size, size] 里找能放下的最大字号，仍放不下则在 min_size 下截断并以「…」收尾。

const (
	textMaxSlots   = 16
	textMaxRunes   = 500
	textMaxSize    = 1024
	textFloorSize  = 6 // 框小到 min_size 放不下一行时，字号最低缩到这里；再放不下就拒绝
	textMaxStroke  = 24
	textMaxShadow  = 32
	textDefaultLH  = 1.2
	maxFontBytes   = 32 << 20
	textLayerLimit = 3840 * 2160 // 单个文字层（框）的像素上限，防内存
)

// textBox 相对画面的矩形，0~1。
type textBox struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type textStroke struct {
	Color string  `json:"color,omitempty"`
	Width float64 `json:"width"` // 像素
}

type textShadow struct {
	Color string `json:"color,omitempty"` // 默认 #000000B3
	Dx    int    `json:"dx,omitempty"`
	Dy    int    `json:"dy,omitempty"` // dx、dy 都为 0 时取 2、2
}

// textSlot 一个文字槽位。
type textSlot struct {
	Text       string      `json:"text"`
	Box        textBox     `json:"box"`
	FontRid    string      `json:"font_rid,omitempty"`
	Size       int         `json:"size,omitempty"`     // 最大字号（像素）；0 = 按框高自适应
	MinSize    int         `json:"min_size,omitempty"` // 缩放下限；0 = 显式字号的一半（至少 8），自适应时 8
	Color      string      `json:"color,omitempty"`    // #RGB / #RRGGBB / #RRGGBBAA，默认 #FFFFFF
	Align      string      `json:"align,omitempty"`    // left|center|right，默认 center
	Valign     string      `json:"valign,omitempty"`   // top|middle|bottom，默认 middle
	Stroke     *textStroke `json:"stroke,omitempty"`
	Shadow     *textShadow `json:"shadow,omitempty"`
	MaxLines   int         `json:"max_lines,omitempty"`   // 0 = 不限（受框高约束）
	LineHeight float64     `json:"line_height,omitempty"` // 行高倍数，默认 1.2，范围 0.8~3
	Start      *float64    `json:"start,omitempty"`       // 仅视频：显示起止（秒）；都省略 = 全程
	End        *float64    `json:"end,omitempty"`
}

func parseTextColor(v string, def color.NRGBA) (color.NRGBA, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "#")
	if v == "" {
		return def, true
	}
	switch len(v) {
	case 3, 4:
		b := make([]byte, 0, 8)
		for i := 0; i < len(v); i++ {
			b = append(b, v[i], v[i])
		}
		v = string(b)
	}
	if len(v) != 6 && len(v) != 8 {
		return color.NRGBA{}, false
	}
	n, err := strconv.ParseUint(v, 16, 32)
	if err != nil {
		return color.NRGBA{}, false
	}
	if len(v) == 6 {
		return color.NRGBA{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n), A: 255}, true
	}
	return color.NRGBA{R: uint8(n >> 24), G: uint8(n >> 16), B: uint8(n >> 8), A: uint8(n)}, true
}

// checkTextSlots 校验槽位（与具体画面无关的部分）。video=true 时允许 start/end。
func checkTextSlots(slots []textSlot, video bool) error {
	if len(slots) < 1 || len(slots) > textMaxSlots {
		return invalid("request.slots_count")
	}
	for _, sl := range slots {
		if strings.TrimSpace(sl.Text) == "" {
			return invalid("request.slot_text_empty")
		}
		if utf8.RuneCountInString(sl.Text) > textMaxRunes || !utf8.ValidString(sl.Text) || strings.ContainsRune(sl.Text, 0) {
			return invalid("request.slot_text")
		}
		b := sl.Box
		for _, v := range []float64{b.X, b.Y, b.W, b.H} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return invalid("request.slot_box_range")
			}
		}
		const eps = 1e-6
		if b.X < 0 || b.Y < 0 || b.W <= 0 || b.H <= 0 || b.X+b.W > 1+eps || b.Y+b.H > 1+eps {
			return invalid("request.slot_box_range")
		}
		if sl.FontRid != "" {
			if _, _, err := ParseRID(sl.FontRid); err != nil {
				return invalid("request.font_rid")
			}
		}
		if sl.Size < 0 || sl.Size > textMaxSize || sl.MinSize < 0 || sl.MinSize > textMaxSize || (sl.Size > 0 && sl.MinSize > sl.Size) {
			return invalid("request.slot_size")
		}
		if _, ok := parseTextColor(sl.Color, color.NRGBA{}); !ok {
			return invalid("request.slot_color")
		}
		switch sl.Align {
		case "", "left", "center", "right":
		default:
			return invalid("request.slot_align")
		}
		switch sl.Valign {
		case "", "top", "middle", "bottom":
		default:
			return invalid("request.slot_valign")
		}
		if sl.Stroke != nil {
			if _, ok := parseTextColor(sl.Stroke.Color, color.NRGBA{}); !ok || math.IsNaN(sl.Stroke.Width) || sl.Stroke.Width < 0 || sl.Stroke.Width > textMaxStroke {
				return invalid("request.slot_stroke")
			}
		}
		if sl.Shadow != nil {
			if _, ok := parseTextColor(sl.Shadow.Color, color.NRGBA{}); !ok || abs(sl.Shadow.Dx) > textMaxShadow || abs(sl.Shadow.Dy) > textMaxShadow {
				return invalid("request.slot_shadow")
			}
		}
		if sl.MaxLines < 0 || sl.MaxLines > 50 {
			return invalid("request.slot_max_lines")
		}
		if sl.LineHeight != 0 && (math.IsNaN(sl.LineHeight) || sl.LineHeight < 0.8 || sl.LineHeight > 3) {
			return invalid("request.slot_line_height")
		}
		if !video && (sl.Start != nil || sl.End != nil) {
			return invalid("request.slot_time_unsupported")
		}
		if video {
			if (sl.Start == nil) != (sl.End == nil) {
				return invalid("request.slot_time_range")
			}
			if sl.Start != nil {
				s, e := *sl.Start, *sl.End
				if math.IsNaN(s) || math.IsNaN(e) || math.IsInf(s, 0) || math.IsInf(e, 0) || s < 0 || e <= s {
					return invalid("request.slot_time_range")
				}
			}
		}
	}
	return nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---- 字体 ----

// textFont 解析后的字体。
type textFont struct {
	f   *opentype.Font
	buf sfnt.Buffer
}

// loadFontFile 读本地字体文件。只收文件头为 TrueType / OpenType / TTC 的字体；WOFF 系列、其它内容一律 422。
func loadFontFile(path string) (*textFont, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, internal(err)
	}
	if st.Size() > maxFontBytes {
		return nil, newAPIError(codeSourceRejected, "font.size_exceeded")
	}
	mime, ok := fontMime(path)
	if !ok {
		return nil, newAPIError(codeSourceRejected, "font.invalid")
	}
	if mime == "font/woff" || mime == "font/woff2" {
		return nil, newAPIError(codeSourceRejected, "font.format_unsupported")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, internal(err)
	}
	var f *opentype.Font
	if mime == "font/collection" {
		var c *opentype.Collection
		if c, err = opentype.ParseCollection(data); err == nil {
			f, err = c.Font(0)
		}
	} else {
		f, err = opentype.Parse(data)
	}
	if err != nil || f == nil {
		return nil, newAPIError(codeSourceRejected, "font.invalid")
	}
	return &textFont{f: f}, nil
}

// builtinFont 内置的拉丁字体（Go Regular）：调用方没给 font_rid 时用，只能排拉丁字符。
func builtinFont() *textFont {
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		panic(err) // 内置数据，解析失败是编译期事故
	}
	return &textFont{f: f}
}

// hasGlyphs 文字里是否有字体没有的字符（控制字符、空白不算）；返回第一个缺的。
func (tf *textFont) missing(text string) (rune, bool) {
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			continue
		}
		idx, err := tf.f.GlyphIndex(&tf.buf, r)
		if err != nil || idx == 0 {
			return r, true
		}
	}
	return 0, false
}

func (tf *textFont) face(size float64) (font.Face, error) {
	return opentype.NewFace(tf.f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
}

// ---- 排版 ----

// isWordRune 属于「按词断行」的字符（拉丁、西里尔、数字等）；其余（CJK、假名、全角……）逐字断行。
func isWordRune(r rune) bool { return r < 0x2E80 && !unicode.IsSpace(r) }

// noLineStart 不能出现在行首的标点（简单禁则）。
const noLineStart = "，。、；：！？）】》」』”’,.;:!?)]}%…—"

func measure(face font.Face, s string) float64 {
	return float64(font.MeasureString(face, s)) / 64
}

// tokenize 把一段（不含换行）切成排版单元：拉丁词、空格、单个宽字符。
func tokenize(p string) []string {
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range p {
		switch {
		case r == '\t' || unicode.IsSpace(r):
			flush()
			toks = append(toks, " ")
		case isWordRune(r):
			cur = append(cur, r)
		default:
			flush()
			toks = append(toks, string(r))
		}
	}
	flush()
	return toks
}

// wrapText 按宽度折行。显式换行保留；过长的词按字符强拆；永远不产生超过 maxW 的行（除非单个字符就比 maxW 宽）。
func wrapText(face font.Face, text string, maxW float64) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		cur := ""
		push := func() {
			lines = append(lines, strings.TrimRight(cur, " "))
			cur = ""
		}
		for _, tok := range tokenize(para) {
			if tok == " " {
				if cur != "" {
					cur += " "
				}
				continue
			}
			if cur == "" || measure(face, cur+tok) <= maxW {
				if cur == "" && measure(face, tok) > maxW {
					// 单词比整行还宽：逐字符拆
					for _, r := range tok {
						if cur != "" && measure(face, cur+string(r)) > maxW {
							push()
						}
						cur += string(r)
					}
					continue
				}
				cur += tok
				continue
			}
			// 放不下：换行。行首禁则：把上一行最后一个字带下来
			carry := ""
			if utf8.RuneCountInString(tok) == 1 && strings.ContainsRune(noLineStart, []rune(tok)[0]) {
				if trimmed := strings.TrimRight(cur, " "); utf8.RuneCountInString(trimmed) >= 2 {
					_, n := utf8.DecodeLastRuneInString(trimmed)
					carry = trimmed[len(trimmed)-n:]
					cur = trimmed[:len(trimmed)-n]
				}
			}
			push()
			cur = carry
			if measure(face, cur+tok) > maxW && cur != "" { // 带下来仍放不下（极窄框）
				push()
			}
			if measure(face, tok) > maxW {
				for _, r := range tok {
					if cur != "" && measure(face, cur+string(r)) > maxW {
						push()
					}
					cur += string(r)
				}
				continue
			}
			cur += tok
		}
		push()
	}
	return lines
}

// ellipsize 把一行截到 maxW 内并以「…」收尾。
func ellipsize(face font.Face, line string, maxW float64) string {
	line = strings.TrimRight(line, " ")
	rs := []rune(line)
	for len(rs) > 0 {
		cand := strings.TrimRight(string(rs), " ") + "…"
		if measure(face, cand) <= maxW {
			return cand
		}
		rs = rs[:len(rs)-1]
	}
	return "…"
}

// textLayout 排版结果。
type textLayout struct {
	lines     []string
	size      float64
	lineH     float64
	face      font.Face
	truncated bool
}

// layoutText 在 availW × availH 内排版：找能放下的最大字号；min 下仍放不下则截断。
func layoutText(tf *textFont, text string, availW, availH float64, startSize, minSize int, maxLines int, lineHeight float64) (*textLayout, error) {
	if availW < 2 || availH < 2 {
		return nil, invalid("request.slot_box_too_small")
	}
	fit := func(size int) (*textLayout, bool, error) {
		face, err := tf.face(float64(size))
		if err != nil {
			return nil, false, internal(err)
		}
		lines := wrapText(face, text, availW)
		lh := float64(size) * lineHeight
		ok := (maxLines == 0 || len(lines) <= maxLines) && float64(len(lines))*lh <= availH+0.01
		return &textLayout{lines: lines, size: float64(size), lineH: lh, face: face}, ok, nil
	}
	hi, lo := startSize, minSize
	if lo > hi {
		lo = hi
	}
	if l, ok, err := fit(hi); err != nil || ok {
		return l, err
	}
	// 二分：lo 若能放下，找最大的能放下的字号
	if l, ok, err := fit(lo); err != nil {
		return nil, err
	} else if ok {
		best := l
		a, b := lo, hi // a 能放下，b 放不下
		for b-a > 1 {
			mid := (a + b) / 2
			l, ok, err := fit(mid)
			if err != nil {
				return nil, err
			}
			if ok {
				a, best = mid, l
			} else {
				b = mid
			}
		}
		return best, nil
	}
	// min 下也放不下：缩到至少放得下一行高度，再截断
	size := lo
	for size > textFloorSize && float64(size)*lineHeight > availH {
		size--
	}
	if float64(size)*lineHeight > availH {
		return nil, invalid("request.slot_box_too_small")
	}
	l, _, err := fit(size)
	if err != nil {
		return nil, err
	}
	n := int(availH/l.lineH + 0.01)
	if maxLines > 0 && n > maxLines {
		n = maxLines
	}
	if n < 1 {
		n = 1
	}
	if len(l.lines) > n {
		l.lines = l.lines[:n]
		l.lines[n-1] = ellipsize(l.face, l.lines[n-1], availW)
		l.truncated = true
	} else {
		// 仍有单行超宽（单字比框宽）：逐行兜底截断
		for i, ln := range l.lines {
			if measure(l.face, ln) > availW {
				l.lines[i] = ellipsize(l.face, ln, availW)
				l.truncated = true
			}
		}
	}
	return l, nil
}

// ---- 渲染 ----

// dilate 把 mask 向外扩 r 像素（圆形），得到描边区域。只在有字的像素上盖章，成本与字迹面积成正比。
func dilate(src *image.Alpha, r int) *image.Alpha {
	out := image.NewAlpha(src.Bounds())
	b := src.Bounds()
	hw := make([]int, 2*r+1)
	for dy := -r; dy <= r; dy++ {
		hw[dy+r] = int(math.Floor(math.Sqrt(float64(r*r - dy*dy))))
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := src.Pix[(y-b.Min.Y)*src.Stride : (y-b.Min.Y)*src.Stride+b.Dx()]
		for x, a := range row {
			if a == 0 {
				continue
			}
			for dy := -r; dy <= r; dy++ {
				yy := y + dy
				if yy < b.Min.Y || yy >= b.Max.Y {
					continue
				}
				base := (yy - b.Min.Y) * out.Stride
				for dx := -hw[dy+r]; dx <= hw[dy+r]; dx++ {
					xx := x + dx
					if xx < b.Min.X || xx >= b.Max.X {
						continue
					}
					if p := &out.Pix[base+xx-b.Min.X]; *p < a {
						*p = a
					}
				}
			}
		}
	}
	return out
}

// renderSlot 把一个槽位画成 boxW × boxH 的透明层。文字完全落在框内（描边与阴影的占位已从可用区域里扣除）。
func renderSlot(fonts map[string]*textFont, sl textSlot, boxW, boxH int) (*image.NRGBA, error) {
	tf := fonts[sl.FontRid]
	if tf == nil {
		return nil, internal(fmt.Errorf("字体未加载"))
	}
	fill, _ := parseTextColor(sl.Color, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	strokeW := 0
	var strokeC color.NRGBA
	if sl.Stroke != nil && sl.Stroke.Width > 0 {
		strokeW = int(math.Ceil(sl.Stroke.Width))
		strokeC, _ = parseTextColor(sl.Stroke.Color, color.NRGBA{A: 255})
	}
	shadowOn, sdx, sdy := false, 0, 0
	var shadowC color.NRGBA
	if sl.Shadow != nil {
		shadowOn, sdx, sdy = true, sl.Shadow.Dx, sl.Shadow.Dy
		if sdx == 0 && sdy == 0 {
			sdx, sdy = 2, 2
		}
		shadowC, _ = parseTextColor(sl.Shadow.Color, color.NRGBA{A: 0xB3})
	}
	// 可用区域：四周各让出描边宽度；阴影偏移的那一侧再让出偏移量
	padL, padR, padT, padB := strokeW, strokeW, strokeW, strokeW
	if shadowOn {
		if sdx > 0 {
			padR += sdx
		} else {
			padL -= sdx
		}
		if sdy > 0 {
			padB += sdy
		} else {
			padT -= sdy
		}
	}
	availW := float64(boxW - padL - padR)
	availH := float64(boxH - padT - padB)

	start := sl.Size
	minSize := sl.MinSize
	if start == 0 {
		start = min(int(availH), textMaxSize)
		if minSize == 0 {
			minSize = 8
		}
	} else if minSize == 0 {
		minSize = max(8, (start+1)/2)
	}
	start = max(start, 1)
	minSize = min(max(minSize, 1), start)
	lh := sl.LineHeight
	if lh == 0 {
		lh = textDefaultLH
	}
	lay, err := layoutText(tf, sl.Text, availW, availH, start, minSize, sl.MaxLines, lh)
	if err != nil {
		return nil, err
	}

	metrics := lay.face.Metrics()
	asc, desc := float64(metrics.Ascent)/64, float64(metrics.Descent)/64
	blockH := float64(len(lay.lines)) * lay.lineH
	top := float64(padT)
	switch sl.Valign {
	case "top":
	case "bottom":
		top += availH - blockH
	default:
		top += (availH - blockH) / 2
	}
	fillMask := image.NewAlpha(image.Rect(0, 0, boxW, boxH))
	dr := &font.Drawer{Dst: fillMask, Src: image.Opaque, Face: lay.face}
	for i, ln := range lay.lines {
		w := measure(lay.face, ln)
		x := float64(padL)
		switch sl.Align {
		case "left":
		case "right":
			x += availW - w
		default:
			x += (availW - w) / 2
		}
		x = math.Max(x, float64(padL)) // 兜底：不画到可用区外（单字比框宽时左对齐）
		y := top + float64(i)*lay.lineH + (lay.lineH-(asc+desc))/2 + asc
		dr.Dot = fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(y * 64)}
		dr.DrawString(ln)
	}

	layer := image.NewNRGBA(image.Rect(0, 0, boxW, boxH))
	outer := fillMask
	if strokeW > 0 {
		outer = dilate(fillMask, strokeW)
	}
	if shadowOn {
		draw.DrawMask(layer, layer.Bounds(), image.NewUniform(shadowC), image.Point{}, outer, image.Pt(-sdx, -sdy), draw.Over)
	}
	if strokeW > 0 {
		draw.DrawMask(layer, layer.Bounds(), image.NewUniform(strokeC), image.Point{}, outer, image.Point{}, draw.Over)
	}
	draw.DrawMask(layer, layer.Bounds(), image.NewUniform(fill), image.Point{}, fillMask, image.Point{}, draw.Over)
	return layer, nil
}

// slotRect 槽位在 width × height 画面上的像素矩形（四舍五入，夹进画面，至少 2×2）。
func slotRect(b textBox, width, height int) image.Rectangle {
	x0 := int(math.Round(b.X * float64(width)))
	y0 := int(math.Round(b.Y * float64(height)))
	x1 := int(math.Round((b.X + b.W) * float64(width)))
	y1 := int(math.Round((b.Y + b.H) * float64(height)))
	x0, y0 = max(0, min(x0, width-2)), max(0, min(y0, height-2))
	x1, y1 = min(width, max(x1, x0+2)), min(height, max(y1, y0+2))
	return image.Rect(x0, y0, x1, y1)
}

// loadSlotFonts 取回槽位用到的全部字体（按 font_rid 去重；空 rid = 内置拉丁字体），并检查每个槽位的字符都有字形。
func (s *Server) loadSlotFonts(ctx context.Context, dir string, rids []string, texts []string) (map[string]*textFont, error) {
	fonts := map[string]*textFont{}
	for i, rid := range rids {
		if _, ok := fonts[rid]; !ok {
			if rid == "" {
				fonts[rid] = builtinFont()
			} else {
				fd := filepath.Join(dir, fmt.Sprintf("font-%d", len(fonts)))
				if err := os.MkdirAll(fd, 0o755); err != nil {
					return nil, internal(err)
				}
				path, err := s.sourceFn(SourceRef{Rid: rid})(ctx, fd)
				if err != nil {
					return nil, err
				}
				tf, err := loadFontFile(path)
				if err != nil {
					return nil, err
				}
				fonts[rid] = tf
			}
		}
		if _, bad := fonts[rid].missing(texts[i]); bad {
			if rid == "" {
				return nil, invalid("text.font_required")
			}
			return nil, newAPIError(codeSourceRejected, "text.glyph_missing")
		}
	}
	return fonts, nil
}

// checkRidSource 只接受 rid（不接受任意 URL / 历史 key）。
func checkRidSource(src SourceRef) error {
	if src.Rid == "" || src.Url != "" || src.Key != "" || len(src.Headers) > 0 {
		return invalid("request.source_rid_only")
	}
	if _, _, err := ParseRID(src.Rid); err != nil {
		return invalid("request.rid")
	}
	return nil
}
