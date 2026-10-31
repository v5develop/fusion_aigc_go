package logic

import (
	"fmt"
	"strings"
	"testing"

	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"
)

const okURL = "https://oss.example.com/a.jpg"

func urls(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("https://oss.example.com/%d.jpg", i)
	}
	return out
}

func garments(n int, role, angle string) []*studio.GarmentImage {
	out := make([]*studio.GarmentImage, n)
	for i := range out {
		out[i] = &studio.GarmentImage{Url: fmt.Sprintf("https://oss.example.com/g%d.jpg", i), Role: role, Angle: angle}
	}
	return out
}

func tryon() *studio.SubmitTryonReq {
	return &studio.SubmitTryonReq{
		UserId: 1, RequestId: "req-1", ModelImageUrls: urls(2), GarmentImages: garments(3, "上衣", "front"),
		Items: []string{"top"}, CountPerModel: 2,
	}
}

// wantCode 断言错误是指定业务码（nil code=0 表示期望成功）。
func wantCode(t *testing.T, name string, err error, code int) {
	t.Helper()
	if code == 0 {
		if err != nil {
			t.Fatalf("%s: 期望通过，得到 %v", name, err)
		}
		return
	}
	got, msg, ok := xerr.Parse(err)
	if !ok || got != code {
		t.Fatalf("%s: 期望码 %d，得到 %v", name, code, err)
	}
	if strings.TrimSpace(msg) == "" {
		t.Fatalf("%s: 错误信息为空", name)
	}
}

func TestRequestIDRequired(t *testing.T) {
	r := tryon()
	r.RequestId = "  "
	_, err := ValidateTryon(r, nil)
	wantCode(t, "tryon 无幂等键", err, xerr.ParamMissing)

	_, err = ValidatePose(&studio.SubmitPoseReq{UserId: 1, SourceImageUrl: okURL, PoseImageUrls: urls(1)}, nil)
	wantCode(t, "pose 无幂等键", err, xerr.ParamMissing)

	_, err = ValidateImageGen(&studio.SubmitImageGenReq{UserId: 1, Prompt: "x", Count: 1}, nil)
	wantCode(t, "imagegen 无幂等键", err, xerr.ParamMissing)

	r = tryon()
	r.RequestId = strings.Repeat("a", MaxRequestIDLen+1)
	_, err = ValidateTryon(r, nil)
	wantCode(t, "幂等键过长", err, xerr.ParamError)

	r = tryon()
	r.UserId = 0
	_, err = ValidateTryon(r, nil)
	wantCode(t, "无用户", err, xerr.ParamMissing)
}

func TestItemsLimitAndExclusive(t *testing.T) {
	cases := []struct {
		items []string
		code  int
	}{
		{[]string{"top"}, 0},
		{[]string{"top", "bottom", "shoes"}, 0},
		{[]string{"dress", "outer", "accessory"}, 0},
		{[]string{"bra", "panties"}, 0},
		{[]string{"TOP"}, 0}, // 大小写归一
		{nil, xerr.ParamMissing},
		{[]string{"top", "bottom", "shoes", "outer"}, xerr.ParamError}, // 超过 3 件
		{[]string{"hat"}, xerr.ParamError},                             // 未知
		{[]string{"top", "top"}, xerr.ParamError},                      // 重复
		{[]string{"dress", "top"}, xerr.ParamError},                    // 裙↔上衣
		{[]string{"bottom", "dress"}, xerr.ParamError},                 // 裙↔下装
		{[]string{"bra", "top"}, xerr.ParamError},                      // 文胸↔上衣
		{[]string{"bra", "dress"}, xerr.ParamError},                    // 文胸↔裙
		{[]string{"panties", "bottom"}, xerr.ParamError},               // 内裤↔下装
		{[]string{"dress", "panties"}, xerr.ParamError},                // 内裤↔裙
	}
	for _, c := range cases {
		_, err := CheckItems(c.items)
		wantCode(t, fmt.Sprintf("items=%v", c.items), err, c.code)
	}
}

