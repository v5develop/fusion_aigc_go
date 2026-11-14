package media

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// 排版内核的纯单元测试（不需要 ffmpeg）。字体用内置的 Go Regular：没有 CJK 字形，但量字宽照常工作（缺字形用 .notdef 的宽度），
// 足够验证折行 / 缩放 / 省略的几何约束。

func TestTokenizeSplitsWideRunesAndKeepsWords(t *testing.T) {
	got := strings.Join(tokenize("你好 world,ok  再见"), "|")
	if got != "你|好| |world,ok| | |再|见" {
		t.Fatalf("tokenize = %q", got)
	}
}

func TestWrapTextNeverExceedsWidthAndKeepsWords(t *testing.T) {
	tf := builtinFont()
	face, err := tf.face(20)
	if err != nil {
		t.Fatal(err)
	}
	const maxW = 160.0
	for _, text := range []string{
		"The quick brown fox jumps over the lazy dog",
		"averyveryveryverylongwordthatcannotfitononeline and more",
		"line one\nline two\n\nline four",
		strings.Repeat("W", 80),
	} {
		lines := wrapText(face, text, maxW)
		if len(lines) == 0 {
			t.Fatalf("%q 折行结果为空", text)
		}
		for _, ln := range lines {
			if w := measure(face, ln); w > maxW+0.01 {
				t.Fatalf("%q 的行 %q 宽 %.1f 超过 %.1f", text, ln, w, maxW)
			}
		}
	}
	// 显式换行保留，空行也算一行
	if got := wrapText(face, "a\n\nb", 500); len(got) != 3 {
		t.Fatalf("显式换行 = %q", got)
	}
	// 拉丁词不被拆开（词本身放得下时）
	for _, ln := range wrapText(face, "alpha beta gamma delta", 90) {
		for _, w := range strings.Fields(ln) {
			if !strings.Contains("alpha beta gamma delta", w) {
				t.Fatalf("词被拆开: %q", ln)
			}
		}
	}
}

func TestWrapTextWideRunesAndLineStartRule(t *testing.T) {
	tf := builtinFont()
	face, _ := tf.face(20)
	text := strings.Repeat("字", 30) + "，" + strings.Repeat("字", 10) + "。"
	lines := wrapText(face, text, 100)
	if len(lines) < 3 {
		t.Fatalf("应折成多行: %q", lines)
	}
	for _, ln := range lines {
		if strings.HasPrefix(ln, "，") || strings.HasPrefix(ln, "。") {
			t.Fatalf("标点出现在行首: %q", lines)
		}
		if measure(face, ln) > 100.01 {
			t.Fatalf("行超宽: %q", ln)
		}
	}
	if strings.Join(lines, "") != text {
		t.Fatal("折行不应丢字")
	}
}

func TestLayoutTextTable(t *testing.T) {
	tf := builtinFont()
	short := "Hello"
	long := strings.Repeat("overflowing words ", 25)
	cases := []struct {
		name         string
		text         string
		w, h         float64
		start, min   int
		maxLines     int
		wantTrunc    bool
		wantShrunk   bool // 最终字号 < start
		wantMaxLines int  // 0 = 不检查
	}{
		{"放得下不缩", short, 400, 80, 40, 20, 0, false, false, 0},
		{"自适应取最大字号", short, 400, 100, 60, 8, 0, false, false, 0},
		{"太宽就缩小", "A fairly long caption that needs scaling", 200, 60, 48, 12, 0, false, true, 0},
		{"限行数", "one two three four five six seven eight nine ten", 120, 200, 24, 24, 2, true, false, 2},
		{"超长省略", long, 200, 60, 24, 12, 0, true, true, 0},
		{"单行高度恰好", short, 400, 24, 20, 10, 1, false, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := layoutText(tf, c.text, c.w, c.h, c.start, c.min, c.maxLines, 1.2)
			if err != nil {
				t.Fatal(err)
			}
			if l.truncated != c.wantTrunc {
				t.Fatalf("truncated = %v, want %v (lines=%q)", l.truncated, c.wantTrunc, l.lines)
			}
			if (l.size < float64(c.start)) != c.wantShrunk {
				t.Fatalf("size = %.0f, start %d, wantShrunk %v", l.size, c.start, c.wantShrunk)
			}
			if c.wantMaxLines > 0 && len(l.lines) > c.wantMaxLines {
				t.Fatalf("行数 %d 超过 %d", len(l.lines), c.wantMaxLines)
			}
			// 不溢出：总高度在框内，每行宽度在框内
			if h := float64(len(l.lines)) * l.lineH; h > c.h+0.01 {
				t.Fatalf("总高 %.1f 超过框高 %.1f", h, c.h)
			}
			for _, ln := range l.lines {
				if w := measure(l.face, ln); w > c.w+0.01 {
					t.Fatalf("行 %q 宽 %.1f 超过框宽 %.1f", ln, w, c.w)
				}
			}
			if c.wantTrunc && !strings.HasSuffix(l.lines[len(l.lines)-1], "…") {
				t.Fatalf("截断后最后一行应以省略号结尾: %q", l.lines)
			}
		})
	}
}

func TestLayoutTextBoxTooSmall(t *testing.T) {
	if _, err := layoutText(builtinFont(), "x", 1, 40, 20, 10, 0, 1.2); err == nil {
		t.Fatal("宽度不足应报错")
	}
	if _, err := layoutText(builtinFont(), "x", 100, 3, 20, 10, 0, 1.2); err == nil {
		t.Fatal("高度放不下一行应报错")
	}
}

