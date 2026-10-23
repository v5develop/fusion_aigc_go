package media

import (
	"encoding/json"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writePNGFile 用 fn 逐像素生成真实的 PNG 字节写到 path。
func writePNGFile(t *testing.T, path string, w, h int, fn func(x, y int) color.NRGBA) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, fn(x, y))
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func solid(c color.NRGBA) func(x, y int) color.NRGBA { return func(int, int) color.NRGBA { return c } }

var (
	cRed   = color.NRGBA{R: 255, A: 255}
	cGreen = color.NRGBA{G: 255, A: 255}
	cBlue  = color.NRGBA{B: 255, A: 255}
	cWhite = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
)

// loadStored 解码落桶后的对象字节。
func loadStored(t *testing.T, f *apiFixture, res MediaResult) image.Image {
	t.Helper()
	fh, err := os.Open(storedObjectPath(t, f.objects, res))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	img, _, err := image.Decode(fh)
	if err != nil {
		t.Fatalf("输出不是可解码图片: %v", err)
	}
	return img
}

// near 像素与期望色的每个通道差不超过 tol（JPEG 有损）。
func pixNear(img image.Image, x, y int, want color.NRGBA, tol int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	d := func(a uint32, w uint8) bool {
		v := int(a>>8) - int(w)
		return v >= -tol && v <= tol
	}
	return d(r, want.R) && d(g, want.G) && d(b, want.B)
}

func requireNear(t *testing.T, img image.Image, x, y int, want color.NRGBA, tol int, what string) {
	t.Helper()
	if !pixNear(img, x, y, want, tol) {
		r, g, b, a := img.At(x, y).RGBA()
		t.Fatalf("%s: 像素(%d,%d) = (%d,%d,%d,%d), want ≈ %v", what, x, y, r>>8, g>>8, b>>8, a>>8, want)
	}
}

type errBody struct {
	Error struct {
		Code  string `json:"code"`
		Check string `json:"check"`
	} `json:"error"`
}

func expectError(t *testing.T, resp *http.Response, status int, code, check string) {
	t.Helper()
	defer resp.Body.Close()
	var e errBody
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if resp.StatusCode != status || e.Error.Code != code || e.Error.Check != check {
		t.Fatalf("响应 = %d %s/%s, want %d %s/%s", resp.StatusCode, e.Error.Code, e.Error.Check, status, code, check)
	}
}

// whiteish 统计矩形内接近白色的像素数。
func whitePixels(img image.Image, x0, y0, x1, y1 int) int {
	n := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if pixNear(img, x, y, cWhite, 40) {
				n++
			}
		}
	}
	return n
}

// 水印：logo 在左上 (margin, margin)、宽 = 图宽 × 15%；文字在右下角；其余区域不变。
func TestImageWatermarkLogoAndTextBottomRight(t *testing.T) {
	requireFFmpeg(t)
	requireFFmpegFilter(t, "drawtext")
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "base.png"), 400, 300, solid(cBlue))
	writePNGFile(t, filepath.Join(f.dir, "logo.png"), 100, 50, solid(cGreen))
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/watermark", map[string]any{
		"source": f.url("base.png"), "logo": f.url("logo.png"), "text": "WM", "purpose": "intermediate",
	}, testToken, "k-wm-1"))
	if res.Object.Width != 400 || res.Object.Height != 300 || res.Object.Mime != "image/png" || res.Object.Class != classWork {
		t.Fatalf("object = %+v", res.Object)
	}
	img := loadStored(t, f, res)
	// logo：宽 400×15% = 60，高 50×60/100 = 30，贴右下角（右、下各留 40）→ 占 x 300..359, y 230..259
	requireNear(t, img, 330, 245, cGreen, 6, "logo 中心")
	requireNear(t, img, 299, 245, cBlue, 6, "logo 左侧")
	requireNear(t, img, 330, 229, cBlue, 6, "logo 上方")
	requireNear(t, img, 360, 245, cBlue, 6, "logo 右侧")
	requireNear(t, img, 40, 40, cBlue, 6, "左上角不再有 logo")
	// 文字：在 logo 左侧、底部对齐；白字落在 logo 左边一段（右边界 = 400-40-60-40 = 260）
	if n := whitePixels(img, 150, 200, 265, 265); n < 30 {
		t.Fatalf("logo 左侧白色文字像素 = %d, 期望 >= 30", n)
	}
	if n := whitePixels(img, 0, 150, 200, 300); n != 0 {
		t.Fatalf("左下角出现了 %d 个白色像素", n)
	}
	requireNear(t, img, 5, 5, cBlue, 6, "角落")
	// logo 与文字都在右、下各留 40 像素之内；整张图保持不透明（Java 在不透明画布上 SRC_OVER）
	if n := whitePixels(img, 361, 0, 400, 300) + whitePixels(img, 150, 266, 400, 300); n != 0 {
		t.Fatalf("文字越过了 40 像素边距：%d 个白色像素", n)
	}
	for y := 0; y < 300; y++ {
		for x := 0; x < 400; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				t.Fatalf("像素(%d,%d) alpha = %d, 整图应不透明", x, y, a>>8)
			}
		}
	}
}

