package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func keysOf(ss []Suggestion) string {
	var k []string
	for _, s := range ss {
		k = append(k, s.Key)
	}
	return strings.Join(k, ",")
}

func TestSuggestionRules(t *testing.T) {
	present := func(keys ...string) map[string]Dim {
		m := map[string]Dim{}
		for _, k := range keys {
			m[k] = Dim{Present: true, Desc: "x"}
		}
		return m
	}
	txt := func(kind string) []OnScreen { return []OnScreen{{Text: "t", Kind: kind}} }
	cases := []struct {
		name string
		r    *AnalysisResult
		want string
	}{
		{"有人物", &AnalysisResult{Dims: present("model")}, "swap_model"},
		{"只有主体列表也算有人", &AnalysisResult{Subjects: []Subject{{ID: "p1"}}}, "swap_model"},
		{"商品", &AnalysisResult{Dims: present("product")}, "swap_product"},
		{"服装", &AnalysisResult{Dims: present("garment")}, "swap_garment"},
		{"字幕", &AnalysisResult{OnScreenText: txt("subtitle")}, "clean_text"},
		{"水印", &AnalysisResult{OnScreenText: txt("watermark")}, "clean_text"},
		{"价格和标题是卖点不算要清理", &AnalysisResult{OnScreenText: txt("price")}, ""},
		{"什么都没有", &AnalysisResult{Dims: present("scene", "style")}, ""},
		{"顺序：人 > 商品 > 衣服 > 字幕", &AnalysisResult{Dims: present("garment", "product", "model")}, "swap_model,swap_product,swap_garment"},
		{"最多 3 条，字幕排在后面被挤掉", &AnalysisResult{Dims: present("model", "product", "garment"), OnScreenText: txt("subtitle")}, "swap_model,swap_product,swap_garment"},
		{"人物 + 字幕", &AnalysisResult{Dims: present("model"), OnScreenText: txt("subtitle")}, "swap_model,clean_text"},
		{"商品 + 服装 + 字幕", &AnalysisResult{Dims: present("product", "garment"), OnScreenText: txt("sticker")}, "swap_product,swap_garment,clean_text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := suggestionsOf(c.r)
			if keysOf(got) != c.want || len(got) > 3 {
				t.Fatalf("got=%s want=%s", keysOf(got), c.want)
			}
		})
	}
	if suggestionsOf(nil) != nil {
		t.Fatal("没有结果不应有建议")
	}
}

func TestSuggestionTextForNonExperts(t *testing.T) {
	r := &AnalysisResult{Dims: map[string]Dim{"model": {Present: true}, "product": {Present: true}, "garment": {Present: true}}, OnScreenText: []OnScreen{{Kind: "watermark"}}}
	r.Dims["garment"] = Dim{}
	all := append(suggestionsOf(r), suggestionsOf(&AnalysisResult{Dims: map[string]Dim{"garment": {Present: true}}, OnScreenText: []OnScreen{{Kind: "subtitle"}}})...)
	needs := map[string]string{SuggestSwapModel: "person_image", SuggestSwapProduct: "product_image", SuggestSwapGarment: "garment_image", SuggestCleanText: ""}
	for _, s := range all {
		if s.Label == "" || s.Desc == "" {
			t.Fatalf("label / desc 不能空：%+v", s)
		}
		for _, bad := range []string{"维度", "档位", "dim", "mode", "replace"} {
			if strings.Contains(s.Label+s.Desc, bad) {
				t.Errorf("%s 的文案不该有术语 %q", s.Key, bad)
			}
		}
		if s.Needs != needs[s.Key] {
			t.Errorf("%s needs=%q 应为 %q", s.Key, s.Needs, needs[s.Key])
		}
	}
}

// 分析成功才有建议；排队 / 失败不给。
func TestAnalysisViewSuggestions(t *testing.T) {
	e := newEnv(t, false)
	a := e.seedAnalysis(12) // 种子分析里人物 / 商品 / 服装都在
	v, err := e.svc.GetAnalysis(e.ctx, uid, a.AnalysisNo)
	if err != nil || v.Status != "succeeded" || keysOf(v.Suggestions) != "swap_model,swap_product,swap_garment" {
		t.Fatalf("成功的分析应带建议：%v %+v", err, v.Suggestions)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"suggestions":[{"key":"swap_model"`) {
		t.Fatalf("JSON 字段名：%s", b)
	}
	for _, x := range e.store.analyses {
		x.Status = 1
	}
	v, _ = e.svc.GetAnalysis(e.ctx, uid, a.AnalysisNo)
	if len(v.Suggestions) != 0 {
		t.Fatalf("没成功不给建议：%+v", v.Suggestions)
	}
	if b, _ := json.Marshal(v); strings.Contains(string(b), "suggestions") {
		t.Fatalf("没有建议时字段应省略：%s", b)
	}
}
