package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/model"
)

// 应用侧不区分 Dev 档：旧客户端 / 已保存草稿里的旧名（GPT25Dev、*Sunburst Developer、Nano Banana 2.1 Dev、旧 Dev 组码）
// 都解析到规范组；新名（GPT25 / gpt-image-2.5-sunburst / 规范组码）同样解析。
func TestFindModelLegacyAndCanonicalAliases(t *testing.T) {
	c := testCore()
	for in, want := range map[string]string{
		"GPT25Dev": "img.gpt-image-2.5-sunburst", "gpt25dev": "img.gpt-image-2.5-sunburst",
		"GPT Image 2.5 Sunburst Developer": "img.gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst-developer": "img.gpt-image-2.5-sunburst",
		"img.gpt-image-2.5-sunburst-developer": "img.gpt-image-2.5-sunburst",
		"GPT25":                                "img.gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst": "img.gpt-image-2.5-sunburst", "img.gpt-image-2.5-sunburst": "img.gpt-image-2.5-sunburst",
		"Nano Banana 2.1 Dev": "img.nano-banana-2.1", "nano-banana-2.1-dev": "img.nano-banana-2.1", "img.nano-banana-2.1-dev": "img.nano-banana-2.1",
		"Nano Banana 2.1": "img.nano-banana-2.1", "img.nano-banana-2.1": "img.nano-banana-2.1",
	} {
		m, ok := c.s.Config.FindModel(in, "", "image")
		if !ok || m.ModelGroup != want {
			t.Errorf("FindModel(%q) → %q,%v want %q", in, m.ModelGroup, ok, want)
		}
	}
}

// 改版前落库、带了 quality 的 run 重新派发到 GPT Image 2.5 / Nano Banana 2.1（旧 Dev 组码或规范组）：一律不发 quality。
func TestGatewayRequestsStripQualityForPerImageGroups(t *testing.T) {
	c := testCore()
	run := &model.NodeRun{ID: 9, Kind: model.KindImage, CanvasID: "cv_x", UserID: 7, TaskID: sql.NullString{String: "cv_t", Valid: true}}
	g26, _ := c.s.Config.FindModel("GPT Image 2.6", "", "image") // 借一个带 Qualities 的档，模拟旧配置
	for _, group := range []string{"img.gpt-image-2.5-sunburst", "img.gpt-image-2.5-sunburst-developer", "img.nano-banana-2.1", "img.nano-banana-2.1-dev"} {
		reqs, err := c.gatewayRequests(context.Background(), run, &runSpec{Kind: model.KindImage, Model: g26, ModelGroup: group, Prompt: "p",
			Count: 1, Size: "1K", Quality: "high", Images: []refMedia{{URL: "https://x/a.png"}}})
		if err != nil || len(reqs) != 1 {
			t.Fatalf("%s: %v", group, err)
		}
		b, _ := json.Marshal(reqs[0])
		if strings.Contains(string(b), "quality") || strings.Contains(string(b), "mask") {
			t.Fatalf("%s 不应带 quality / mask: %s", group, b)
		}
	}
}

// 轮询器重提：受理时冻结的旧 Dev 组码与现配置的规范组等价 → 沿用冻结值（同一幂等键的请求体不变）。
func TestSpecFromRunKeepsEquivalentFrozenGroup(t *testing.T) {
	c := testCore()
	run := &model.NodeRun{ID: 9, Kind: model.KindImage, CanvasID: "cv_x", UserID: 7, ModelGroup: "img.nano-banana-2.1-dev",
		Request: json.RawMessage(`{"kind":"image","model":"Banana","prompt":"p","size":"2K","count":1}`)}
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if sp.ModelGroup != "img.nano-banana-2.1-dev" {
		t.Fatalf("应沿用冻结组：%s", sp.ModelGroup)
	}
	run.ModelGroup = "img.seedream-5.5" // 不等价：按现配置
	if sp, err = c.specFromRun(context.Background(), run); err != nil || sp.ModelGroup != "img.nano-banana-2.1" {
		t.Fatalf("不等价应按现配置：%v %v", sp, err)
	}
}