// 只有文字时不画 logo；不透明度 0 等于没画；输出格式跟随输入（JPEG 进 JPEG 出）。
func TestImageWatermarkTextOnlyJPEGAndOpacity(t *testing.T) {
	requireFFmpeg(t)
	requireFFmpegFilter(t, "drawtext")
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "b.png"), 400, 300, solid(cBlue))
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-i", filepath.Join(f.dir, "b.png"), filepath.Join(f.dir, "b.jpg")).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/watermark", map[string]any{
		"source": f.url("b.jpg"), "text": "WM", "purpose": "third_party",
	}, testToken, "k-wm-jpg"))
	if res.Object.Mime != "image/jpeg" || res.Object.Class != classDeliver || res.Object.Width != 400 || res.Object.Height != 300 {
		t.Fatalf("object = %+v", res.Object)
	}
	if n := whitePixels(loadStored(t, f, res), 250, 160, 360, 262); n < 30 {
		t.Fatalf("JPEG 右下角白色文字像素 = %d", n)
	}
	zero := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/watermark", map[string]any{
		"source": f.url("b.png"), "text": "WM", "text_opacity_percent": 0, "purpose": "intermediate",
	}, testToken, "k-wm-op0"))
	if n := whitePixels(loadStored(t, f, zero), 0, 0, 400, 300); n != 0 {
		t.Fatalf("不透明度 0 仍有 %d 个白色像素", n)
	}
}

func TestImageWatermarkRejections(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "b.png"), 100, 100, solid(cBlue))
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-i", filepath.Join(f.dir, "b.png"), filepath.Join(f.dir, "b.webp")).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg 不带 webp 编码器: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "n.png"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	post := func(key string, body map[string]any) *http.Response {
		body["purpose"] = "intermediate"
		return postWithKey(t, f.ts.URL, "/v1/mediasvc/image/watermark", body, testToken, key)
	}
	// WebP 不支持水印（与 Java SUPPORTED=png/jpg/jpeg 一致）：素材类 422
	expectError(t, post("k1", map[string]any{"source": f.url("b.webp"), "text": "x"}), 422, "source_rejected", "image.format_unsupported")
	expectError(t, post("k2", map[string]any{"source": f.url("n.png"), "text": "x"}), 422, "source_rejected", "image.not_image")
	expectError(t, post("k3", map[string]any{"source": f.url("b.png"), "text": "x", "logo": f.url("n.png")}), 422, "source_rejected", "image.not_image")
	// 请求本身的问题是 400
	expectError(t, post("k4", map[string]any{"source": f.url("b.png")}), 400, "invalid_request", "request.watermark_empty")
	expectError(t, post("k5", map[string]any{"source": f.url("b.png"), "text": "x", "text_opacity_percent": 101}), 400, "invalid_request", "request.text_opacity_percent")
}

