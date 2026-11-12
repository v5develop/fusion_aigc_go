package logic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------- 商拍 model_views

func tryonWithViews(models int, views ...string) *studio.SubmitTryonReq {
	r := tryon()
	r.ModelImageUrls = urls(models)
	r.ModelViews = views
	return r
}

func TestCheckModelViews(t *testing.T) {
	// 不传 / 空数组 = 老行为
	for _, v := range [][]string{nil, {}} {
		got, err := CheckModelViews(1, v, 3)
		if err != nil || got != nil {
			t.Fatalf("空应回 nil：%v %v", got, err)
		}
	}
	got, err := CheckModelViews(1, []string{"front", " BACK", "Side"}, 3)
	if err != nil || !reflect.DeepEqual(got, []string{"front", "back", "side"}) {
		t.Fatalf("%v %v", got, err)
	}
	bads := map[string][]string{
		"数量少于模特": {"front", "back"},
		"数量多于模特": {"front", "back", "side", "front"},
		"未知取值":   {"front", "left", "side"},
		"中文":     {"front", "背面", "side"},
		"空项":     {"front", "", "side"},
	}
	for name, v := range bads {
		_, err := CheckModelViews(1, v, 3)
		wantCode(t, name, err, xerr.ParamError)
	}
	_, err = CheckModelViews(1, []string{"front"}, 3)
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "数量（1）") || !strings.Contains(msg, "模特图数量（3）") {
		t.Fatalf("长度错误要写清两边数量：%q", msg)
	}
	_, err = CheckModelViews(1, []string{"front", "left", "side"}, 3)
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "第 2 项") || !strings.Contains(msg, "left") || !strings.Contains(msg, "front / back / side") {
		t.Fatalf("取值错误要指出第几项与可选值：%q", msg)
	}
}

func TestTryonModelViewsSplitAndPayload(t *testing.T) {
	// 3 位模特 × 每位 2 张，各自一个面：每位模特各有自己的任务（model_index 0..2），视角随批次 payload 落库
	in := tryonWithViews(3, "front", "back", "side")
	v, err := ValidateTryon(in, nil)
	wantCode(t, "三模特三视角", err, 0)
	sp := tryonSpec(in, v)
	want := []slot{{0, 0, 0}, {0, 1, 0}, {1, 0, 0}, {1, 1, 0}, {2, 0, 0}, {2, 1, 0}}
	if !reflect.DeepEqual(sp.Slots, want) {
		t.Fatalf("拆任务：%v", sp.Slots)
	}
	if got, _ := sp.Payload["model_views"].([]string); !reflect.DeepEqual(got, []string{"front", "back", "side"}) {
		t.Fatalf("payload.model_views = %v", sp.Payload["model_views"])
	}
	// 历史回填：payload → 编排用的 batchPayload，第 i 位模特的面 = model_views[i]
	raw, _, err := payloadOf(sp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	p, err := parsePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range []string{"front", "back", "side", ""} { // 越界 → ""（不筛）
		if p.viewOf(i) != w {
			t.Fatalf("viewOf(%d) = %q，want %q", i, p.viewOf(i), w)
		}
	}
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if mv, _ := back["model_views"].([]any); len(mv) != 3 || mv[1] != "back" {
		t.Fatalf("payload 往返丢了 model_views：%s", raw)
	}

	// 不传：payload 没有这个键（幂等哈希与旧版逐字一致）；老客户端 / AIGC 桥行为不变
	old := tryon()
	ov, err := ValidateTryon(old, nil)
	wantCode(t, "老行为", err, 0)
	osp := tryonSpec(old, ov)
	if _, has := osp.Payload["model_views"]; has || ov.ModelViews != nil {
		t.Fatalf("不传 model_views 时不能写 payload：%v", osp.Payload)
	}
	// 同样的请求加上视角后指纹必然不同（同一幂等键换了视角 = 不同请求）
	_, h1, _ := payloadOf(osp.Payload)
	_, h2, _ := payloadOf(sp.Payload)
	if h1 == h2 {
		t.Fatal("视角进指纹")
	}
}

func TestTryonModelViewsRejected(t *testing.T) {
	for name, in := range map[string]*studio.SubmitTryonReq{
		"数量不符": tryonWithViews(2, "front"),
		"取值非法": tryonWithViews(2, "front", "left"),
	} {
		_, err := ValidateTryon(in, nil)
		wantCode(t, name, err, xerr.ParamError)
	}
}

// ---------------------------------------------------------------- 姿态 garment_image_urls

func garmentURLs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("https://oss.example.com/garment-%d.jpg", i)
	}
	return out
}

func poseWithGarments(n int) *studio.SubmitPoseReq {
	return &studio.SubmitPoseReq{UserId: 1, RequestId: "r", SourceImageUrl: okURL, PoseImageUrls: urls(2), GarmentImageUrls: garmentURLs(n)}
}

