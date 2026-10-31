// mock-media 模拟共享媒体服务（app/media）里 studio 链路用到的子集，仅开发 / e2e 用。端口默认 18090。
//
// 为什么需要它：真媒体服务本地不接 OSS 时（LocalStore）没有任何可访问地址，resources/urls、resources/copy 拿不到 url，
// studio 的 FetchBytes（反推要 data URL）与成品转存都走不通；接 OSS 又需要真实凭证。线上一律用真媒体服务。
//
//	GET  /healthz
//	GET  /v1/mediasvc/capabilities（校验 X-Media-Token）
//	POST /v1/mediasvc/ingest              {source:{url},purpose}         拉 URL 落内存
//	POST /v1/mediasvc/ingest/upload?purpose=                              原始字节
//	POST /v1/mediasvc/probe               {source:{rid}}                 {mime,size,width,height}
//	POST /v1/mediasvc/image/conform       {source:{rid},rule{max_long_side,mime_types}}
//	POST /v1/mediasvc/image/crop          {source:{rid},boxes[{x,y,width,height}]} → {crops:[…]}（PNG）
//	POST /v1/mediasvc/image/resize        {source:{rid},width,height,format}
//	POST /v1/mediasvc/resources/copy      {rid,purpose}                   新 rid（asset 类），url kind=public
//	POST /v1/mediasvc/resources/urls      {rid,store,mode,ttl_seconds}    {url}
//	GET  /obj/{rid}                       对象内容（public / signed 地址都指向这里，不验签）
//
// 令牌：请求头 X-Media-Token 必须等于环境变量 MOCK_MEDIA_TOKEN（默认 change-me）。
// rid 格式与真服务一致：{6 位实例}-{user|work|asset}-{26 位 base32}，由幂等键派生（同键同 rid）。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

const instance = "mockmd"

type object struct {
	rid    string
	class  string
	data   []byte
	mime   string
	w, h   int
	parent string
}

type server struct {
	mu    sync.Mutex
	token string
	objs  map[string]*object
	hc    *http.Client
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, c, msg string) {
	writeJSON(w, code, map[string]any{"code": c, "message": msg})
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newRID(class, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return instance + "-" + class + "-" + strings.ToLower(b32.EncodeToString(sum[:]))[:26]
}

func classOf(purpose string) string {
	switch purpose {
	case "user_input":
		return "user"
	case "asset_library":
		return "asset"
	case "third_party":
		return "deliver"
	}
	return "work"
}

func storeOf(class string) string {
	if class == "asset" {
		return "asset_library"
	}
	return "private"
}

func (s *server) result(r *http.Request, o *object, withURL bool) map[string]any {
	out := map[string]any{
		"object": map[string]any{"rid": o.rid, "class": o.class, "store": storeOf(o.class), "size": len(o.data),
			"mime": o.mime, "width": o.w, "height": o.h, "sha256": fmt.Sprintf("%x", sha256.Sum256(o.data))},
		"url": map[string]any{"kind": "none"},
	}
	if withURL {
		out["url"] = map[string]any{"kind": "public", "value": fmt.Sprintf("http://%s/obj/%s", r.Host, o.rid)}
	}
	if o.parent != "" {
		out["parents"] = []string{o.parent}
	}
	return out
}

// put 存对象：rid 由 (类别, 幂等键或内容) 派生。
func (s *server) put(class, idem string, data []byte, parent string) *object {
	seed := idem
	if seed == "" {
		sum := sha256.Sum256(data)
		seed = hex.EncodeToString(sum[:])
	}
	o := &object{rid: newRID(class, class+"|"+seed), class: class, data: data, parent: parent}
	o.mime = http.DetectContentType(data)
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		o.w, o.h = cfg.Width, cfg.Height
	}
	s.mu.Lock()
	s.objs[o.rid] = o
	s.mu.Unlock()
	return o
}

func (s *server) get(rid string) *object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objs[rid]
}

func (s *server) auth(fn func(w http.ResponseWriter, r *http.Request, body map[string]any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			fail(w, 405, "method", "POST only")
			return
		}
		if r.Header.Get("X-Media-Token") != s.token {
			fail(w, 401, "unauthorized", "bad token")
			return
		}
		body := map[string]any{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				fail(w, 400, "invalid_request", err.Error())
				return
			}
		}
		fn(w, r, body)
	}
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func num(m map[string]any, k string) int    { v, _ := m[k].(float64); return int(v) }
func sub(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	if v == nil {
		v = map[string]any{}
	}
	return v
}

func (s *server) source(w http.ResponseWriter, body map[string]any) (*object, image.Image, bool) {
	o := s.get(str(sub(body, "source"), "rid"))
	if o == nil {
		fail(w, 404, "not_found", "rid 不存在")
		return nil, nil, false
	}
	img, _, err := image.Decode(bytes.NewReader(o.data))
	if err != nil {
		fail(w, 422, "source_rejected", "不是可解码的图片")
		return nil, nil, false
	}
	return o, img, true
}

func encode(img image.Image, format string) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&buf, img)
	}
	return buf.Bytes(), err
}