// 排版公式：与 Java GridImageService 逐项对齐。
func TestGridLayoutMatchesJava(t *testing.T) {
	// 2 张：一行两列等高。200x100 → 高 960 时宽 1920；画布 3860x960 超过 1920 → 缩放 scale=1920/3860
	cw, ch, pl, ow, oh := gridLayout([][2]int{{200, 100}, {200, 100}}, 960, 20, 1920)
	if cw != 3860 || ch != 960 || len(pl) != 2 || pl[1].x != 1940 || ow != 1920 || oh != 477 /* int(960 × 1920/3860) */ {
		t.Fatalf("2 张: %d %d %+v %d %d", cw, ch, pl, ow, oh)
	}
	// 3 张：上 1 通栏居中，下 2 并排居中
	cw, ch, pl, _, _ = gridLayout([][2]int{{100, 100}, {50, 100}, {50, 100}}, 100, 10, 99999)
	if cw != 110 || ch != 210 || pl[0].x != 5 || pl[1].x != 0 || pl[2].x != 60 || pl[1].y != 110 {
		t.Fatalf("3 张: %d %d %+v", cw, ch, pl)
	}
	// 4 张 2+2；5 张 3+2；7 张 3+3+1
	_, ch, pl, _, _ = gridLayout(sq(4), 10, 2, 9999)
	if ch != 22 || pl[2].y != 12 || pl[2].x != 0 {
		t.Fatalf("4 张: %d %+v", ch, pl)
	}
	cw, ch, pl, _, _ = gridLayout(sq(5), 10, 2, 9999)
	if cw != 34 || ch != 22 || pl[3].x != 6 { // 第二行 2 张，宽 22，居中偏移 (34-22)/2
		t.Fatalf("5 张: %d %d %+v", cw, ch, pl)
	}
	_, ch, pl, _, _ = gridLayout(sq(7), 10, 2, 9999)
	if ch != 10*3+2*2 || pl[6].x != (34-10)/2 {
		t.Fatalf("7 张: %d %+v", ch, pl)
	}
	// 单张：不拼格，只限制最长边
	cw, ch, _, ow, oh = gridLayout([][2]int{{4000, 2000}}, 960, 20, 1920)
	if cw != 4000 || ch != 2000 || ow != 1920 || oh != 960 {
		t.Fatalf("1 张: %d %d -> %d %d", cw, ch, ow, oh)
	}
}

func sq(n int) [][2]int {
	s := make([][2]int, n)
	for i := range s {
		s[i] = [2]int{10, 10}
	}
	return s
}

// 宫格真实输出：两张不同高度的图等高并排，间距为白色，尺寸与像素位置符合排版。
func TestImageGridComposesTwoImages(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "r.png"), 100, 50, solid(cRed))
	writePNGFile(t, filepath.Join(f.dir, "g.png"), 80, 80, solid(cGreen))
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/grid", map[string]any{
		"sources": []any{f.url("r.png"), f.url("g.png")}, "row_height": 100, "gap": 10, "purpose": "intermediate",
	}, testToken, "k-grid-2"))
	// r → 200x100，g → 100x100；画布 (200+100+10)x100
	if res.Object.Width != 310 || res.Object.Height != 100 || res.Object.Mime != "image/png" {
		t.Fatalf("object = %+v", res.Object)
	}
	img := loadStored(t, f, res)
	requireNear(t, img, 5, 50, cRed, 3, "第一张")
	requireNear(t, img, 199, 99, cRed, 3, "第一张右下")
	requireNear(t, img, 205, 50, cWhite, 3, "间距")
	requireNear(t, img, 210, 50, cGreen, 3, "第二张左边缘")
	requireNear(t, img, 309, 99, cGreen, 3, "第二张右下")
	if ct := pngColorType(t, storedObjectPath(t, f.objects, res)); ct != 2 {
		t.Fatalf("宫格应是 RGB, 颜色类型 = %d", ct)
	}
}

