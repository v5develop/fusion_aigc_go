package pipeline

import (
	"errors"
	"strings"
	"testing"

	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/reverse"
)

func TestValidNormBox(t *testing.T) {
	ok := [][]float64{{0, 0, 1, 1}, {0.1, 0.2, 0.3, 0.4}, {0.3334, 0, 0.6667, 1}}
	bad := [][]float64{{-0.1, 0, 0.5, 0.5}, {0, 0, 0, 0.5}, {0.6, 0, 0.5, 0.5}, {0, 0.7, 0.2, 0.4}, {1.1, 0, 0.1, 0.1}, {0, 0, 0.5}, nil}
	for _, b := range ok {
		if !ValidNormBox(b) {
			t.Errorf("want valid %v", b)
		}
	}
	for _, b := range bad {
		if ValidNormBox(b) {
			t.Errorf("want invalid %v", b)
		}
	}
}

func TestNormalizeCropLabel(t *testing.T) {
	items := []string{"top", "bottom"}
	cases := []struct {
		label, item, want string
		ok                bool
	}{
		{"领口", "top", "上衣·领口", true},
		{"上衣·领口", "top", "上衣·领口", true},
		{"上装·领口", "top", "上衣·领口", true},
		{"下装·腰头", "top", "", false}, // 串件
		{"下装·腰头", ItemLook, "下装·腰头", true},
		{"鞋子·鞋面", ItemLook, "", false}, // 不在 items
		{"面料纹理", ItemLook, "整套·面料纹理", true},
		{"文胸·杯型", "bra", "文胸·杯型", true},
		{"", "top", "", false},
		{"上衣·", "top", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeCropLabel(c.label, c.item, items)
		if got != c.want || ok != c.ok {
			t.Errorf("%q/%s = %q,%v want %q,%v", c.label, c.item, got, ok, c.want, c.ok)
		}
	}
}

func TestBuildCropJobs(t *testing.T) {
	srcs := []Image{
		{Key: "g-top-front-1", Item: "top", Angle: "front", Width: 1000, Height: 2000},
		{Key: "g-bottom-front-1", Item: "bottom", Angle: "front", Width: 800, Height: 800},
		{Key: "g-bottom-back-1", Item: "bottom", Angle: "back"}, // 没尺寸
	}
	plans := []reverse.CropPlan{
		{Index: 0, Crops: []reverse.Crop{
			{Label: "领口", Box: []float64{0.1, 0.1, 0.2, 0.1}, Confidence: 0.86},
			{Label: "上衣·领口", Box: []float64{0.1, 0.1, 0.2, 0.1}}, // 规范化后重复
			{Label: "下装·腰头", Box: []float64{0.1, 0.5, 0.2, 0.1}}, // 串件
			{Label: "袖口", Box: []float64{0.9, 0.1, 0.2, 0.1}},    // x+w>1
		}},
		{Index: 1, Crops: []reverse.Crop{{Label: "下装·腰头", Box: []float64{0.25, 0.2, 0.5, 0.14}, Confidence: 0.4}}},
		{Index: 2, Crops: []reverse.Crop{{Label: "后袋", Box: []float64{0.1, 0.1, 0.2, 0.2}}}},
		{Index: 9},
	}
	jobs, warns := BuildCropJobs(srcs, plans, []string{"top", "bottom"})
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	j0 := jobs[0]
	if len(j0.Boxes) != 1 || j0.Boxes[0] != (mediasvc.PixelBox{X: 100, Y: 200, W: 200, H: 200, Label: "上衣·领口"}) || j0.Confidence[0] != 0.86 {
		t.Fatalf("job0 = %+v", j0)
	}
	if jobs[1].Boxes[0] != (mediasvc.PixelBox{X: 200, Y: 160, W: 400, H: 112, Label: "下装·腰头"}) {
		t.Fatalf("job1 = %+v", jobs[1])
	}
	if len(warns) != 4 { // 串件、越界框、缺尺寸、下标越界
		t.Fatalf("warns = %v", warns)
	}
	res := []mediasvc.Resource{{RID: "r1", Width: 256, Height: 256}}
	d := DetailImages(j0, res)
	if len(d) != 1 || d[0].Key != "g-top-front-1-c1" || d[0].Kind != ImageDetail || d[0].Role != RoleDetail || d[0].Item != "top" || d[0].RID != "r1" {
		t.Fatalf("details = %+v", d)
	}
	if CropParams.Pad != 0.04 || CropParams.MinShortEdge != 256 || CropParams.Format != "jpeg" {
		t.Fatal("crop params")
	}
}

func TestFilterCropChecks(t *testing.T) {
	details := []Image{{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}}
	res := []reverse.CropCheckResult{
		{Index: 0, OK: false, Confidence: 0.85}, // 删
		{Index: 1, OK: false, Confidence: 0.84}, // 留
		{Index: 2, OK: true, Confidence: 0.99},  // 留
		{Index: 7, OK: false, Confidence: 1},    // 越界忽略
	}
	got, warns := FilterCropChecks(details, res, nil)
	if strings.Join(keys(got), ",") != "b,c,d" || len(warns) != 1 {
		t.Fatalf("got %v warns %v", keys(got), warns)
	}
	got, warns = FilterCropChecks(details, nil, errors.New("502"))
	if len(got) != 4 || len(warns) != 1 {
		t.Fatalf("failure must not block: %v %v", keys(got), warns)
	}
}
