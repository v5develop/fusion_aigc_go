package logic

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// partsMedia 按 rid 返回不同抠图、按幂等键给不同的素材库地址（逐件蒙版要能分辨）。
type partsMedia struct {
	mu      sync.Mutex
	cutouts map[string][]byte
	idems   []string
}

func (m *partsMedia) FetchBytes(_ context.Context, rid string) ([]byte, string, error) {
	if b, ok := m.cutouts[rid]; ok {
		return b, "image/png", nil
	}
	return nil, "", errors.New("no such rid")
}

func (m *partsMedia) IngestBytes(_ context.Context, _ string, _ []byte, _, idem string) (mediasvc.Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idems = append(m.idems, idem)
	return mediasvc.Resource{RID: "work:" + idem}, nil
}

func (m *partsMedia) CopyToLibrary(_ context.Context, rid, _, _ string) (mediasvc.Resource, error) {
	name := "all"
	if i := strings.LastIndex(rid, ":part:"); i >= 0 {
		name = rid[i+len(":part:"):]
	}
	return mediasvc.Resource{RID: "lib-" + name, URL: "https://cdn.example.com/mask-" + name + ".png"}, nil
}

func partsCall(parts ...string) *aigateway.Call {
	c := segCall("0.02")
	for _, p := range parts {
		c.Result.Parts = append(c.Result.Parts, aigateway.SegmentPart{Label: p, Image: &aigateway.MediaOut{Rid: "gw-" + p}})
	}
	return c
}

func TestNormalizeMaskParts(t *testing.T) {
	got, err := normalizeMaskParts(MaskGarment, []string{" PANTS", "tops", "pants", "", "hat"})
	if err != nil || !reflect.DeepEqual(got, []string{"tops", "pants", "hat"}) {
		t.Fatalf("got=%v err=%v（应去重并按固定顺序）", got, err)
	}
	if got, err := normalizeMaskParts(MaskGarment, nil); got != nil || err != nil {
		t.Fatalf("空 = 不细分：got=%v err=%v", got, err)
	}
	if got, err := normalizeMaskParts(MaskProduct, []string{" "}); got != nil || err != nil {
		t.Fatalf("全空白等于没传：got=%v err=%v", got, err)
	}
	for _, c := range []struct {
		target string
		parts  []string
	}{{MaskGarment, []string{"gloves"}}, {MaskPerson, []string{"tops"}}, {MaskGarment, []string{"上衣"}}} {
		if _, err := normalizeMaskParts(c.target, c.parts); err == nil {
			t.Fatalf("%v 应拒收", c)
		} else if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
			t.Fatalf("%v code=%d", c, code)
		}
	}
	if !reflect.DeepEqual(availableMaskParts(MaskGarment), []string{"tops", "coat", "pants", "skirt", "shoes", "bag", "hat"}) ||
		availableMaskParts(MaskBackground) != nil {
		t.Fatal("只有服装可细分")
	}
}

func TestTryonMaskParts(t *testing.T) {
	cases := []struct {
		kind    model.Kind
		payload string
		want    []string
	}{
		{model.KindTryon, `{"items":["bottom","top"]}`, []string{"tops", "pants"}},
		{model.KindTryon, `{"items":["dress","outer","shoes"]}`, []string{"coat", "skirt", "shoes"}},
		{model.KindTryon, `{"items":["accessory"]}`, []string{"bag", "hat"}},
		{model.KindTryon, `{"items":["bra","panties"]}`, []string{"tops", "pants"}},
		{model.KindTryon, `{"items":["unknown"]}`, nil},
		{model.KindTryon, `{}`, nil},
		{model.KindTryon, `not json`, nil},
		{model.KindPose, `{"items":["top"]}`, nil},
	}
	for _, c := range cases {
		if got := tryonMaskParts(c.kind, json.RawMessage(c.payload)); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s %s: got %v want %v", c.kind, c.payload, got, c.want)
		}
	}
}