// 三张：上一张居中，下两张并排；超过 max_side 等比缩小。
func TestImageGridThreeImagesAndMaxSide(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "r.png"), 100, 100, solid(cRed))
	writePNGFile(t, filepath.Join(f.dir, "g.png"), 50, 100, solid(cGreen))
	writePNGFile(t, filepath.Join(f.dir, "b.png"), 50, 100, solid(cBlue))
	srcs := []any{f.url("r.png"), f.url("g.png"), f.url("b.png")}
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/grid", map[string]any{
		"sources": srcs, "row_height": 100, "gap": 10, "purpose": "intermediate",
	}, testToken, "k-grid-3"))
	if res.Object.Width != 110 || res.Object.Height != 210 {
		t.Fatalf("size = %dx%d, want 110x210", res.Object.Width, res.Object.Height)
	}
	img := loadStored(t, f, res)
	requireNear(t, img, 55, 50, cRed, 3, "顶部通栏")
	requireNear(t, img, 2, 150, cGreen, 3, "底左")
	requireNear(t, img, 107, 150, cBlue, 3, "底右")
	requireNear(t, img, 55, 150, cWhite, 3, "底部间距")
	small := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/grid", map[string]any{
		"sources": srcs, "row_height": 100, "gap": 10, "max_side": 105, "purpose": "intermediate",
	}, testToken, "k-grid-3s"))
	// 最长边 210 → 105，scale 0.5：宽 int(110×0.5)=55
	if small.Object.Width != 55 || small.Object.Height != 105 {
		t.Fatalf("缩小后 = %dx%d, want 55x105", small.Object.Width, small.Object.Height)
	}
}

func TestImageGridRejections(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "r.png"), 10, 10, solid(cRed))
	if err := os.WriteFile(filepath.Join(f.dir, "n.png"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	ten := make([]any, 10)
	for i := range ten {
		ten[i] = f.url("r.png")
	}
	post := func(key string, srcs []any) *http.Response {
		return postWithKey(t, f.ts.URL, "/v1/mediasvc/image/grid", map[string]any{"sources": srcs, "purpose": "intermediate"}, testToken, key)
	}
	expectError(t, post("g1", ten), 400, "invalid_request", "request.sources_count")
	expectError(t, post("g2", nil), 400, "invalid_request", "request.sources_count")
	expectError(t, post("g3", []any{f.url("r.png"), f.url("n.png")}), 422, "source_rejected", "image.not_image")
}

// 裁剪：像素矩形，每张一个 rid（各不相同），内容与位置一致；同键重放得到同一组 rid。
func TestImageCropBoxesDistinctRids(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "q.png"), 200, 100, func(x, y int) color.NRGBA {
		switch {
		case x < 100 && y < 50:
			return cRed
		case x >= 100 && y < 50:
			return cGreen
		case x < 100:
			return cBlue
		}
		return cWhite
	})
	body := map[string]any{
		"source": f.url("q.png"), "purpose": "intermediate",
		"boxes": []any{
			map[string]int{"x": 0, "y": 0, "width": 100, "height": 50},
			map[string]int{"x": 100, "y": 50, "width": 100, "height": 50},
			map[string]int{"x": 90, "y": 40, "width": 20, "height": 20},
		},
	}
	call := func() imageCropResp {
		resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/crop", body, testToken, "k-crop")
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var out imageCropResp
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := call()
	if len(out.Crops) != 3 {
		t.Fatalf("crops = %d", len(out.Crops))
	}
	seen := map[string]bool{}
	for _, c := range out.Crops {
		if seen[c.Object.RID] {
			t.Fatalf("rid 重复: %s", c.Object.RID)
		}
		seen[c.Object.RID] = true
		if c.Object.Class != classWork || c.Object.Mime != "image/png" {
			t.Fatalf("object = %+v", c.Object)
		}
	}
	if o := out.Crops[0].Object; o.Width != 100 || o.Height != 50 {
		t.Fatalf("第一张尺寸 %dx%d", o.Width, o.Height)
	}
	requireNear(t, loadStored(t, f, out.Crops[0]), 50, 25, cRed, 2, "第一张")
	requireNear(t, loadStored(t, f, out.Crops[1]), 50, 25, cWhite, 2, "第二张")
	mid := loadStored(t, f, out.Crops[2])
	requireNear(t, mid, 2, 2, cRed, 2, "跨区第三张左上")    // (92,42) 在红区
	requireNear(t, mid, 17, 2, cGreen, 2, "跨区第三张右上") // (107,42) 在绿区
	requireNear(t, mid, 2, 17, cBlue, 2, "跨区第三张左下")  // (92,57) 在蓝区
	again := call()
	for i := range again.Crops {
		if again.Crops[i].Object.RID != out.Crops[i].Object.RID {
			t.Fatalf("重放 rid 变了: %s vs %s", again.Crops[i].Object.RID, out.Crops[i].Object.RID)
		}
	}
	// 换一个键得到另一组 rid
	body["boxes"] = body["boxes"]
	other := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/crop", body, testToken, "k-crop-2")
	defer other.Body.Close()
	var o2 imageCropResp
	_ = json.NewDecoder(other.Body).Decode(&o2)
	if len(o2.Crops) != 3 || o2.Crops[0].Object.RID == out.Crops[0].Object.RID {
		t.Fatalf("不同键应得到不同 rid")
	}
}

