package logic

import (
	"encoding/json"
	"strings"
	"testing"

	"aigc-life/app/note/rpc/internal/config"
	"aigc-life/app/note/rpc/internal/svc"
)

// sampleScript AIGC 同事提供的 74 真实返回（image_prompt 已截短）。
const sampleScript = `{
 "images": [
   {"image_prompt": "以图2为底版做最小改动编辑……3:4竖版", "index": 1, "reference_materials": [1, 2]}
 ],
 "images_url": [
   "https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/aigc_temp/e9269c01_1.jpg",
   "https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/aigc_temp/ad6f47bb_msg_picture_0ef23631ab8c2913a8a4c08abcfa1249.jpeg",
   "https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/aigc_temp/540e844d_bao.png"
 ],
 "note": {
   "content": "夏天护肤常见的两难：T区泛油，两颊却发紧。\n\n入手前建议做几项核查。",
   "tags": ["#夏日护肤", "#控油不拔干", "#清爽面霜", "#时尚达人"],
   "title": "夏日清爽护肤🧴控油不拔干面霜怎么选"
 }
}`

func testSvc() *svc.ServiceContext {
	var c config.Config
	c.Generate.Model, c.Generate.Size = "mini", "3:4"
	return &svc.ServiceContext{Config: c}
}

func TestParseSampleScript(t *testing.T) {
	s, err := parseScript([]byte(sampleScript))
	if err != nil {
		t.Fatal(err)
	}
	if s.Note.Title != "夏日清爽护肤🧴控油不拔干面霜怎么选" || len(s.Note.Tags) != 4 || len(s.ImagesURL) != 3 {
		t.Fatalf("parsed %+v", s)
	}
	task := imagesTask(testSvc(), s, 3) // 只构思了 1 张，套图数量按实际截断
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(task.Images) != 1 || len(task.ImageURLs) != 3 || task.Images[0].ReferenceMaterials[1] != 2 || task.Size != "3:4" {
		t.Fatalf("task %+v", task)
	}
}

func TestImagesTaskOrderAndCount(t *testing.T) {
	raw := `{"note":{"title":"t"},"images_url":[],
	  "images":[{"index":3,"image_prompt":"c"},{"index":1,"image_prompt":"a"},{"index":2,"image_prompt":"b"}]}`
	s, err := parseScript([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	task := imagesTask(testSvc(), s, 2)
	if len(task.Images) != 2 || task.Images[0].ImagePrompt != "a" || task.Images[1].ImagePrompt != "b" {
		t.Fatalf("task %+v", task.Images)
	}
	// 缺 reference_materials 时传空数组（纯文生图），不能是 null
	b, _ := json.Marshal(task)
	if want := `"reference_materials":[]`; !strings.Contains(string(b), want) {
		t.Fatalf("json %s", b)
	}
}

func TestParseScriptInvalid(t *testing.T) {
	for _, raw := range []string{`{}`, `{"note":{"title":"t"}}`, `not json`} {
		if _, err := parseScript([]byte(raw)); err == nil {
			t.Errorf("want error for %s", raw)
		}
	}
}

func TestNormalizeTags(t *testing.T) {
	got := normalizeTags([]string{"#夏日护肤", "夏日护肤", " ＃控油 ", "", "#a", "#b"}, 3)
	if len(got) != 3 || got[0] != "#夏日护肤" || got[1] != "#控油" || got[2] != "#a" {
		t.Fatalf("%v", got)
	}
}
