package mediasvc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ridA = "tst001-user-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	ridB = "tst001-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"
	ridC = "tst001-asset-cccccccccccccccccccccccccc"
)

type seen struct {
	method, path, query, idem, ct string
	body                          []byte
}

type fake struct {
	mu   sync.Mutex
	reqs []seen
	h    func(path string, body []byte) (int, any)
}

func newFake(t *testing.T, h func(path string, body []byte) (int, any)) (*Client, *fake) {
	t.Helper()
	f := &fake{h: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type"), b})
		f.mu.Unlock()
		if r.Header.Get(TokenHeader) != "tok" {
			w.WriteHeader(401)
			return
		}
		code, v := f.h(r.URL.Path, b)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(srv.Close)
	return New([]string{srv.URL}, "tok"), f
}

func (f *fake) last() seen { return f.reqs[len(f.reqs)-1] }

func res(rid string, w, h int) map[string]any {
	return map[string]any{
		"object": map[string]any{"rid": rid, "class": "work", "store": "private", "sha256": "abc", "size": 123, "mime": "image/png", "width": w, "height": h},
		"url":    map[string]any{"kind": "signed", "value": "https://x/" + rid},
	}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIngestURLAndBytes(t *testing.T) {
	c, f := newFake(t, func(string, []byte) (int, any) { return 200, res(ridA, 10, 20) })
	r, err := c.IngestURL(context.Background(), "https://a/b.jpg", PurposeUserInput, "")
	if err != nil {
		t.Fatal(err)
	}
	s := f.last()
	m := decode(t, s.body)
	if s.method != "POST" || s.path != "/v1/mediasvc/ingest" || s.idem == "" || m["purpose"] != "user_input" ||
		m["source"].(map[string]any)["url"] != "https://a/b.jpg" {
		t.Fatalf("bad request %+v", s)
	}
	if r.RID != ridA || r.Width != 10 || r.Height != 20 || r.Mime != "image/png" || r.Bytes != 123 || r.SHA256 != "abc" {
		t.Fatalf("bad resource %+v", r)
	}
	// 自动键确定性
	_, _ = c.IngestURL(context.Background(), "https://a/b.jpg", PurposeUserInput, "")
	if f.reqs[0].idem != f.last().idem {
		t.Fatal("auto idem not deterministic")
	}

	if _, err := c.IngestBytes(context.Background(), "x.png", []byte("PNGDATA"), PurposeUserInput, "k1"); err != nil {
		t.Fatal(err)
	}
	s = f.last()
	if s.path != "/v1/mediasvc/ingest/upload" || s.query != "purpose=user_input" || s.idem != "k1" ||
		string(s.body) != "PNGDATA" || s.ct != "image/png" {
		t.Fatalf("bad upload %+v", s)
	}
}

func TestProbe(t *testing.T) {
	c, f := newFake(t, func(string, []byte) (int, any) {
		return 200, map[string]any{"mime": "image/jpeg", "size": 9, "width": 800, "height": 600}
	})
	p, err := c.Probe(context.Background(), ridA)
	if err != nil || p.Width != 800 || p.Height != 600 || p.Mime != "image/jpeg" || p.Bytes != 9 {
		t.Fatalf("%+v %v", p, err)
	}
	s := f.last()
	if s.path != "/v1/mediasvc/probe" || decode(t, s.body)["source"].(map[string]any)["rid"] != ridA {
		t.Fatalf("%+v", s)
	}
}

func TestConformResizeGrid(t *testing.T) {
	c, f := newFake(t, func(string, []byte) (int, any) { return 200, res(ridB, 1600, 900) })
	ctx := context.Background()
	if _, err := c.Conform(ctx, ridA, ConformParams{LongEdgeMax: 1600, Format: "jpeg", Quality: 90}, ""); err != nil {
		t.Fatal(err)
	}
	s := f.last()
	m := decode(t, s.body)
	rule := m["rule"].(map[string]any)
	if s.path != "/v1/mediasvc/image/conform" || s.idem == "" || rule["max_long_side"].(float64) != 1600 ||
		rule["mime_types"].([]any)[0] != "image/jpeg" || m["purpose"] != "intermediate" {
		t.Fatalf("%+v", s)
	}

	if _, err := c.Resize(ctx, ridA, 300, 200, "rk"); err != nil {
		t.Fatal(err)
	}
	s = f.last()
	m = decode(t, s.body)
	if s.path != "/v1/mediasvc/image/resize" || s.idem != "rk" || m["width"].(float64) != 300 || m["height"].(float64) != 200 || m["format"] != "png" {
		t.Fatalf("%+v", s)
	}

	gap := 0
	if _, err := c.Grid(ctx, []string{ridA, ridB, ridC}, GridParams{Gap: &gap, MaxSide: 1920}, ""); err != nil {
		t.Fatal(err)
	}
	s = f.last()
	m = decode(t, s.body)
	srcs := m["sources"].([]any)
	if s.path != "/v1/mediasvc/image/grid" || s.idem == "" || len(srcs) != 3 || srcs[1].(map[string]any)["rid"] != ridB ||
		m["gap"].(float64) != 0 || m["max_side"].(float64) != 1920 {
		t.Fatalf("%+v", s)
	}
	if _, ok := m["row_height"]; ok {
		t.Fatal("row_height should be omitted")
	}
	if _, err := c.Grid(ctx, make([]string, 10), GridParams{}, ""); err == nil {
		t.Fatal("expected error for 10 images")
	}
}

func TestCropPadAndUpscale(t *testing.T) {
	c, f := newFake(t, func(path string, body []byte) (int, any) {
		switch path {
		case "/v1/mediasvc/probe":
			return 200, map[string]any{"mime": "image/jpeg", "width": 1000, "height": 800}
		case "/v1/mediasvc/image/crop":
			return 200, map[string]any{"crops": []any{res(ridB, 100, 100), res(ridC, 500, 400)}}
		case "/v1/mediasvc/image/resize":
			return 200, res("tst001-work-dddddddddddddddddddddddddd", 256, 256)
		}
		return 404, map[string]any{"message": "x"}
	})
	boxes := []PixelBox{{X: 100, Y: 100, W: 92, H: 92, Label: "top"}, {X: 400, Y: 300, W: 500, H: 400, Label: "dress"}}
	out, err := c.Crop(context.Background(), ridA, boxes, CropParams{Pad: 0.04, MinShortEdge: 256}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Label != "top" || out[1].Label != "dress" {
		t.Fatalf("%+v", out)
	}
	var crop seen
	resizes := 0
	for _, s := range f.reqs {
		if s.path == "/v1/mediasvc/image/crop" {
			crop = s
		}
		if s.path == "/v1/mediasvc/image/resize" {
			resizes++
			if s.idem == "" {
				t.Fatal("resize missing idem")
			}
		}
	}
	if crop.idem == "" {
		t.Fatal("crop missing Idempotency-Key")
	}
	bx := decode(t, crop.body)["boxes"].([]any)
	b0 := bx[0].(map[string]any)
	// 92*0.04=3.68 -> 4：x 96, y 96, w 100, h 100
	if b0["x"].(float64) != 96 || b0["width"].(float64) != 100 || b0["height"].(float64) != 100 {
		t.Fatalf("box0 %+v", b0)
	}
	if resizes != 1 { // 只有第一张短边 100 < 256 要放大
		t.Fatalf("resizes=%d", resizes)
	}
	if out[0].Parents[0] != ridB {
		t.Fatalf("parents %+v", out[0].Parents)
	}
}

func TestPadBoxClamp(t *testing.T) {
	// 越界框裁进图内
	b := PadBox(PixelBox{X: -50, Y: -10, W: 200, H: 5000}, 1000, 800, 0)
	if b.X != 0 || b.Y != 0 || b.W != 150 || b.H != 800 {
		t.Fatalf("%+v", b)
	}
	// pad 后不超图
	b = PadBox(PixelBox{X: 0, Y: 0, W: 1000, H: 800}, 1000, 800, 0.1)
	if b.X != 0 || b.Y != 0 || b.W != 1000 || b.H != 800 {
		t.Fatalf("%+v", b)
	}
	b = PadBox(PixelBox{X: 900, Y: 700, W: 100, H: 100}, 1000, 800, 0.04)
	if b.X+b.W > 1000 || b.Y+b.H > 800 || b.X != 896 || b.Y != 696 {
		t.Fatalf("%+v", b)
	}
	// 完全在图外：退化为 1x1 且在图内
	b = PadBox(PixelBox{X: 5000, Y: 5000, W: 10, H: 10}, 1000, 800, 0)
	if b.W < 1 || b.H < 1 || b.X+b.W > 1000 || b.Y+b.H > 800 {
		t.Fatalf("%+v", b)
	}
	// 归一化裁到 [0,1]
	n := BoxFromNorm(-0.2, 0.5, 1.6, 0.8, 1000, 800)
	if n.X != 0 || n.Y != 400 || n.W != 1000 || n.H != 400 {
		t.Fatalf("%+v", n)
	}
}

func TestUpscaleSize(t *testing.T) {
	w, h, s := UpscaleSize(128, 512, 256)
	if w != 256 || h != 1024 || s != 2 {
		t.Fatal(w, h, s)
	}
	w, h, s = UpscaleSize(300, 400, 256)
	if w != 300 || h != 400 || s != 1 {
		t.Fatal(w, h, s)
	}
	w, h, _ = UpscaleSize(100, 150, 256)
	if w != 256 || h != 384 {
		t.Fatal(w, h)
	}
	if w, h, s = UpscaleSize(100, 100, 0); w != 100 || s != 1 {
		t.Fatal("no min")
	}
}

func TestResourceURLsAndCopy(t *testing.T) {
	c, f := newFake(t, func(path string, body []byte) (int, any) {
		m := decode(t, body)
		if path == "/v1/mediasvc/resources/copy" {
			r := res("tst001-asset-eeeeeeeeeeeeeeeeeeeeeeeeee", 10, 10)
			r["parents"] = []string{m["rid"].(string)}
			return 200, r
		}
		return 200, map[string]any{"rid": m["rid"], "url": "https://oss/" + m["rid"].(string) + "?sig=1"}
	})
	ctx := context.Background()
	urls, err := c.ResourceURLs(ctx, []string{ridA, ridC}, 10*time.Minute)
	if err != nil || len(urls) != 2 || urls[ridA] != "https://oss/"+ridA+"?sig=1" {
		t.Fatalf("%v %v", urls, err)
	}
	for _, s := range f.reqs {
		m := decode(t, s.body)
		wantStore := "private"
		if m["rid"] == ridC {
			wantStore = "asset_library"
		}
		if s.path != "/v1/mediasvc/resources/urls" || m["mode"] != "signed" || m["ttl_seconds"].(float64) != 600 || m["store"] != wantStore {
			t.Fatalf("%+v", s)
		}
	}
	if _, err := c.ResourceURLs(ctx, []string{"bad"}, 0); err == nil {
		t.Fatal("expected bad rid error")
	}

	r, err := c.CopyToLibrary(ctx, ridB, PurposeAssetLibrary, "")
	if err != nil {
		t.Fatal(err)
	}
	s := f.last()
	m := decode(t, s.body)
	if s.path != "/v1/mediasvc/resources/copy" || s.idem == "" || m["rid"] != ridB || m["purpose"] != "asset_library" ||
		len(r.Parents) != 1 || r.Parents[0] != ridB {
		t.Fatalf("%+v %+v", s, r)
	}
}

func TestFetchBytes(t *testing.T) {
	obj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(TokenHeader) != "" {
			t.Error("token leaked to object URL")
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("JPEGBYTES"))
	}))
	defer obj.Close()
	c, _ := newFake(t, func(string, []byte) (int, any) { return 200, map[string]any{"url": obj.URL + "/o.jpg"} })
	data, mt, err := c.FetchBytes(context.Background(), ridB)
	if err != nil || string(data) != "JPEGBYTES" || mt != "image/jpeg" {
		t.Fatalf("%q %q %v", data, mt, err)
	}
}

func TestPermanentErrorSurfaces(t *testing.T) {
	c, _ := newFake(t, func(string, []byte) (int, any) {
		return 422, map[string]any{"code": "source_rejected", "message": "image.not_image"}
	})
	_, err := c.Resize(context.Background(), ridA, 1, 1, "")
	e, ok := err.(*Error)
	if !ok || !e.Permanent() || !strings.Contains(e.Message, "not_image") {
		t.Fatalf("%v", err)
	}
}