// 行列均分：行优先，单格 floor(W/cols) × floor(H/rows)，余数丢弃（奇数边 201x101 → 100x50）。
func TestImageCropGridRowMajorFloor(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "q.png"), 201, 101, func(x, y int) color.NRGBA {
		switch {
		case x < 100 && y < 50:
			return cRed
		case x >= 100 && y < 50:
			return cGreen
		case x < 100:
			return cBlue
		}
		return cWhite
	})
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/crop", map[string]any{
		"source": f.url("q.png"), "grid": map[string]int{"rows": 2, "cols": 2}, "purpose": "intermediate",
	}, testToken, "k-crop-grid")
	defer resp.Body.Close()
	var out imageCropResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Crops) != 4 {
		t.Fatalf("status=%d err=%v crops=%d", resp.StatusCode, err, len(out.Crops))
	}
	want := []color.NRGBA{cRed, cGreen, cBlue, cWhite}
	for i, c := range out.Crops {
		if c.Object.Width != 100 || c.Object.Height != 50 {
			t.Fatalf("第 %d 格尺寸 %dx%d", i, c.Object.Width, c.Object.Height)
		}
		requireNear(t, loadStored(t, f, c), 50, 25, want[i], 2, "格")
	}
}

func TestImageCropRejections(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "q.png"), 100, 100, solid(cRed))
	post := func(key string, extra map[string]any) *http.Response {
		extra["source"], extra["purpose"] = f.url("q.png"), "intermediate"
		return postWithKey(t, f.ts.URL, "/v1/mediasvc/image/crop", extra, testToken, key)
	}
	box := func(x, y, w, h int) any { return map[string]int{"x": x, "y": y, "width": w, "height": h} }
	expectError(t, post("c1", map[string]any{"boxes": []any{box(50, 50, 51, 10)}}), 400, "invalid_request", "request.crop_out_of_bounds")
	expectError(t, post("c2", map[string]any{"boxes": []any{box(0, 0, 0, 10)}}), 400, "invalid_request", "request.crop_box")
	expectError(t, post("c3", map[string]any{}), 400, "invalid_request", "request.crop_boxes_or_grid")
	expectError(t, post("c4", map[string]any{"boxes": []any{box(0, 0, 1, 1)}, "grid": map[string]int{"rows": 1, "cols": 1}}), 400, "invalid_request", "request.crop_boxes_or_grid")
	expectError(t, post("c5", map[string]any{"grid": map[string]int{"rows": 0, "cols": 2}}), 400, "invalid_request", "request.crop_grid")
	// 10x10 的小图切 11 行：单格不足 1 像素
	writePNGFile(t, filepath.Join(f.dir, "s.png"), 10, 10, solid(cRed))
	tiny := postWithKey(t, f.ts.URL, "/v1/mediasvc/image/crop", map[string]any{
		"source": f.url("s.png"), "grid": map[string]int{"rows": 11, "cols": 1}, "purpose": "intermediate",
	}, testToken, "c6")
	expectError(t, tiny, 400, "invalid_request", "request.crop_grid_too_fine")
}