func TestGarmentImages(t *testing.T) {
	// 9 张：3 件 × 3 张
	ok := append(append(garments(3, "上衣", "front"), garments(3, "下装", "back")...), garments(3, "鞋子", "")...)
	out, err := CheckGarments(ok)
	wantCode(t, "9 张", err, 0)
	if out[8].Angle != "unknown" {
		t.Fatalf("空角度应归一为 unknown，得到 %q", out[8].Angle)
	}
	_, err = CheckGarments(garments(6, "上衣", "front"))
	wantCode(t, "单件 6 张", err, 0)

	_, err = CheckGarments(nil)
	wantCode(t, "0 张", err, xerr.ParamMissing)
	_, err = CheckGarments(append(garments(5, "上衣", "front"), garments(5, "下装", "front")...))
	wantCode(t, "超过 9 张", err, xerr.ParamError)
	_, err = CheckGarments(garments(7, "上衣", "front"))
	wantCode(t, "单件超过 6 张", err, xerr.ParamError)
	_, err = CheckGarments(garments(1, "上衣", "top-left"))
	wantCode(t, "未知角度", err, xerr.ParamError)
	for _, a := range Angles {
		_, err = CheckGarments(garments(1, "上衣", strings.ToUpper(a)))
		wantCode(t, "角度 "+a, err, 0)
	}
	_, err = CheckGarments([]*studio.GarmentImage{{Url: "blob:https://x/1", Role: "上衣"}})
	wantCode(t, "服装图 blob", err, xerr.ParamError)
}

func TestTryonModelsAndCount(t *testing.T) {
	for _, n := range []int{1, 9} {
		r := tryon()
		r.ModelImageUrls = urls(n)
		_, err := ValidateTryon(r, nil)
		wantCode(t, fmt.Sprintf("模特 %d", n), err, 0)
	}
	r := tryon()
	r.ModelImageUrls = nil
	_, err := ValidateTryon(r, nil)
	wantCode(t, "模特 0", err, xerr.ParamMissing)
	r = tryon()
	r.ModelImageUrls = urls(10)
	_, err = ValidateTryon(r, nil)
	wantCode(t, "模特 10", err, xerr.ParamError)

	for _, c := range []struct {
		n    int32
		code int
	}{{1, 0}, {9, 0}, {0, xerr.ParamError}, {10, xerr.ParamError}, {-1, xerr.ParamError}} {
		r := tryon()
		r.CountPerModel = c.n
		_, err := ValidateTryon(r, nil)
		wantCode(t, fmt.Sprintf("每位张数 %d", c.n), err, c.code)
	}
}

func TestTryonOrderAndNormalize(t *testing.T) {
	// 校验顺序：单品 → 服装图 → 模特（三者都错时报单品）
	r := tryon()
	r.Items = []string{"dress", "top"}
	r.GarmentImages = nil
	r.ModelImageUrls = nil
	_, err := ValidateTryon(r, nil)
	if _, msg, _ := xerr.Parse(err); !strings.Contains(msg, "不能同时") {
		t.Fatalf("应先报单品互斥，得到 %v", err)
	}

	r = tryon()
	r.Items = []string{"top", "bottom"}
	r.Lengths = map[string]string{"top": "短款", "dress": "历史 key", "bottom": " "}
	r.Sleeves = map[string]string{"top": "长袖", "outer": "历史 key"}
	r.Resolution = "2K"
	in, err := ValidateTryon(r, nil)
	wantCode(t, "归一", err, 0)
	if len(in.Lengths) != 1 || in.Lengths["top"] != "短款" {
		t.Fatalf("lengths 应只留 items 里的非空键：%v", in.Lengths)
	}
	if len(in.Sleeves) != 1 || in.Sleeves["top"] != "长袖" {
		t.Fatalf("sleeves 应只留 items 里的键：%v", in.Sleeves)
	}
	if in.Resolution != "2k" || in.AspectRatio != "" || in.SetMode != "one" || in.ModelName != "通用" {
		t.Fatalf("默认值 / 归一不对：%+v", in)
	}

	r = tryon()
	r.SetMode = "three"
	_, err = ValidateTryon(r, nil)
	wantCode(t, "成套方式", err, xerr.ParamError)

	r = tryon()
	r.Prompt = strings.Repeat("字", MaxPromptLen)
	_, err = ValidateTryon(r, nil)
	wantCode(t, "prompt 3000 字", err, 0)
	r.Prompt += "字"
	_, err = ValidateTryon(r, nil)
	wantCode(t, "prompt 3001 字", err, xerr.ParamError)

	// 旧版兼容位：只有 cloth_image_url
	r = tryon()
	r.GarmentImages = nil
	r.ClothImageUrl = okURL
	in, err = ValidateTryon(r, nil)
	wantCode(t, "cloth_image_url 兼容", err, 0)
	if len(in.Garments) != 1 || in.Garments[0].Url != okURL {
		t.Fatalf("兼容位没转成服装图：%v", in.Garments)
	}
}