// scale 最近邻缩放到 w×h。
func scale(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, b.Min.Y+y*b.Dy()/h))
		}
	}
	return dst
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "mock": true})
	})
	// capabilities：真实服务用它回报能力（就绪检查脚本拿它验令牌），mock 只校验令牌后返回固定值。
	mux.HandleFunc("/v1/mediasvc/capabilities", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Media-Token") != s.token {
			fail(w, 401, "unauthorized", "bad token")
			return
		}
		writeJSON(w, 200, map[string]any{"watermark": false, "mock": true})
	})
	mux.HandleFunc("/obj/", func(w http.ResponseWriter, r *http.Request) {
		o := s.get(strings.TrimPrefix(r.URL.Path, "/obj/"))
		if o == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", o.mime)
		_, _ = w.Write(o.data)
	})
	mux.HandleFunc("/v1/mediasvc/ingest", s.auth(func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		u := str(sub(body, "source"), "url")
		resp, err := s.hc.Get(u)
		if err != nil {
			fail(w, 422, "fetch_failed", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			fail(w, 422, "fetch_failed", fmt.Sprintf("源地址返回 %d", resp.StatusCode))
			return
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		o := s.put(classOf(str(body, "purpose")), r.Header.Get("Idempotency-Key"), data, "")
		writeJSON(w, 200, s.result(r, o, false))
	}))
	mux.HandleFunc("/v1/mediasvc/ingest/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Media-Token") != s.token {
			fail(w, 401, "unauthorized", "bad token")
			return
		}
		data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		o := s.put(classOf(r.URL.Query().Get("purpose")), r.Header.Get("Idempotency-Key"), data, "")
		writeJSON(w, 200, s.result(r, o, false))
	})
	mux.HandleFunc("/v1/mediasvc/probe", s.auth(func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		o := s.get(str(sub(body, "source"), "rid"))
		if o == nil {
			fail(w, 404, "not_found", "rid 不存在")
			return
		}
		writeJSON(w, 200, map[string]any{"mime": o.mime, "size": len(o.data), "width": o.w, "height": o.h})
	}))
	mux.HandleFunc("/v1/mediasvc/image/conform", s.auth(func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		o, img, ok := s.source(w, body)
		if !ok {
			return
		}
		rule := sub(body, "rule")
		maxLong := num(rule, "max_long_side")
		format := "png"
		if ms, _ := rule["mime_types"].([]any); len(ms) > 0 && ms[0] == "image/jpeg" {
			format = "jpeg"
		}
		bw, bh := img.Bounds().Dx(), img.Bounds().Dy()
		if maxLong > 0 && max(bw, bh) > maxLong {
			if bw >= bh {
				bw, bh = maxLong, max(1, bh*maxLong/bw)
			} else {
				bw, bh = max(1, bw*maxLong/bh), maxLong
			}
			img = scale(img, bw, bh)
		}
		data, err := encode(img, format)
		if err != nil {
			fail(w, 500, "encode", err.Error())
			return
		}
		writeJSON(w, 200, s.result(r, s.put("work", r.Header.Get("Idempotency-Key"), data, o.rid), false))
	}))
	mux.HandleFunc("/v1/mediasvc/image/resize", s.auth(func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		o, img, ok := s.source(w, body)
		if !ok {
			return
		}
		data, err := encode(scale(img, max(1, num(body, "width")), max(1, num(body, "height"))), str(body, "format"))
		if err != nil {
			fail(w, 500, "encode", err.Error())
			return
		}
		writeJSON(w, 200, s.result(r, s.put("work", r.Header.Get("Idempotency-Key"), data, o.rid), false))
	}))
	mux.HandleFunc("/v1/mediasvc/image/crop", s.auth(func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		o, img, ok := s.source(w, body)
		if !ok {
			return
		}
		boxes, _ := body["boxes"].([]any)
		crops := []any{}
		for i, bx := range boxes {
			m, _ := bx.(map[string]any)
			rect := image.Rect(num(m, "x"), num(m, "y"), num(m, "x")+num(m, "width"), num(m, "y")+num(m, "height")).Intersect(img.Bounds())
			if rect.Empty() {
				fail(w, 400, "invalid_request", "框在图外")
				return
			}
			dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
			draw.Draw(dst, dst.Bounds(), img, rect.Min, draw.Src)
			data, _ := encode(dst, "png")
			crops = append(crops, s.result(r, s.put("work", fmt.Sprintf("%s#%d", r.Header.Get("Idempotency-Key"), i), data, o.rid), false))
		}
		writeJSON(w, 200, map[string]any{"crops": crops})
	}))
	mux.HandleFunc("/v1/mediasvc/resources/copy", s.auth(func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		o := s.get(str(body, "rid"))
		if o == nil {
			fail(w, 404, "not_found", "rid 不存在")
			return
		}
		purpose := str(body, "purpose")
		if purpose != "asset_library" && purpose != "third_party" {
			fail(w, 400, "invalid_request", "purpose 只接受 asset_library / third_party")
			return
		}
		if o.class == "user" {
			fail(w, 403, "forbidden", "user 类别不能复制到公有桶")
			return
		}
		c := s.put(classOf(purpose), "copy|"+o.rid, o.data, o.rid)
		writeJSON(w, 200, s.result(r, c, true))
	}))
	mux.HandleFunc("/v1/mediasvc/resources/urls", s.auth(func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		o := s.get(str(body, "rid"))
		if o == nil {
			fail(w, 404, "not_found", "rid 不存在")
			return
		}
		writeJSON(w, 200, map[string]any{"rid": o.rid, "store": storeOf(o.class), "mode": "signed", "size": len(o.data),
			"url": fmt.Sprintf("http://%s/obj/%s", r.Host, o.rid)})
	}))
	return mux
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "listen address")
	flag.Parse()
	token := os.Getenv("MOCK_MEDIA_TOKEN")
	if token == "" {
		token = "change-me"
	}
	s := &server{token: token, objs: map[string]*object{}, hc: &http.Client{}}
	log.Printf("mock-media listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, s.handler()))
}
