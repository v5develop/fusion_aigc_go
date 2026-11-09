package logic

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------- fakes（不调任何真实网关 / 供应商）

type fakeMaskGW struct {
	calls []aigateway.CallRequest
	keys  []string
	resp  *aigateway.Call
	err   error
}

func (g *fakeMaskGW) Submit(_ context.Context, req aigateway.CallRequest, opt aigateway.SubmitOptions) (*aigateway.Call, error) {
	g.calls, g.keys = append(g.calls, req), append(g.keys, opt.IdempotencyKey)
	if g.err != nil {
		return nil, g.err
	}
	return g.resp, nil
}

func (g *fakeMaskGW) Get(context.Context, string, time.Duration) (*aigateway.Call, error) {
	return g.resp, g.err
}

type fakeMaskMedia struct {
	cutout   []byte
	ingested []byte
	copied   []string
	fetchErr error
}

func (m *fakeMaskMedia) FetchBytes(context.Context, string) ([]byte, string, error) {
	return m.cutout, "image/png", m.fetchErr
}

func (m *fakeMaskMedia) IngestBytes(_ context.Context, _ string, data []byte, purpose, _ string) (mediasvc.Resource, error) {
	if purpose != mediasvc.PurposeIntermediate {
		return mediasvc.Resource{}, errors.New("purpose")
	}
	m.ingested = data
	return mediasvc.Resource{RID: "aaaaaa-work-mask"}, nil
}

func (m *fakeMaskMedia) CopyToLibrary(_ context.Context, rid, purpose, _ string) (mediasvc.Resource, error) {
	if purpose != mediasvc.PurposeAssetLibrary {
		return mediasvc.Resource{}, errors.New("purpose")
	}
	m.copied = append(m.copied, rid)
	return mediasvc.Resource{RID: "aaaaaa-asset-mask", URL: "https://cdn.example.com/mask.png"}, nil
}