func TestURLScheme(t *testing.T) {
	bad := []string{
		"blob:https://app.example.com/0b1c", "BLOB:xx", "data:image/png;base64,AAAA", "file:///tmp/a.jpg",
		"ftp://x/a.jpg", "//cdn.example.com/a.jpg", "oss/a.jpg", "https://", "",
	}
	for _, u := range bad {
		err := checkURL("图", u)
		if err == nil {
			t.Fatalf("%q 应被拒绝", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:8888/files/a.jpg", "https://oss.example.com/a.jpg?x=1", " https://a.b/c.png "} {
		if err := checkURL("图", u); err != nil {
			t.Fatalf("%q 应通过：%v", u, err)
		}
	}
	r := tryon()
	r.ModelImageUrls = []string{okURL, "blob:https://x/2"}
	_, err := ValidateTryon(r, nil)
	wantCode(t, "模特图 blob", err, xerr.ParamError)
}

func TestResolutionNormalize(t *testing.T) {
	for in, want := range map[string]string{"": "1k", "1K": "1k", "1k": "1k", " 2K ": "2k", "4k": "4k"} {
		got, err := NormalizeResolution(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeResolution(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"8k", "1080p", "2"} {
		_, err := NormalizeResolution(in)
		wantCode(t, "分辨率 "+in, err, xerr.ParamError)
	}
	for in, want := range map[string]string{"": "3:4", "16:9": "16:9", " 1:1 ": "1:1"} {
		got, err := NormalizeAspectRatio(in, "3:4")
		if err != nil || got != want {
			t.Fatalf("NormalizeAspectRatio(%q)=%q,%v", in, got, err)
		}
	}
	for _, in := range []string{"0:1", "3/4", "a:b", "3:4:5"} {
		_, err := NormalizeAspectRatio(in, "3:4")
		wantCode(t, "比例 "+in, err, xerr.ParamError)
	}
}

func TestModelMapping(t *testing.T) {
	groups := config.DefaultModelGroups
	for in, want := range map[string]string{"": "img.gpt-image-2", "通用": "img.gpt-image-2", "gpt": "img.gpt-image-2",
		"Banana": "img.nano-banana-2.1", "seedream": "img.seedream-5-pro"} {
		_, g, err := NormalizeModel(in, groups)
		if err != nil || g != want {
			t.Fatalf("NormalizeModel(%q)=%q,%v want %q", in, g, err, want)
		}
	}
	_, _, err := NormalizeModel("midjourney", groups)
	wantCode(t, "未知模型", err, xerr.ParamError)
}

func TestPose(t *testing.T) {
	base := func() *studio.SubmitPoseReq {
		return &studio.SubmitPoseReq{UserId: 1, RequestId: "r", SourceImageUrl: okURL, PoseImageUrls: urls(3)}
	}
	r := base()
	r.PosePrompts = map[int32]string{0: "抬手", 2: " "}
	in, err := ValidatePose(r, nil)
	wantCode(t, "姿态正常", err, 0)
	if len(in.PosePrompts) != 1 || in.Resolution != "1k" || in.AspectRatio != "" {
		t.Fatalf("姿态归一不对：%+v", in)
	}
	r = base()
	r.PoseImageUrls = urls(9)
	_, err = ValidatePose(r, nil)
	wantCode(t, "姿势 9", err, 0)
	r.PoseImageUrls = urls(10)
	_, err = ValidatePose(r, nil)
	wantCode(t, "姿势 10", err, xerr.ParamError)
	r.PoseImageUrls = nil
	_, err = ValidatePose(r, nil)
	wantCode(t, "姿势 0", err, xerr.ParamMissing)

	r = base()
	r.SourceImageUrl = ""
	_, err = ValidatePose(r, nil)
	wantCode(t, "无人物图", err, xerr.ParamMissing)
	r = base()
	r.SourceImageUrl = "blob:https://x/1"
	_, err = ValidatePose(r, nil)
	wantCode(t, "人物图 blob", err, xerr.ParamError)
	r = base()
	r.PosePrompts = map[int32]string{3: "越界"}
	_, err = ValidatePose(r, nil)
	wantCode(t, "姿势序号越界", err, xerr.ParamError)
	r = base()
	r.GlobalPrompt = strings.Repeat("a", MaxPromptLen+1)
	_, err = ValidatePose(r, nil)
	wantCode(t, "全局说明超长", err, xerr.ParamError)
}

func TestImageGen(t *testing.T) {
	base := func() *studio.SubmitImageGenReq {
		return &studio.SubmitImageGenReq{UserId: 1, RequestId: "r", Prompt: "一只猫", Count: 1, Resolution: "4K"}
	}
	in, err := ValidateImageGen(base(), nil)
	wantCode(t, "图片生成正常", err, 0)
	if in.Resolution != "4k" || in.AspectRatio != "1:1" {
		t.Fatalf("图片生成默认值不对：%+v", in)
	}
	for _, c := range []struct {
		n    int32
		code int
	}{{9, 0}, {0, xerr.ParamError}, {10, xerr.ParamError}} {
		r := base()
		r.Count = c.n
		_, err := ValidateImageGen(r, nil)
		wantCode(t, fmt.Sprintf("count %d", c.n), err, c.code)
	}
	r := base()
	r.Prompt = "  "
	_, err = ValidateImageGen(r, nil)
	wantCode(t, "空提示词", err, xerr.ParamMissing)
	r = base()
	r.Prompt = strings.Repeat("字", MaxPromptLen+1)
	_, err = ValidateImageGen(r, nil)
	wantCode(t, "提示词超长", err, xerr.ParamError)
	r = base()
	r.ImageUrls = urls(9)
	_, err = ValidateImageGen(r, nil)
	wantCode(t, "参考图 9", err, 0)
	r.ImageUrls = urls(10)
	_, err = ValidateImageGen(r, nil)
	wantCode(t, "参考图 10", err, xerr.ParamError)
	r = base()
	r.ImageUrls = []string{"data:image/png;base64,AA"}
	_, err = ValidateImageGen(r, nil)
	wantCode(t, "参考图 data:", err, xerr.ParamError)
}

func TestCursorAndLimit(t *testing.T) {
	c := FormatCursor(1761800000123, 42)
	ms, id, err := ParseCursor(c)
	if err != nil || ms != 1761800000123 || id != 42 {
		t.Fatalf("游标往返失败：%s → %d %d %v", c, ms, id, err)
	}
	if ms, id, err := ParseCursor(""); err != nil || ms != 0 || id != 0 {
		t.Fatal("空游标应为第一页")
	}
	for _, bad := range []string{"abc", "123", "1_x", "0_1", "1_0", "-1_2"} {
		_, _, err := ParseCursor(bad)
		wantCode(t, "游标 "+bad, err, xerr.ParamError)
	}
	for in, want := range map[int32]int32{0: 20, -5: 20, 10: 10, 50: 50, 51: 50} {
		if got := NormalizeLimit(in); got != want {
			t.Fatalf("NormalizeLimit(%d)=%d", in, got)
		}
	}
	wantCode(t, "kind", checkKind("video"), xerr.ParamError)
	wantCode(t, "kind 空", checkKind(""), 0)
}

// Seedream 档位在提交时归一：4K → 2K、1K 非 1:1 → 2K，并带告警；其它组不动。
func TestValidateSeedreamNormalized(t *testing.T) {
	groups := map[string]string{"即梦": "image.seedream-4", "通用": "image.gpt"}
	cases := []struct {
		model, res, ar, want string
		warn                 bool
	}{
		{"即梦", "4K", "1:1", "2k", true},
		{"即梦", "1k", "3:4", "2k", true},
		{"即梦", "1k", "1:1", "1k", false},
		{"即梦", "2k", "16:9", "2k", false},
		{"通用", "4k", "3:4", "4k", false},
	}
	for _, c := range cases {
		v, err := ValidateImageGen(&studio.SubmitImageGenReq{UserId: 7, RequestId: "r", Prompt: "猫", Count: 1,
			Model: c.model, Resolution: c.res, AspectRatio: c.ar}, groups)
		if err != nil {
			t.Fatal(err)
		}
		if v.Resolution != c.want || (len(v.Warnings) > 0) != c.warn {
			t.Fatalf("%+v: got %s warnings=%v", c, v.Resolution, v.Warnings)
		}
		sp := imageGenSpec(&studio.SubmitImageGenReq{UserId: 7, RequestId: "r"}, v)
		if sp.Payload["resolution"] != c.want || sp.Resolution != c.want {
			t.Fatalf("快照 / 计费档位应用归一值：%v %s", sp.Payload["resolution"], sp.Resolution)
		}
	}
	// 商拍不传比例（自动）+ Seedream 1K：比例未定，按 2K
	tv, err := ValidateTryon(&studio.SubmitTryonReq{UserId: 7, RequestId: "r", Items: []string{"top"}, CountPerModel: 1, Model: "即梦",
		ModelImageUrls: []string{"https://a/m.png"}, GarmentImages: []*studio.GarmentImage{{Url: "https://a/g.png", Role: "上衣"}}}, groups)
	if err != nil {
		t.Fatal(err)
	}
	if tv.Resolution != "2k" || len(tv.Warnings) == 0 {
		t.Fatalf("got %s %v", tv.Resolution, tv.Warnings)
	}
}
