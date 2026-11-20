package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
)

// 生成格式归一：jpg / jpeg / 空 → jpeg，png / webp 原样（大小写、前导点都收），其它拒。
func TestOutputFormatOf(t *testing.T) {
	for in, want := range map[string]string{"": "jpeg", "jpg": "jpeg", "JPEG": "jpeg", ".png": "png", " WebP ": "webp"} {
		if got, ok := outputFormatOf(in); !ok || got != want {
			t.Errorf("outputFormatOf(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := outputFormatOf("gif"); ok {
		t.Fatal("gif 应拒")
	}
}

// 网关请求带 output_format：显式 png 原样下发；改版前落库的 run（空）重新派发按 jpeg。
func TestGatewayRequestsOutputFormat(t *testing.T) {
	c := testCore()
	run := &model.NodeRun{ID: 9, Kind: model.KindImage, CanvasID: "cv_x", UserID: 7, TaskID: sql.NullString{String: "cv_t", Valid: true}}
	m, _ := c.s.Config.FindModel("GPT Image 2.6", "", "image")
	for format, want := range map[string]string{"png": `"output_format":"png"`, "": `"output_format":"jpeg"`} {
		for _, imgs := range [][]refMedia{nil, {{URL: "https://x/a.png"}}} {
			reqs, err := c.gatewayRequests(context.Background(), run, &runSpec{Kind: model.KindImage, Model: m, ModelGroup: m.ModelGroup,
				Prompt: "p", Count: 1, Size: "1K", OutputFormat: format, Images: imgs})
			if err != nil || len(reqs) != 1 {
				t.Fatalf("%q: %v", format, err)
			}
			b, _ := json.Marshal(reqs[0])
			if !strings.Contains(string(b), want) {
				t.Fatalf("%q 应带 %s: %s", format, want, b)
			}
		}
	}
}

// 落库请求：旧 run 没有 output_format，与新请求空格式幂等比对等价；specFromRun 按 jpeg 归一。
func TestStoredRequestOutputFormat(t *testing.T) {
	old := []byte(`{"kind":"image","model":"Banana","model_id":"","prompt":"p","references":[],"ratio":"","size":"2K","quality":"","count":1,"cfg":0}`)
	want, _ := json.Marshal(storedRequest{Kind: "image", Model: "Banana", Prompt: "p", Size: "2K", Count: 1, References: []storedRef{}})
	if !sameRequest(old, want) {
		t.Fatalf("空格式应与旧落库请求等价：%s", want)
	}
	c := testCore()
	run := &model.NodeRun{ID: 9, Kind: model.KindImage, CanvasID: "cv_x", UserID: 7, ModelGroup: "img.nano-banana-2.1",
		Request: json.RawMessage(`{"kind":"image","model":"Banana","prompt":"p","size":"2K","count":1,"output_format":"webp"}`)}
	sp, err := c.specFromRun(context.Background(), run)
	if err != nil || sp.OutputFormat != "webp" {
		t.Fatalf("应还原 webp：%v %v", sp, err)
	}
	run.Request = json.RawMessage(`{"kind":"image","model":"Banana","prompt":"p","size":"2K","count":1}`)
	if sp, err = c.specFromRun(context.Background(), run); err != nil || sp.OutputFormat != "jpeg" {
		t.Fatalf("旧 run 应按 jpeg：%v %v", sp, err)
	}
}

// 智能体执行：节点 outputFormat 生效，args.output_format 临时覆盖，旧节点缺省 jpeg；视频不带。
func TestAgentRunRequestOutputFormat(t *testing.T) {
	node := func(data map[string]any) *Node {
		data["model"] = "Banana"
		return &Node{ID: "m_1", Type: "model", Data: data}
	}
	d := &Doc{}
	cases := []struct {
		data map[string]any
		args map[string]any
		want string
	}{
		{map[string]any{}, map[string]any{}, "jpeg"},
		{map[string]any{"outputFormat": "png"}, map[string]any{}, "png"},
		{map[string]any{"outputFormat": "png"}, map[string]any{"output_format": "webp"}, "webp"},
	}
	for i, tc := range cases {
		req, err := buildAgentRunRequest(d, node(tc.data), nil, tc.args)
		if err != nil || req.OutputFormat != tc.want {
			t.Fatalf("#%d: %v %v want %s", i, req, err, tc.want)
		}
	}
	if _, err := buildAgentRunRequest(d, node(map[string]any{}), nil, map[string]any{"output_format": "gif"}); err == nil {
		t.Fatal("gif 应拒")
	}
}

// 幂等口径：旧 run（无字段）vs 智能体请求（jpeg）、jpg vs jpeg 判同一请求；png vs jpeg 不同。
func TestStoredOutputFormatIdempotency(t *testing.T) {
	stored := func(f string) []byte {
		b, _ := json.Marshal(toStored(&canvas.RunNodeRequest{Kind: "image", Model: "Banana", Prompt: "p", Size: "2K", Count: 1, OutputFormat: f}))
		return b
	}
	old := []byte(`{"kind":"image","model":"Banana","model_id":"","prompt":"p","references":[],"ratio":"","size":"2K","quality":"","count":1,"cfg":0}`)
	if !sameRequest(old, stored("jpeg")) || !sameRequest(old, stored("")) {
		t.Fatalf("旧 run 与 jpeg / 空应判同一请求：%s", stored("jpeg"))
	}
	if !sameRequest(stored("jpg"), stored("jpeg")) || !sameRequest(stored("JPEG"), stored("")) {
		t.Fatal("jpg / JPEG / jpeg / 空 应判同一请求")
	}
	if sameRequest(stored("png"), stored("jpeg")) || sameRequest(old, stored("webp")) {
		t.Fatal("png / webp 与 jpeg 不应判同一请求")
	}
	if !sameRequest(stored("PNG"), stored(".png")) {
		t.Fatal("png 的不同写法应判同一请求")
	}
}