func TestPoseGarmentsValidation(t *testing.T) {
	groups := map[string]string{"通用": "img.nano-banana-2.1", "GPT": "img.gpt-image-2.6-sunburst"}
	v, err := ValidatePoseCap(poseWithGarments(6), groups, nil)
	wantCode(t, "6 张", err, 0)
	if len(v.Garments) != 6 {
		t.Fatalf("%v", v.Garments)
	}
	// 不传 = 老行为
	v, err = ValidatePoseCap(&studio.SubmitPoseReq{UserId: 1, RequestId: "r", SourceImageUrl: okURL, PoseImageUrls: urls(1)}, groups, nil)
	if err != nil || v.Garments != nil {
		t.Fatalf("%v %v", v, err)
	}

	_, err = ValidatePoseCap(poseWithGarments(7), groups, nil)
	wantCode(t, "7 张", err, xerr.ParamError)
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "最多 6 张") {
		t.Fatalf("%q", msg)
	}

	r := poseWithGarments(1)
	r.GarmentImageUrls = []string{"blob:https://x/1"}
	_, err = ValidatePoseCap(r, groups, nil)
	wantCode(t, "blob 地址", err, xerr.ParamError)

	// 多人（带配对 / 声明了多人）+ 服装图：拒绝，文案固定
	r = poseWithGarments(1)
	r.SourcePersonCount = 2
	r.Mappings = []*studio.PoseMapping{{PoseIndex: 0, Mapping: []int32{1, 0}, PosePersonCount: 2}}
	_, err = ValidatePoseCap(r, groups, nil)
	wantCode(t, "多人配对", err, xerr.ParamError)
	if _, msg, _ := xerr.Parse(err); msg != "多人姿态暂不支持服装参考图" {
		t.Fatalf("%q", msg)
	}
	r = poseWithGarments(1)
	r.SourcePersonCount = 3
	_, err = ValidatePoseCap(r, groups, nil)
	wantCode(t, "声明多人", err, xerr.ParamError)
	// 多人配对不带服装图照常通过（老行为）
	r = poseWithGarments(0)
	r.GarmentImageUrls = nil
	r.SourcePersonCount = 2
	r.Mappings = []*studio.PoseMapping{{PoseIndex: 0, Mapping: []int32{1, 0}, PosePersonCount: 2}}
	_, err = ValidatePoseCap(r, groups, nil)
	wantCode(t, "多人无服装", err, 0)

	// 参考图上限：原图 + 姿势 + 服装 ≤ 上限。GPT 2.6 硬上限 6 → 服装最多 4；目录给 4 → 服装最多 2
	gpt := func(n int) error {
		r := poseWithGarments(n)
		r.Model = "GPT"
		_, err := ValidatePoseCap(r, groups, nil)
		return err
	}
	wantCode(t, "GPT2.6 四张服装", gpt(4), 0)
	err = gpt(5)
	wantCode(t, "GPT2.6 五张服装", err, xerr.ParamError)
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "最多支持 6 张参考图") || !strings.Contains(msg, "服装参考图最多 4 张") || !strings.Contains(msg, "已选 5 张") {
		t.Fatalf("上限文案要写清上限 / 占位 / 可放 / 已选：%q", msg)
	}
	cap4 := func(group string) int { return 4 }
	_, err = ValidatePoseCap(poseWithGarments(2), groups, cap4)
	wantCode(t, "目录上限 4 放 2 张", err, 0)
	_, err = ValidatePoseCap(poseWithGarments(3), groups, cap4)
	wantCode(t, "目录上限 4 放 3 张", err, xerr.ParamError)
	cap2 := func(group string) int { return 2 }
	_, err = ValidatePoseCap(poseWithGarments(1), groups, cap2)
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "不能再放服装参考图") {
		t.Fatalf("上限 2 的文案：%v", err)
	}
}