// cutoutPNG w×h 的透明 PNG，左半边 alpha=a（模拟「只含区域」的抠图产物）。
func cutoutPNG(t *testing.T, w, h int, a uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w/2; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 10, B: 10, A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func segCall(cost string) *aigateway.Call {
	return &aigateway.Call{
		ID: "call_1", Status: aigateway.StatusSucceeded,
		Result: &aigateway.Result{Operation: "image.segment", Image: &aigateway.MediaOut{Rid: "gwgwgw-work-cut"}},
		Cost:   &aigateway.Cost{Amount: cost, Currency: "CNY", AmountCNY: cost},
	}
}

// ---------------------------------------------------------------- 纯函数

func TestNormalizeMaskURL(t *testing.T) {
	a, ka, err := normalizeMaskURL("HTTPS://CDN.Example.com/a/b.png?Expires=1&Signature=x#f")
	if err != nil || a != "https://cdn.example.com/a/b.png" {
		t.Fatalf("norm=%q err=%v", a, err)
	}
	_, kb, _ := normalizeMaskURL("https://cdn.example.com/a/b.png?Expires=2")
	if ka != kb || len(ka) != 64 {
		t.Fatalf("签名参数不同应同键：%s %s", ka, kb)
	}
	for _, bad := range []string{"", "ftp://x/a.png", "/rel.png", "data:image/png;base64,xx"} {
		if _, _, err := normalizeMaskURL(bad); err == nil {
			t.Fatalf("%q 应拒收", bad)
		}
	}
}

func TestNormalizeMaskTarget(t *testing.T) {
	for _, s := range []string{"garment", " Product ", "PERSON", "background"} {
		if _, ok := normalizeMaskTarget(s); !ok {
			t.Fatalf("%q 应认", s)
		}
	}
	if _, ok := normalizeMaskTarget("sky"); ok {
		t.Fatal("sky 不应认")
	}
}

func TestBuildMaskKeepsSoftAlphaAndInverts(t *testing.T) {
	src := cutoutPNG(t, 10, 4, 128)
	out, cov, w, h, err := buildMask(src, false)
	if err != nil || w != 10 || h != 4 {
		t.Fatalf("w=%d h=%d err=%v", w, h, err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	c := color.NRGBAModel.Convert(img.At(1, 1)).(color.NRGBA)
	if c != (color.NRGBA{255, 255, 255, 128}) { // 白色 + 原 alpha（软边不二值化）
		t.Fatalf("区域内像素 %+v", c)
	}
	if a := color.NRGBAModel.Convert(img.At(9, 1)).(color.NRGBA).A; a != 0 {
		t.Fatalf("区域外 alpha=%d", a)
	}
	if want := 0.5 * 128 / 255; cov < want-1e-6 || cov > want+1e-6 {
		t.Fatalf("coverage=%f want %f", cov, want)
	}
	out, cov, _, _, _ = buildMask(src, true)
	img, _ = png.Decode(bytes.NewReader(out))
	if a := color.NRGBAModel.Convert(img.At(9, 1)).(color.NRGBA).A; a != 255 {
		t.Fatalf("取反后区域外 alpha=%d", a)
	}
	if a := color.NRGBAModel.Convert(img.At(1, 1)).(color.NRGBA).A; a != 127 {
		t.Fatalf("取反后区域内 alpha=%d", a)
	}
	if cov < 0.74 || cov > 0.76 {
		t.Fatalf("取反 coverage=%f", cov)
	}
	if _, _, _, _, err := buildMask([]byte("not png"), false); err == nil {
		t.Fatal("坏图应报错")
	}
}

// ---------------------------------------------------------------- masker.Get

const maskSelect = "SELECT .* FROM `afv_studio_mask_cache` AS `mc` WHERE \\(mc.url_key = '[0-9a-f]{64}'\\) AND \\(mc.target = '%s'\\) AND \\(mc.parts = ''\\) LIMIT 1"

// maskSelectParts 带部位的缓存查询。
func maskSelectParts(target, parts string) string {
	return "SELECT .* FROM `afv_studio_mask_cache` AS `mc` WHERE \\(mc.url_key = '[0-9a-f]{64}'\\) AND \\(mc.target = '" + target +
		"'\\) AND \\(mc.parts = '" + parts + "'\\) LIMIT 1"
}

var maskCols = []string{"id", "url_key", "target", "parts", "source_url", "status", "mask_rid", "mask_url", "part_masks", "coverage",
	"width", "height", "model_group", "gateway_call_id", "cost_cny", "user_id", "hits", "created_at", "updated_at"}

func TestMaskerComputesStoresAndCaches(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: segCall("0.012")}
	media := &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 255)}
	m := &masker{repo: r.Mask, gw: gw, media: media, now: time.Now}

	mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_mask_cache`") + ".*'garment'.*1, 'aaaaaa-asset-mask', 'https://cdn.example.com/mask.png'.*'imgseg.aliyun-cloth', 'call_1', '0.012000', 7.*ON DUPLICATE KEY UPDATE").
		WillReturnResult(sqlmock.NewResult(1, 1))
	res, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png?sig=1", "garment", nil)
	if err != nil || res.URL != "https://cdn.example.com/mask.png" || res.Cached || res.Target != MaskGarment {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	req := gw.calls[0]
	if req.Operation != "image.segment" || req.ModelGroup != "imgseg.aliyun-cloth" || req.Labels["kind"] != "mask" {
		t.Fatalf("网关请求 %+v", req)
	}
	if !strings.HasPrefix(gw.keys[0], "studio-mask:garment:") {
		t.Fatalf("幂等键 %s", gw.keys[0])
	}
	if len(media.ingested) == 0 || len(media.copied) != 1 {
		t.Fatal("蒙版应转存到素材库桶")
	}

	// 第二次：命中缓存，不再调网关，命中计数 +1
	now := time.Now()
	mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "garment")).WillReturnRows(sqlmock.NewRows(maskCols).
		AddRow(5, strings.Repeat("a", 64), "garment", "", "https://cdn.example.com/r.png", 1, "aaaaaa-asset-mask",
			"https://cdn.example.com/mask.png", nil, "0.5", 8, 8, "imgseg.aliyun-cloth", "call_1", "0.012", 7, 0, now, now))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_mask_cache` SET hits = hits + 1 WHERE (id = 5)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	res, err = m.Get(context.Background(), 8, "https://cdn.example.com/r.png?sig=2", "garment", nil)
	if err != nil || !res.Cached || res.URL != "https://cdn.example.com/mask.png" {
		t.Fatalf("缓存命中 res=%+v err=%v", res, err)
	}
	if len(gw.calls) != 1 {
		t.Fatalf("命中缓存不应再调网关：%d", len(gw.calls))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMaskerBackgroundInvertsSubject(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: segCall("0")}
	media := &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 255)}
	m := &masker{repo: r.Mask, gw: gw, media: media, now: time.Now}
	mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "background")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "background", nil); err != nil {
		t.Fatal(err)
	}
	if gw.calls[0].ModelGroup != "imgseg.aliyun-hd" {
		t.Fatalf("背景用主体抠图：%s", gw.calls[0].ModelGroup)
	}
	img, _ := png.Decode(bytes.NewReader(media.ingested))
	if a := color.NRGBAModel.Convert(img.At(7, 0)).(color.NRGBA).A; a != 255 {
		t.Fatalf("主体外应不透明 alpha=%d", a)
	}
}