// 多部位：一次网关调用（带 classes），合并蒙版 + 逐件蒙版；合并行带 part_masks，逐件另落单部位缓存行（成本 0）。
func TestMaskerMultiPartsOneCall(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: partsCall("tops", "pants")}
	media := &partsMedia{cutouts: map[string][]byte{
		"gwgwgw-work-cut": cutoutPNG(t, 8, 8, 255), "gw-tops": cutoutPNG(t, 8, 8, 200), "gw-pants": cutoutPNG(t, 8, 8, 0),
	}}
	m := &masker{repo: r.Mask, gw: gw, media: media, now: time.Now}

	mock.ExpectQuery(maskSelectParts("garment", "tops,pants")).WillReturnRows(sqlmock.NewRows(maskCols))
	// 合并行：parts='tops,pants'，part_masks 只含识别到的上衣（下装全透明 → 跳过）
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_mask_cache`") +
		".*'garment', 'tops,pants'.*1, 'lib-all', 'https://cdn.example.com/mask-all.png', " +
		regexp.QuoteMeta(`'[{"part":"tops","mask_url":"https://cdn.example.com/mask-tops.png"}]'`) +
		".*'imgseg.aliyun-cloth', 'call_1', '0.020000'").WillReturnResult(sqlmock.NewResult(1, 1))
	// 单部位行：parts='tops'，成本 0
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_mask_cache`") +
		".*'garment', 'tops'.*1, 'lib-tops', 'https://cdn.example.com/mask-tops.png', NULL.*'call_1', '0'").
		WillReturnResult(sqlmock.NewResult(2, 1))

	res, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "garment", []string{"pants", "tops"})
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://cdn.example.com/mask-all.png" ||
		!reflect.DeepEqual(res.Parts, []maskPart{{Part: "tops", URL: "https://cdn.example.com/mask-tops.png"}}) {
		t.Fatalf("res=%+v", res)
	}
	if len(gw.calls) != 1 {
		t.Fatalf("多部位只调一次网关：%d", len(gw.calls))
	}
	req := gw.calls[0]
	if got := req.Input.(map[string]any)["classes"]; !reflect.DeepEqual(got, []string{"tops", "pants"}) {
		t.Fatalf("classes=%v", got)
	}
	if req.Labels["parts"] != "tops,pants" || !strings.HasPrefix(gw.keys[0], "studio-mask:garment:tops,pants:") {
		t.Fatalf("labels=%v key=%s", req.Labels, gw.keys[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 不带部位：网关请求里没有 classes、幂等键与旧版一致（回归）。
func TestMaskerWithoutPartsSendsNoClasses(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: segCall("0")}
	m := &masker{repo: r.Mask, gw: gw, media: &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 255)}, now: time.Now}
	mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_mask_cache`") + ".*'garment', ''.*NULL, '0.000000'").
		WillReturnResult(sqlmock.NewResult(1, 1))
	res, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "garment", nil)
	if err != nil || res.Parts != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, ok := gw.calls[0].Input.(map[string]any)["classes"]; ok {
		t.Fatal("不细分不发 classes")
	}
	if _, ok := gw.calls[0].Labels["parts"]; ok {
		t.Fatal("不细分不打 parts 标签")
	}
	_, key, _ := normalizeMaskURL("https://cdn.example.com/r.png")
	if gw.keys[0] != "studio-mask:garment:"+key {
		t.Fatalf("幂等键应与旧版一致：%s", gw.keys[0])
	}
}

// 单部位：合并蒙版就是这一件，不再落额外的单部位行；命中缓存时逐件 = 合并。
func TestMaskerSinglePart(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: partsCall("coat")}
	media := &partsMedia{cutouts: map[string][]byte{"gwgwgw-work-cut": cutoutPNG(t, 8, 8, 255)}}
	m := &masker{repo: r.Mask, gw: gw, media: media, now: time.Now}
	mock.ExpectQuery(maskSelectParts("garment", "coat")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `afv_studio_mask_cache`") + ".*'garment', 'coat'.*'https://cdn.example.com/mask-all.png', NULL").
		WillReturnResult(sqlmock.NewResult(1, 1))
	res, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "garment", []string{"coat"})
	if err != nil || !reflect.DeepEqual(res.Parts, []maskPart{{Part: "coat", URL: "https://cdn.example.com/mask-all.png"}}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(media.idems) != 1 {
		t.Fatalf("单部位只转存一张：%v", media.idems)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	mock.ExpectQuery(maskSelectParts("garment", "coat")).WillReturnRows(sqlmock.NewRows(maskCols).
		AddRow(5, strings.Repeat("a", 64), "garment", "coat", "u", 1, "lib", "https://cdn.example.com/c.png", nil,
			"0.5", 8, 8, "imgseg.aliyun-cloth", "call_1", "0.02", 7, 0, now, now))
	mock.ExpectExec("UPDATE `afv_studio_mask_cache` SET hits").WillReturnResult(sqlmock.NewResult(0, 1))
	res, err = m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "garment", []string{"coat"})
	if err != nil || !res.Cached || !reflect.DeepEqual(res.Parts, []maskPart{{Part: "coat", URL: "https://cdn.example.com/c.png"}}) {
		t.Fatalf("cached res=%+v err=%v", res, err)
	}
}

func TestMaskerMultiPartsCacheHitReadsPartMasks(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{}
	m := &masker{repo: r.Mask, gw: gw, media: &partsMedia{}, now: time.Now}
	now := time.Now()
	pm := `[{"part":"tops","mask_url":"https://cdn.example.com/t.png"},{"part":"shoes","mask_url":"https://cdn.example.com/s.png"}]`
	mock.ExpectQuery(maskSelectParts("garment", "tops,shoes")).WillReturnRows(sqlmock.NewRows(maskCols).
		AddRow(6, strings.Repeat("a", 64), "garment", "tops,shoes", "u", 1, "lib", "https://cdn.example.com/all.png", pm,
			"0.5", 8, 8, "imgseg.aliyun-cloth", "call_1", "0.02", 7, 0, now, now))
	mock.ExpectExec("UPDATE `afv_studio_mask_cache` SET hits").WillReturnResult(sqlmock.NewResult(0, 1))
	res, err := m.Get(context.Background(), 7, "https://cdn.example.com/r.png", "garment", []string{"shoes", "tops"})
	if err != nil || !res.Cached || res.URL != "https://cdn.example.com/all.png" || len(res.Parts) != 2 || res.Parts[1].Part != "shoes" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(gw.calls) != 0 {
		t.Fatal("命中缓存不调网关")
	}
}

func TestSegmentMaskPartsContract(t *testing.T) {
	owns := func(u string) bool { return strings.HasPrefix(u, "https://cdn.example.com/") }
	for _, in := range []*studio.SegmentMaskReq{
		{UserId: 1, ImageUrl: "https://cdn.example.com/a.png", Target: "person", Parts: []string{"tops"}},
		{UserId: 1, ImageUrl: "https://cdn.example.com/a.png", Target: "garment", Parts: []string{"gloves"}},
	} {
		if _, err := segmentMask(context.Background(), &masker{}, owns, in); err == nil {
			t.Fatalf("%+v 应拒收", in)
		} else if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
			t.Fatalf("%+v code=%d", in, code)
		}
	}

	r, mock := newMockRepo(t)
	m := &masker{repo: r.Mask, gw: &fakeMaskGW{resp: partsCall("tops", "hat")}, now: time.Now,
		media: &partsMedia{cutouts: map[string][]byte{"gwgwgw-work-cut": cutoutPNG(t, 8, 8, 255),
			"gw-tops": cutoutPNG(t, 8, 8, 255), "gw-hat": cutoutPNG(t, 8, 8, 255)}}}
	mock.ExpectQuery(maskSelectParts("garment", "tops,hat")).WillReturnRows(sqlmock.NewRows(maskCols))
	for i := 0; i < 3; i++ {
		mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	}
	resp, err := segmentMask(context.Background(), m, owns, &studio.SegmentMaskReq{UserId: 1,
		ImageUrl: "https://cdn.example.com/a.png", Target: "Garment", Parts: []string{"hat", "tops"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Target != "garment" || resp.MaskUrl != "https://cdn.example.com/mask-all.png" || len(resp.Parts) != 2 ||
		resp.Parts[0].Part != "tops" || resp.Parts[1].MaskUrl != "https://cdn.example.com/mask-hat.png" ||
		len(resp.AvailableParts) != len(garmentParts) {
		t.Fatalf("resp=%+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 商拍成品：按所选单品带部位识别，mask_url = 合并、mask_parts = 逐件。
func TestAttachMaskAsyncWithParts(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &fakeMaskGW{resp: partsCall("tops", "pants")}
	media := &partsMedia{cutouts: map[string][]byte{"gwgwgw-work-cut": cutoutPNG(t, 8, 8, 255),
		"gw-tops": cutoutPNG(t, 8, 8, 255), "gw-pants": cutoutPNG(t, 8, 8, 255)}}
	f := &flow{repo: r, mask: &masker{repo: r.Mask, gw: gw, media: media, now: time.Now}}
	mock.ExpectQuery(maskSelectParts("garment", "tops,pants")).WillReturnRows(sqlmock.NewRows(maskCols))
	for i := 0; i < 3; i++ {
		mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET mask_url = 'https://cdn.example.com/mask-all.png', " +
		`mask_parts = '[{"part":"tops","mask_url":"https://cdn.example.com/mask-tops.png"},{"part":"pants","mask_url":"https://cdn.example.com/mask-pants.png"}]'` +
		" WHERE (id = 2)")).WillReturnResult(sqlmock.NewResult(0, 1))
	f.attachMaskAsync(&model.Task{ID: 2, Kind: model.KindTryon}, 7, "https://cdn.example.com/r.png", []string{"tops", "pants"})
	f.wg.Wait()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 带部位识别服务不可用（如网关还不认 classes）→ 退回整体服装蒙版；没识别到则不退。
func TestAttachMaskAsyncPartsFallback(t *testing.T) {
	r, mock := newMockRepo(t)
	gw := &seqGW{resps: []*aigateway.Call{
		{ID: "c1", Status: aigateway.StatusFailed, Failure: &aigateway.Failure{Class: "VENDOR_FAILED"}},
		segCall("0.01"),
	}}
	f := &flow{repo: r, mask: &masker{repo: r.Mask, gw: gw, media: &fakeMaskMedia{cutout: cutoutPNG(t, 8, 8, 255)}, now: time.Now}}
	mock.ExpectQuery(maskSelectParts("garment", "tops")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectQuery(strings.ReplaceAll(maskSelect, "%s", "garment")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET mask_url = 'https://cdn.example.com/mask.png' WHERE (id = 5)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	f.attachMaskAsync(&model.Task{ID: 5, Kind: model.KindTryon}, 7, "https://cdn.example.com/r.png", []string{"tops"})
	f.wg.Wait()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(gw.reqs) != 2 || gw.reqs[1].Input.(map[string]any)["classes"] != nil {
		t.Fatalf("第二次应不带 classes：%d", len(gw.reqs))
	}

	// 没识别到（供应商判图里没有服饰，InvalidImage.Content / noClothError）：不退回整体
	r2, mock2 := newMockRepo(t)
	gw2 := &seqGW{resps: []*aigateway.Call{noClothCall()}}
	f2 := &flow{repo: r2, mask: &masker{repo: r2.Mask, gw: gw2, media: &fakeMaskMedia{}, now: time.Now}}
	mock2.ExpectQuery(maskSelectParts("garment", "tops")).WillReturnRows(sqlmock.NewRows(maskCols))
	mock2.ExpectExec("INSERT INTO `afv_studio_mask_cache`").WillReturnResult(sqlmock.NewResult(1, 1))
	f2.attachMaskAsync(&model.Task{ID: 6, Kind: model.KindTryon}, 7, "https://cdn.example.com/r.png", []string{"tops"})
	f2.wg.Wait()
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(gw2.reqs) != 1 {
		t.Fatalf("没识别到不应退回：%d", len(gw2.reqs))
	}

	// 格式 / 尺寸不收：换不细分也一样不收，不退回、不落缓存
	r3, mock3 := newMockRepo(t)
	gw3 := &seqGW{resps: []*aigateway.Call{rejectedCall("vendor.input_rejected", "", "InvalidImage.Resolution", "")}}
	f3 := &flow{repo: r3, mask: &masker{repo: r3.Mask, gw: gw3, media: &fakeMaskMedia{}, now: time.Now}}
	mock3.ExpectQuery(maskSelectParts("garment", "tops")).WillReturnRows(sqlmock.NewRows(maskCols))
	f3.attachMaskAsync(&model.Task{ID: 7, Kind: model.KindTryon}, 7, "https://cdn.example.com/r.png", []string{"tops"})
	f3.wg.Wait()
	if err := mock3.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(gw3.reqs) != 1 {
		t.Fatalf("格式不收不应退回：%d", len(gw3.reqs))
	}
}

// seqGW 依次返回 resps。
type seqGW struct {
	mu    sync.Mutex
	resps []*aigateway.Call
	reqs  []aigateway.CallRequest
}

func (g *seqGW) Submit(_ context.Context, req aigateway.CallRequest, _ aigateway.SubmitOptions) (*aigateway.Call, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reqs = append(g.reqs, req)
	return g.resps[len(g.reqs)-1], nil
}

func (g *seqGW) Get(context.Context, string, time.Duration) (*aigateway.Call, error) {
	return nil, errors.New("not used")
}