func TestPoseSpecGarments(t *testing.T) {
	in := poseWithGarments(3)
	v, err := ValidatePoseCap(in, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sp := poseSpec(in, v)
	if got, _ := sp.Payload["garment_image_urls"].([]string); len(got) != 3 || got[0] != garmentURLs(3)[0] {
		t.Fatalf("payload：%v", sp.Payload["garment_image_urls"])
	}
	// 归属校验覆盖服装图，报错字段名可定位
	if n := len(sp.URLs); n != 1+2+3 || sp.URLs[3].field != "服装参考图第 1 张" || sp.URLs[5].field != "服装参考图第 3 张" {
		t.Fatalf("URLs：%+v", sp.URLs)
	}
	if sp.UserRefs != 3 {
		t.Fatalf("UserRefs = %d", sp.UserRefs)
	}
	// 历史回填
	raw, _, _ := payloadOf(sp.Payload)
	p, err := parsePayload(raw)
	if err != nil || len(p.GarmentImageURLs) != 3 {
		t.Fatalf("%v %+v", err, p)
	}
	// 不传：payload 没有这个键，UserRefs 为 0（冻结口径与旧版一致）
	old := &studio.SubmitPoseReq{UserId: 1, RequestId: "r", SourceImageUrl: okURL, PoseImageUrls: urls(2)}
	ov, _ := ValidatePose(old, nil)
	osp := poseSpec(old, ov)
	if _, has := osp.Payload["garment_image_urls"]; has || osp.UserRefs != 0 || len(osp.URLs) != 3 {
		t.Fatalf("老行为被改了：%v %d %d", osp.Payload, osp.UserRefs, len(osp.URLs))
	}
}

func TestSubmitPoseGarmentURLNotOwned(t *testing.T) {
	d, mock := newTestDeps(t, &fakeBill{})
	d.owns = func(u string) bool { return u != garmentURLs(2)[1] }
	in := poseWithGarments(2)
	in.UserId, in.RequestId = 7, "req-1"
	v, err := ValidatePoseCap(in, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(qBatchByReq).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, err = d.submit(t.Context(), poseSpec(in, v))
	if c, msg, _ := xerr.Parse(err); c != xerr.ParamError || msg != "服装参考图第 2 张 不是本服务上传的地址" {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)
}

// 计费：只有按请求参数冻结的载体（GPT Image 2.6 按实际成本）看参考图张数；姿态 = 2 + 服装图张数。
func TestImageUsagePoseCountsGarments(t *testing.T) {
	g := "img.gpt-image-2.6-sunburst"
	if u := imageUsage(model.KindPose, g, 0); u.RefImages != 2 {
		t.Fatalf("无服装 = 2：%d", u.RefImages)
	}
	if u := imageUsage(model.KindPose, g, -1); u.RefImages != 2 { // 报价没带服装张数：与旧版同
		t.Fatalf("未知 = 2：%d", u.RefImages)
	}
	if u := imageUsage(model.KindPose, g, 3); u.RefImages != 5 {
		t.Fatalf("3 张服装 = 5：%d", u.RefImages)
	}
	if u := imageUsage(model.KindPose, g, 6); u.RefImages != 6 { // 夹在组上限（GPT 2.6 = 6）
		t.Fatalf("上限 6：%d", u.RefImages)
	}
	// 商拍 / 图片生成不受影响
	if a, b := imageUsage(model.KindTryon, g, 0), imageUsage(model.KindTryon, g, 4); a.RefImages != b.RefImages {
		t.Fatal("商拍不看 userRefs")
	}
}

func TestResolvePoseGarments(t *testing.T) {
	srcWith := func(people, refs, count int, gerr string) *reverse.PoseSource {
		d := reverse.PoseSourceData{HasPerson: true, PersonCount: people, People: make([]reverse.PoseSourcePerson, people), GarmentImageCount: count, GarmentError: gerr}
		for i := 0; i < refs; i++ {
			d.GarmentRefs = append(d.GarmentRefs, json.RawMessage(`{"slot":"上衣"}`))
		}
		raw, _ := json.Marshal(map[string]any{"has_person": true, "person_count": people, "garment_refs": d.GarmentRefs,
			"garment_image_count": count, "garment_person": 0, "garment_missing_views": "无", "garment_error": gerr, "outfit": []string{"衬衫"}})
		return &reverse.PoseSource{Source: d, Raw: raw}
	}
	// 没带服装图：原样
	s := srcWith(1, 0, 0, "")
	if used, w, out := resolvePoseGarments(s, 0); used != 0 || w != "" || out != s {
		t.Fatalf("%d %q", used, w)
	}
	// 正常：全部送
	if used, w, out := resolvePoseGarments(srcWith(1, 2, 3, ""), 3); used != 3 || w != "" || len(out.Source.GarmentRefs) != 2 {
		t.Fatalf("%d %q", used, w)
	}
	// sidecar 只读了 2 张：以它读的为准
	if used, _, _ := resolvePoseGarments(srcWith(1, 2, 2, ""), 3); used != 2 {
		t.Fatalf("used=%d", used)
	}
	// 读失败 / 没读出来 / 多人：不送服装图，提示用户，source 里的服装键被抹掉（提示词不会引用没送的图）
	for name, s := range map[string]*reverse.PoseSource{
		"读失败":  srcWith(1, 0, 0, "模型超时"),
		"没读出来": srcWith(1, 0, 3, ""),
		"多人":   srcWith(2, 2, 3, ""),
	} {
		used, w, out := resolvePoseGarments(s, 3)
		if used != 0 || w == "" {
			t.Fatalf("%s：%d %q", name, used, w)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out.Raw, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"garment_refs", "garment_image_count", "garment_missing_views", "garment_person", "garment_error"} {
			if _, has := m[k]; has {
				t.Fatalf("%s：%s 没抹掉", name, k)
			}
		}
		if _, has := m["outfit"]; !has || len(out.Source.GarmentRefs) != 0 {
			t.Fatalf("%s：不该动别的键", name)
		}
	}
}