func TestMaskerNoRegion(t *testing.T) {
	// 1) 抠图几乎全透明 → 没识别到，按 status=2 缓存
	r, mock := newMockRepo(t)
	m := &masker{repo: r.Mask, gw: &fakeMaskGW{resp: segCall("0.01")}, media: &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 0)}, now: time.Now}
	mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "person")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_mask_cache`") + ".*'person'.*, 2, NULL, NULL,").WillReturnResult(sqlmock.NewResult(1, 1))
	_, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "person", nil)
	if code, msg, _ := xerr.Parse(err); code != xerr.NotFound || msg != msgMaskNoRegion {
		t.Fatalf("code=%d msg=%s", code, msg)
	}

	// 2) 供应商判输入不可用（INPUT_REJECTED）→ 同样按没识别到
	r2, mock2 := newMockRepo(t)
	failed := &aigateway.Call{ID: "call_2", Status: aigateway.StatusFailed, Failure: &aigateway.Failure{Class: "INPUT_REJECTED", Code: "vendor.input"}}
	m2 := &masker{repo: r2.Mask, gw: &fakeMaskGW{resp: failed}, media: &fakeMaskMedia{}, now: time.Now}
	mock2.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock2.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := m2.Get(context.Background(), 7, "https://cdn.example.com/r.png", "garment", nil); !errors.Is(err, errMaskNoRegion) {
		t.Fatalf("err=%v", err)
	}

	// 3) 24 小时内的「没识别到」缓存：直接返回，不调网关
	r3, mock3 := newMockRepo(t)
	gw3 := &fakeMaskGW{resp: segCall("0")}
	m3 := &masker{repo: r3.Mask, gw: gw3, media: &fakeMaskMedia{}, now: time.Now}
	now := time.Now()
	mock3.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "garment")).WillReturnRows(sqlmock.NewRows(maskCols).
		AddRow(9, strings.Repeat("b", 64), "garment", "", "u", 2, nil, nil, nil, "0", 8, 8, "g", nil, "0", 7, 0, now, now))
	mock3.ExpectExec("UPDATE `afv_studio_mask_cache` SET hits").WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := m3.Get(context.Background(), 7, "https://cdn.example.com/r.png", "garment", nil); !errors.Is(err, errMaskNoRegion) || len(gw3.calls) != 0 {
		t.Fatalf("err=%v calls=%d", err, len(gw3.calls))
	}
}

func TestMaskerServiceFailureIsNotCached(t *testing.T) {
	for name, gw := range map[string]*fakeMaskGW{
		"submit error": {err: errors.New("boom")},
		"vendor failed": {resp: &aigateway.Call{ID: "c", Status: aigateway.StatusFailed,
			Failure: &aigateway.Failure{Class: "VENDOR_FAILED"}}},
	} {
		r, mock := newMockRepo(t)
		m := &masker{repo: r.Mask, gw: gw, media: &fakeMaskMedia{}, now: time.Now}
		mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "product")).WillReturnRows(sqlmock.NewRows(maskCols))
		_, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "product", nil)
		if code, _, _ := xerr.Parse(err); code != xerr.ThirdPartyError {
			t.Fatalf("%s: code=%d err=%v", name, code, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil { // 没有 INSERT：服务失败不落缓存
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestSegmentMaskValidatesInput(t *testing.T) {
	m := &masker{}
	owns := func(u string) bool { return strings.HasPrefix(u, "https://cdn.example.com/") }
	cases := []struct {
		in   *studio.SegmentMaskReq
		code int
	}{
		{&studio.SegmentMaskReq{ImageUrl: "https://cdn.example.com/a.png", Target: "garment"}, xerr.ParamMissing},
		{&studio.SegmentMaskReq{UserId: 1, Target: "garment"}, xerr.ParamMissing},
		{&studio.SegmentMaskReq{UserId: 1, ImageUrl: "https://cdn.example.com/a.png", Target: "sky"}, xerr.ParamError},
		{&studio.SegmentMaskReq{UserId: 1, ImageUrl: "https://evil.example.org/a.png", Target: "garment"}, xerr.ParamError},
		{&studio.SegmentMaskReq{UserId: 1, ImageUrl: "https://cdn.example.com/a.png", Target: "garment"}, xerr.NotReady},
	}
	for _, c := range cases {
		_, err := segmentMask(context.Background(), m, owns, c.in)
		if code, _, _ := xerr.Parse(err); code != c.code {
			t.Fatalf("%+v: code=%d want %d (%v)", c.in, code, c.code, err)
		}
	}
}

// 成品服装蒙版：只给商拍 / 姿态补，异步写 mask_url；图片生成不补；没识别到不写。
func TestAttachMaskAsync(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: segCall("0.01")}
	m := &masker{repo: r.Mask, gw: gw, media: &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 255)}, now: time.Now}
	f := &flow{repo: r, mask: m}

	f.attachMaskAsync(&model.Task{ID: 1, Kind: model.KindImageGen}, 7, "https://cdn.example.com/r.png", nil)
	f.wg.Wait()
	if len(gw.calls) != 0 {
		t.Fatal("图片生成不自动补服装蒙版")
	}

	mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET mask_url = 'https://cdn.example.com/mask.png' WHERE (id = 2)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	f.attachMaskAsync(&model.Task{ID: 2, Kind: model.KindTryon}, 7, "https://cdn.example.com/r.png", nil)
	f.wg.Wait()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// 没配蒙版服务：直接跳过
	(&flow{repo: r}).attachMaskAsync(&model.Task{ID: 3, Kind: model.KindPose}, 7, "https://cdn.example.com/r.png", nil)
}

// x-oss-process 改变像素内容，必须进缓存键；签名参数仍然去掉。
func TestNormalizeMaskURLKeepsProcess(t *testing.T) {
	a, ka, _ := normalizeMaskURL("https://cdn.example.com/a.png?x-oss-process=style%2Fwm&Signature=1")
	b, kb, _ := normalizeMaskURL("https://cdn.example.com/a.png?Signature=2&x-oss-process=style%2Fwm")
	_, kc, _ := normalizeMaskURL("https://cdn.example.com/a.png?Signature=2")
	if ka != kb || a != b || ka == kc || !strings.Contains(a, "x-oss-process=style%2Fwm") {
		t.Fatalf("a=%q ka=%s kb=%s kc=%s", a, ka, kb, kc)
	}
}