// 标记：圆环（半径 30）画在像素 (100,100) 周围，圆心保持原色；默认红色；输出 JPEG。
func TestImageMarkRingJPEG(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "w.png"), 200, 200, solid(cWhite))
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/mark", map[string]any{
		"source": f.url("w.png"), "marks": []any{map[string]int{"x": 100, "y": 100, "radius": 30}}, "purpose": "intermediate",
	}, testToken, "k-mark-ring"))
	if res.Object.Mime != "image/jpeg" || res.Object.Width != 200 || res.Object.Height != 200 {
		t.Fatalf("object = %+v", res.Object)
	}
	img := loadStored(t, f, res)
	requireNear(t, img, 130, 100, cRed, 60, "圆环右侧")
	requireNear(t, img, 100, 70, cRed, 60, "圆环上侧")
	requireNear(t, img, 100, 100, cWhite, 25, "圆心")
	requireNear(t, img, 10, 10, cWhite, 25, "远处")
}

// 实心点、自定义颜色、PNG 输出；坐标超界夹到边缘；2 点自动画矩形。
func TestImageMarkDotColorPNGAndRect(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "w.png"), 200, 200, solid(cWhite))
	dot := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/mark", map[string]any{
		"source": f.url("w.png"), "shape": "dot", "format": "png", "purpose": "third_party",
		"marks": []any{map[string]any{"x": 50, "y": 60, "radius": 10, "color": "#0000ff"}, map[string]any{"x": 9999, "y": -5, "radius": 5}},
	}, testToken, "k-mark-dot"))
	if dot.Object.Mime != "image/png" || dot.Object.Class != classDeliver {
		t.Fatalf("object = %+v", dot.Object)
	}
	img := loadStored(t, f, dot)
	requireNear(t, img, 50, 60, cBlue, 0, "蓝点中心")
	requireNear(t, img, 50, 80, cWhite, 0, "点外")
	requireNear(t, img, 199, 0, cRed, 0, "夹到右上角的红点")
	rect := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/image/mark", map[string]any{
		"source": f.url("w.png"), "format": "png", "purpose": "intermediate",
		"marks": []any{map[string]int{"x": 40, "y": 40}, map[string]int{"x": 160, "y": 120}},
	}, testToken, "k-mark-rect"))
	ri := loadStored(t, f, rect)
	requireNear(t, ri, 100, 40, cRed, 30, "矩形上边")
	requireNear(t, ri, 160, 80, cRed, 30, "矩形右边")
	requireNear(t, ri, 100, 80, cWhite, 0, "矩形内部")
}

func TestImageMarkRejections(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	writePNGFile(t, filepath.Join(f.dir, "w.png"), 50, 50, solid(cWhite))
	post := func(key string, body map[string]any) *http.Response {
		body["source"], body["purpose"] = f.url("w.png"), "intermediate"
		return postWithKey(t, f.ts.URL, "/v1/mediasvc/image/mark", body, testToken, key)
	}
	pt := []any{map[string]int{"x": 1, "y": 1}}
	expectError(t, post("m1", map[string]any{"marks": []any{}}), 400, "invalid_request", "request.marks_count")
	expectError(t, post("m2", map[string]any{"marks": pt, "color": "red"}), 400, "invalid_request", "request.color")
	expectError(t, post("m3", map[string]any{"marks": pt, "shape": "star"}), 400, "invalid_request", "request.shape")
	expectError(t, post("m4", map[string]any{"marks": pt, "shape": "rect"}), 400, "invalid_request", "request.rect_needs_two_marks")
	expectError(t, post("m5", map[string]any{"marks": pt, "format": "gif"}), 400, "invalid_request", "request.format")
}

// 四个端点都要求 Idempotency-Key、鉴权，并登记在能力声明里。
func TestImageOpsRequireKeyAuthAndCapabilities(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	for _, p := range []string{"watermark", "grid", "crop", "mark"} {
		path := "/v1/mediasvc/image/" + p
		expectError(t, postWithKey(t, f.ts.URL, path, map[string]any{}, testToken, ""), 400, "invalid_request", "request.idempotency_key_missing")
		expectError(t, postWithKey(t, f.ts.URL, path, map[string]any{}, "wrong", "k"), 401, "unauthorized", "auth.token")
	}
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/v1/mediasvc/capabilities", nil)
	resp := send(t, req, testToken)
	defer resp.Body.Close()
	var caps map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"image_watermark", "image_grid", "image_crop", "image_mark"} {
		if !caps[k] {
			t.Fatalf("capabilities 缺 %s", k)
		}
	}
}
