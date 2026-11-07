package studio

import (
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
)

// 预览响应：数组一律 []（前端不判 null），失败的姿势图 pose 为 null、error 有值。
func TestToPosePreviewResp(t *testing.T) {
	out := toPosePreviewResp(&studio_client.PreviewPoseResp{
		MappingEnabled: true,
		Source: &studio_client.PosePreviewImage{PersonCount: 2, Interaction: "无", People: []*studio_client.PosePreviewPerson{
			{Label: "P1", Position: "画面左侧", GenderAge: "成年女性", Summary: "灰色西装"}, {Label: "P2", Position: "画面右侧", Primary: true},
		}},
		Poses: []*studio_client.PosePreviewItem{
			{PoseIndex: 0, Pose: &studio_client.PosePreviewImage{PersonCount: 2}, DefaultMapping: []int32{0, 1}},
			{PoseIndex: 1, Error: "姿势参考图中没有识别到人物，请换一张"},
		},
	})
	raw, _ := json.Marshal(out)
	s := string(raw)
	for _, want := range []string{`"mapping_enabled":true`, `"person_count":2`, `"default_mapping":[0,1]`, `"pose":null`,
		`"default_mapping":[]`, `"people":[]`, `"label":"P2"`, `"primary":true`} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺 %s：%s", want, s)
		}
	}
	empty, _ := json.Marshal(toPosePreviewResp(&studio_client.PreviewPoseResp{}))
	if !strings.Contains(string(empty), `"poses":[]`) || !strings.Contains(string(empty), `"people":[]`) {
		t.Fatalf("空响应也要是 []：%s", empty)
	}
}

func TestPoseMappings(t *testing.T) {
	if poseMappings(nil) != nil {
		t.Fatal("不传配对应为 nil")
	}
	got := poseMappings([]types.StudioPoseMapping{{PoseIndex: 2, Mapping: []int32{1, 0}, PosePersonCount: 2}})
	if len(got) != 1 || got[0].PoseIndex != 2 || got[0].Mapping[0] != 1 || got[0].PosePersonCount != 2 {
		t.Fatalf("got = %+v", got)
	}
}