func TestRenderSlotStrokeShadowStayInsideAndPaint(t *testing.T) {
	tf := builtinFont()
	fonts := map[string]*textFont{"": tf}
	sl := textSlot{Text: "Stroke & shadow sample text", Color: "#FFFFFF", Size: 30,
		Stroke: &textStroke{Color: "#000000", Width: 3}, Shadow: &textShadow{Dx: 4, Dy: 4}, Align: "left", Valign: "top"}
	layer, err := renderSlot(fonts, sl, 200, 80)
	if err != nil {
		t.Fatal(err)
	}
	if layer.Bounds().Dx() != 200 || layer.Bounds().Dy() != 80 {
		t.Fatalf("层尺寸 = %v", layer.Bounds())
	}
	var painted, border int
	b := layer.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if layer.NRGBAAt(x, y).A != 0 {
				painted++
				if x == 0 || y == 0 || x == b.Max.X-1 || y == b.Max.Y-1 {
					border++
				}
			}
		}
	}
	if painted == 0 {
		t.Fatal("没画出任何像素")
	}
	// 描边 3px + 阴影偏移 4px 都已从可用区域里扣掉，框的最外圈不应被碰到
	if border != 0 {
		t.Fatalf("有 %d 个像素画到了框的最外圈", border)
	}
}

func TestCheckTextSlotsTable(t *testing.T) {
	box := textBox{X: 0.1, Y: 0.1, W: 0.5, H: 0.2}
	f := func(v float64) *float64 { return &v }
	ok := textSlot{Text: "hi", Box: box}
	cases := []struct {
		name  string
		slots []textSlot
		video bool
		want  string
	}{
		{"合法", []textSlot{ok}, false, ""},
		{"合法带时间", []textSlot{{Text: "hi", Box: box, Start: f(0), End: f(1)}}, true, ""},
		{"空文字", []textSlot{{Text: "", Box: box}}, false, "request.slot_text_empty"},
		{"文字 501 字", []textSlot{{Text: strings.Repeat("字", 501), Box: box}}, false, "request.slot_text"},
		{"文字 500 字", []textSlot{{Text: strings.Repeat("字", 500), Box: box}}, false, ""},
		{"非法 UTF-8", []textSlot{{Text: string([]byte{0xff, 0xfe}), Box: box}}, false, "request.slot_text"},
		{"框 x+w 超 1", []textSlot{{Text: "a", Box: textBox{X: 0.6, Y: 0, W: 0.5, H: 0.5}}}, false, "request.slot_box_range"},
		{"框负起点", []textSlot{{Text: "a", Box: textBox{X: -0.1, Y: 0, W: 0.5, H: 0.5}}}, false, "request.slot_box_range"},
		{"字号过大", []textSlot{{Text: "a", Box: box, Size: 5000}}, false, "request.slot_size"},
		{"min 大于 size", []textSlot{{Text: "a", Box: box, Size: 20, MinSize: 30}}, false, "request.slot_size"},
		{"描边过粗", []textSlot{{Text: "a", Box: box, Stroke: &textStroke{Width: 100}}}, false, "request.slot_stroke"},
		{"行高太小", []textSlot{{Text: "a", Box: box, LineHeight: 0.1}}, false, "request.slot_line_height"},
		{"视频时间反了", []textSlot{{Text: "a", Box: box, Start: f(2), End: f(1)}}, true, "request.slot_time_range"},
		{"视频时间为负", []textSlot{{Text: "a", Box: box, Start: f(-1), End: f(1)}}, true, "request.slot_time_range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkTextSlots(c.slots, c.video)
			got := ""
			if err != nil {
				got = classifyError(err).Check
			}
			if got != c.want {
				t.Fatalf("check = %q, want %q", got, c.want)
			}
		})
	}
	if utf8.RuneCountInString(strings.Repeat("字", 500)) != 500 {
		t.Fatal("测试数据有误")
	}
}

// 真实 CJK 字体（TTC）：本机没有系统字体时跳过（生产镜像装了 Noto CJK）。验证 TTC 解析、中文字形检查、折行与省略。
func TestRenderSlotCJKWithSystemFont(t *testing.T) {
	var path string
	for _, p := range []string{
		"/System/Library/Fonts/Hiragino Sans GB.ttc", "/System/Library/Fonts/STHeiti Light.ttc",
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc", "/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
	} {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		t.Skip("本机没有 CJK 系统字体")
	}
	tf, err := loadFontFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if r, bad := tf.missing("这一段还在重做，店名：老王烧烤"); bad {
		t.Fatalf("CJK 字体缺字形 %q", r)
	}
	if _, bad := builtinFont().missing("店名"); !bad {
		t.Fatal("内置拉丁字体应缺中文字形")
	}
	text := strings.Repeat("这一段还在重做，", 20)
	l, err := layoutText(tf, text, 240, 90, 32, 16, 0, 1.2)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range l.lines {
		if measure(l.face, ln) > 240.01 || strings.HasPrefix(ln, "，") {
			t.Fatalf("行不合规: %q", ln)
		}
	}
	if !l.truncated || !strings.HasSuffix(l.lines[len(l.lines)-1], "…") {
		t.Fatalf("超长中文应被省略: %q", l.lines)
	}
}
