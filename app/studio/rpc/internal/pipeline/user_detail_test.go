package pipeline

import (
	"reflect"
	"strings"
	"testing"

	"aigc-local-life/pkg/reverse"
)

func TestUserCropPlansMapsBySource(t *testing.T) {
	gs := []Image{
		{Key: "g-top-front-1", Item: ItemTop, Source: 1, Angle: "front", Width: 1000, Height: 1000},
		{Key: "g-shoes-unknown-1", Item: ItemShoes, Source: 0, Width: 800, Height: 600},
	}
	plans, warns := UserCropPlans(gs, []UserCrop{
		{GarmentIndex: 1, Label: "上衣·领口", Box: []float64{0.3, 0.1, 0.3, 0.2}},
		{GarmentIndex: 0, Label: "鞋头", Box: []float64{0.1, 0.5, 0.3, 0.3}, Confidence: 0.6},
		{GarmentIndex: 1, Label: "上衣·袖口", Box: []float64{0.0, 0.5, 0.2, 0.2}},
		{GarmentIndex: 5, Label: "上衣·下摆", Box: []float64{0.1, 0.8, 0.5, 0.1}},
	})
	if len(warns) != 1 || !strings.Contains(warns[0], "不存在") {
		t.Fatalf("warns=%v", warns)
	}
	if len(plans) != 2 || plans[0].Index != 0 || plans[1].Index != 1 || len(plans[0].Crops) != 2 {
		t.Fatalf("plans=%+v", plans)
	}
	if plans[0].Crops[0].Confidence != 1 || plans[1].Crops[0].Confidence != 0.6 {
		t.Fatalf("confidence=%+v", plans)
	}
	jobs, _ := BuildCropJobs(gs, plans, []string{ItemTop, ItemShoes})
	if len(jobs) != 2 || jobs[1].Boxes[0].Label != "鞋子·鞋头" || jobs[0].Boxes[1].Label != "上衣·袖口" {
		t.Fatalf("jobs=%+v", jobs)
	}
}

func boolp(b bool) *bool { return &b }

func TestReadWarnings(t *testing.T) {
	cases := []struct {
		name  string
		meta  reverse.AnalyzeMeta
		warn  string
		items []string
		want  []string
	}{
		{"正常", reverse.AnalyzeMeta{Vision: true, GarmentMatch: boolp(true)}, "", []string{"bra"}, nil},
		{"品类不符", reverse.AnalyzeMeta{Vision: true, GarmentMatch: boolp(false), GarmentType: "针织毛衣"},
			"参考图看起来是针织毛衣，与所选「文胸」不一致，请确认品类", []string{"bra"},
			[]string{"参考图看起来是针织毛衣，与所选「文胸」不一致，请确认品类"}},
		{"组合缺件", reverse.AnalyzeMeta{Vision: true, GarmentMatch: boolp(false), GarmentType: "只有上衣"}, "", []string{"top", "bottom"},
			[]string{"参考图里没有找齐所选的「上衣、下装」（识别为只有上衣），请确认品类或补齐服装图"}},
		{"读图降级不透出网关原文", reverse.AnalyzeMeta{Degraded: true}, "读图返回 FAILED/NO_CAPACITY，已改用文字信息生成", []string{"top"},
			[]string{WarnReadDegraded}},
		{"模板兜底", reverse.AnalyzeMeta{Degraded: true}, "读图连接失败（timed out），已改用文字信息生成；文字模型返回 503，已用模板生成", []string{"top"},
			[]string{WarnReadTemplate}},
		{"品类名里的英文代号被去掉", reverse.AnalyzeMeta{GarmentMatch: boolp(false), GarmentType: "\"sweater\" 毛衣 deepseek-v4"}, "", []string{"top"},
			[]string{"参考图看起来是毛衣，与所选「上衣」不一致，请确认品类"}},
		{"品类名为空", reverse.AnalyzeMeta{GarmentMatch: boolp(false)}, "", []string{"panties"},
			[]string{"参考图看起来是其他品类，与所选「内裤」不一致，请确认品类"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReadWarnings(c.meta, c.warn, c.items)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q want %q", got, c.want)
			}
			for _, w := range got {
				for _, bad := range []string{"FAILED", "NO_CAPACITY", "503", "timed out", "deepseek"} {
					if strings.Contains(w, bad) {
						t.Fatalf("泄露上游原文 %q: %q", bad, w)
					}
				}
			}
		})
	}
}

func TestPoseMappingUserWarning(t *testing.T) {
	if got := PoseMappingUserWarning("原图人数与预览不一致（预览 2 人、本次识别 3 人），已按默认配对生成"); got != "人物数量与预览时不一致，已按默认配对生成" {
		t.Fatal(got)
	}
	if got := PoseMappingUserWarning("配对被反推服务校正：提交 [0 0]，实际 [0 1]"); got != "所选人物配对无效，已按默认配对生成" {
		t.Fatal(got)
	}
	if PoseMappingUserWarning("其它") != "" || PoseMappingUserWarning("") != "" {
		t.Fatal("非配对告警应为空")
	}
	if got := DedupeStrings([]string{" a", "b", "a", "", "b "}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
}
