package agenttool

import (
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/app/replicate/core"
)

// CONTRACT §B 的 17 个工具 + §J2 定妆图 1 个，一个都不能少、不能多。
var contractTools = []string{
	"replicate_limits", "replicate_analyze", "replicate_get_analysis", "replicate_list_models", "replicate_list_samples",
	"replicate_try_sample", "replicate_create_plan", "replicate_update_plan", "replicate_get_plan", "replicate_quote",
	"replicate_confirm", "replicate_get_job", "replicate_list_jobs", "replicate_continue", "replicate_redo_segment",
	"replicate_stop", "replicate_cancel_rest", "replicate_preview_frame",
	"replicate_quote_batch", "replicate_confirm_batch", "replicate_get_batch", // CONTRACT §M
	"replicate_publish_copy", "replicate_export_ratio", // CONTRACT §N
	"replicate_list_voices",                               // CONTRACT §P
	"replicate_retry_audio",                               // CONTRACT §Q
	"replicate_trending",                                  // CONTRACT §S
	"replicate_share_sample", "replicate_withdraw_sample", // CONTRACT §T
}

func TestRegistryMatchesContract(t *testing.T) {
	got := map[string]bool{}
	for _, tl := range Tools() {
		if got[tl.Name] {
			t.Fatalf("工具重名：%s", tl.Name)
		}
		got[tl.Name] = true
	}
	for _, n := range contractTools {
		if !got[n] {
			t.Errorf("缺少契约工具 %s", n)
		}
		delete(got, n)
	}
	for n := range got {
		t.Errorf("契约之外的工具 %s（改契约 CONTRACT.md §B 再加）", n)
	}
}

func TestRegistryWellFormed(t *testing.T) {
	for _, tl := range Tools() {
		if tl.Title == "" || len([]rune(tl.Description)) < 30 {
			t.Errorf("%s 缺标题 / 说明太短", tl.Name)
		}
		if tl.Input["type"] != "object" || tl.Input["additionalProperties"] != false {
			t.Errorf("%s 入参必须是封闭对象", tl.Name)
		}
		if tl.Output["type"] != "object" {
			t.Errorf("%s 出参必须是对象", tl.Name)
		}
		checkRequired(t, tl.Name, tl.Input)
		if _, err := json.Marshal(tl.Input); err != nil {
			t.Errorf("%s 入参 Schema 不能序列化：%v", tl.Name, err)
		}
		if tl.Charges && tl.ReadOnly {
			t.Errorf("%s 扣点工具不能是只读", tl.Name)
		}
	}
}

func checkRequired(t *testing.T, name string, s Schema) {
	t.Helper()
	props, _ := s["properties"].(Props)
	if req, ok := s["required"].([]any); ok {
		for _, k := range req {
			if _, has := props[k.(string)]; !has {
				t.Errorf("%s: required %q 不在 properties 里", name, k)
			}
		}
	}
	for k, p := range props {
		if ps, ok := p.(Schema); ok {
			if ps["type"] == "object" {
				checkRequired(t, name+"."+k, ps)
			}
			if items, ok := ps["items"].(Schema); ok && items["type"] == "object" {
				checkRequired(t, name+"."+k+"[]", items)
			}
		}
	}
}

func TestDimKeysMatchCore(t *testing.T) {
	if strings.Join(DimKeys, ",") != strings.Join(core.DimKeys, ",") {
		t.Fatalf("维度键与 core.DimKeys 不一致：%v vs %v", DimKeys, core.DimKeys)
	}
}

// 扣点工具恰好是 confirm / continue / redo_segment / confirm_batch；写 / 只读标注正确。
func TestChargeAndReadOnlyFlags(t *testing.T) {
	charges := map[string]bool{"replicate_confirm": true, "replicate_continue": true, "replicate_redo_segment": true, "replicate_confirm_batch": true}
	readOnly := map[string]bool{"replicate_limits": true, "replicate_get_analysis": true, "replicate_list_models": true, "replicate_list_samples": true,
		"replicate_try_sample": true, "replicate_get_plan": true, "replicate_get_job": true, "replicate_list_jobs": true,
		"replicate_quote_batch": true, "replicate_get_batch": true, "replicate_list_voices": true, "replicate_trending": true}
	for _, tl := range Tools() {
		if tl.Charges != charges[tl.Name] {
			t.Errorf("%s Charges=%v", tl.Name, tl.Charges)
		}
		if tl.ReadOnly != readOnly[tl.Name] {
			t.Errorf("%s ReadOnly=%v", tl.Name, tl.ReadOnly)
		}
		if tl.Charges {
			if _, ok := tl.Input["properties"].(Props)["expected_points"]; !ok {
				t.Errorf("%s 必须声明 expected_points", tl.Name)
			}
		}
	}
}

// 说明里必须交代规划模型要守的规则（CONTRACT / AGENT §16）。
func TestDescriptionsStateRules(t *testing.T) {
	must := map[string][]string{
		"replicate_confirm":         {"扣", "plan_hash", "expected_points", "绝不", "QUOTE_CHANGED", "replicate_redo_segment", "不要轮询"},
		"replicate_continue":        {"扣", "plan_hash", "expected_points", "绝不", "QUOTE_CHANGED"},
		"replicate_redo_segment":    {"扣", "expected_points", "绝不", "replicate_confirm", "失败", "note", "200"},
		"replicate_get_analysis":    {"绝不要", "轮询"},
		"replicate_analyze":         {"video", "source", "系统", "不要轮询", "cached"},
		"replicate_create_plan":     {"报价", "不需要再调 replicate_quote", "gaps", "ref_images_from", "refs"},
		"replicate_update_plan":     {"新方案", "STALE", "合并", "ref_images_from"},
		"replicate_quote":           {"不需要再调"},
		"replicate_stop":            {"退"},
		"replicate_cancel_rest":     {"退", "幂等"},
		"replicate_get_job":         {"decision", "money", "cancelRest", "不要轮询"},
		"replicate_trending":        {"industry", "不含任何人", "不要向用户编造", "20"},
		"replicate_share_sample":    {"授权", "portrait_consent", "PORTRAIT_CONSENT_REQUIRED", "完整版", "不扣点"},
		"replicate_withdraw_sample": {"下架", "submission_id", "不扣点"},
	}
	for name, subs := range must {
		tl, ok := Find(name)
		if !ok {
			t.Fatalf("没有 %s", name)
		}
		for _, s := range subs {
			if !strings.Contains(tl.Description, s) {
				t.Errorf("%s 的说明缺少 %q", name, s)
			}
		}
	}
	// 系统注入字段要明说「由系统填写」
	for _, c := range []struct{ tool, field string }{
		{"replicate_analyze", "source"}, {"replicate_create_plan", "ref_images"}, {"replicate_update_plan", "ref_images"},
		{"replicate_confirm", "plan_hash"}, {"replicate_confirm", "expected_points"}, {"replicate_continue", "plan_hash"},
		{"replicate_continue", "expected_points"}, {"replicate_redo_segment", "expected_points"},
	} {
		tl, _ := Find(c.tool)
		d, _ := tl.Input["properties"].(Props)[c.field].(Schema)["description"].(string)
		if !strings.Contains(d, "由系统填写") {
			t.Errorf("%s.%s 的字段说明应写明由系统填写：%q", c.tool, c.field, d)
		}
	}
}

// 定妆图工具：免费、不是扣点 / 破坏性工具，说明里写明免费、每天 3 次、静态预览、确认前调用。
func TestPreviewFrameTool(t *testing.T) {
	tl, ok := Find(ToolPreviewFrame)
	if !ok {
		t.Fatal("没有定妆图工具")
	}
	if tl.Charges || tl.Destructive || tl.ReadOnly {
		t.Fatalf("定妆图不扣点、不破坏，但不是只读（出图写库）：%+v", tl)
	}
	for _, sub := range []string{"免费", "3 次", "不是最终视频", "确认", "PREVIEW_QUOTA_EXHAUSTED"} {
		if !strings.Contains(tl.Description, sub) {
			t.Errorf("说明缺少 %q", sub)
		}
	}
	if _, err := ParseArguments(tl, []byte(`{"plan_no":"VRP1"}`)); err != nil {
		t.Fatalf("合法入参被拒：%v", err)
	}
	if _, err := ParseArguments(tl, []byte(`{}`)); err == nil {
		t.Fatal("缺 plan_no 应拒")
	}
}

func TestRenderFormats(t *testing.T) {
	for _, f := range []Format{FormatOpenAI, FormatAnthropic, FormatMCP} {
		tools := Render(f)
		if len(tools) != len(contractTools) {
			t.Fatalf("%s 工具数 %d", f, len(tools))
		}
		if _, err := json.Marshal(tools); err != nil {
			t.Fatal(err)
		}
	}
	if fn := Render(FormatOpenAI)[0]["function"].(map[string]any); fn["name"] == "" || fn["parameters"] == nil {
		t.Fatalf("openai 形状：%v", fn)
	}
	if Render(FormatAnthropic)[0]["input_schema"] == nil {
		t.Fatal("anthropic 形状")
	}
	m := Render(FormatMCP)
	for _, x := range m {
		ann := x["annotations"].(map[string]any)
		if x["name"] == "replicate_confirm" && (ann["readOnlyHint"] != false || ann["openWorldHint"] != true) {
			t.Fatalf("confirm 注解：%v", ann)
		}
		if x["name"] == "replicate_get_job" && ann["readOnlyHint"] != true {
			t.Fatalf("get_job 注解：%v", ann)
		}
	}
}

func parse(t *testing.T, tool, argsJSON string) (map[string]any, error) {
	t.Helper()
	tl, ok := Find(tool)
	if !ok {
		t.Fatalf("没有 %s", tool)
	}
	return ParseArguments(tl, json.RawMessage(argsJSON))
}

func TestParseArguments(t *testing.T) {
	ok := []struct{ tool, args string }{
		{"replicate_limits", `{}`},
		{"replicate_analyze", `{"video":"视频1"}`},
		{"replicate_analyze", `{"source":"https://x/video/7/20260101/a.mp4"}`}, // Java 注入后模型的 video 被去掉
		{"replicate_analyze", `"{\"video\":\"视频1\"}"`},                         // OpenAI 的字符串化 arguments
		{"replicate_create_plan", `{"analysis_no":"VRA1","annotations":{"dims":{"product":{"mode":"replace","refs":[1]},"scene":{"mode":"describe","note":"改成海边"}},"audio":"keep","texts":[{"text":"99元","kind":"price"}]},"ref_images_from":["图1"],"ref_images":["mr://r1"],"resolution":"720p","aspect_ratio":"9:16"}`},
		{"replicate_create_plan", `{"sample_id":3}`},
		{"replicate_update_plan", `{"plan_no":"VRP1","model_group":"vid.x"}`},
		{"replicate_update_plan", `{"planNo":"VRP1","ref_images_from":[]}`}, // 驼峰归一
		{"replicate_confirm", `{"plan_no":"VRP1","stage":"first","plan_hash":"h","expected_points":120}`},
		{"replicate_confirm", `{"plan_no":"VRP1"}`}, // expected_points 的必填由入口校验（Java 注入前模型不写）
		{"replicate_redo_segment", `{"job_id":42,"seg_index":2,"note":"动作再大一点"}`},
		{"replicate_list_jobs", `{"page":2}`},
	}
	for _, c := range ok {
		if _, err := parse(t, c.tool, c.args); err != nil {
			t.Errorf("%s %s 应通过：%v", c.tool, c.args, err)
		}
	}
	bad := []struct{ tool, args, sub string }{
		{"replicate_get_analysis", `{}`, "analysis_no"},
		{"replicate_confirm", `{}`, "plan_no"},
		{"replicate_confirm", `{"plan_no":"VRP1","stage":"half"}`, "first / all"},
		{"replicate_confirm", `{"plan_no":"VRP1","expected_points":0}`, "不能小于"},
		{"replicate_confirm", `{"plan_no":"VRP1","foo":1}`, "不认识的字段"},
		{"replicate_redo_segment", `{"job_id":42}`, "seg_index"},
		{"replicate_redo_segment", `{"job_id":42,"seg_index":1,"note":"` + strings.Repeat("长", 201) + `"}`, "最多 200"},
		{"replicate_create_plan", `{"analysis_no":"VRA1","annotations":{"dims":{"product":{"mode":"swap"}}}}`, "之一"},
		{"replicate_create_plan", `{"analysis_no":"VRA1","annotations":{"dims":{"weather":{"mode":"keep"}}}}`, "不认识的字段"},
		{"replicate_create_plan", `{"analysis_no":"VRA1","resolution":"4k"}`, "之一"},
		{"replicate_create_plan", `{"analysis_no":"VRA1","annotations":{"texts":[{"text":"` + strings.Repeat("字", 31) + `"}]}}`, "最多 30"},
		{"replicate_create_plan", `{"analysis_no":"VRA1","ref_images_from":["图1","图2","图3","图4","图5","图6","图7","图8","图9","图10"]}`, "最多 9"},
		{"replicate_get_job", `{"job_id":0}`, "不能小于"},
		{"replicate_get_job", `[]`, "JSON 对象"},
	}
	for _, c := range bad {
		_, err := parse(t, c.tool, c.args)
		if err == nil || !strings.Contains(err.Error(), c.sub) {
			t.Errorf("%s %.60s 应报含 %q：%v", c.tool, c.args, c.sub, err)
		}
	}
}

func TestParseCallShapes(t *testing.T) {
	for name, body := range map[string]string{
		"契约形状 tool":             `{"call_id":"c1","tool":"replicate_limits","arguments":{}}`,
		"画布形状 name":             `{"call_id":"c1","name":"replicate_limits","arguments":{}}`,
		"tool 与 name 都给时以 tool": `{"call_id":"c1","tool":"replicate_limits","name":"other","arguments":{}}`,
		"OpenAI tool_call":      `{"id":"c1","type":"function","function":{"name":"replicate_limits","arguments":"{}"}}`,
	} {
		c, err := ParseCall([]byte(body))
		if err != nil || c.Tool != "replicate_limits" || c.CallID != "c1" {
			t.Errorf("%s：%+v %v", name, c, err)
		}
	}
	if _, err := ParseCall([]byte(`{"foo":1}`)); err == nil {
		t.Error("没有工具名应报错")
	}
	if _, err := ParseCall([]byte(`[]`)); err == nil {
		t.Error("不是对象应报错")
	}
}

// CONTRACT §O1：dims.<维度>.occurrences 逐套换装进了入参 schema（schema 是封闭的，不加就会被拒）；index / refs 有边界。
func TestDimEditOccurrencesSchema(t *testing.T) {
	tl, _ := Find("replicate_create_plan")
	ok := `{"analysis_no":"VRA1","ref_images_from":["图1","图2"],"annotations":{"dims":{"garment":{"mode":"replace","occurrences":[{"index":1,"refs":[1]},{"index":3,"refs":[2]}]}}}}`
	if _, err := ParseArguments(tl, []byte(ok)); err != nil {
		t.Fatalf("逐套换装应通过：%v", err)
	}
	for name, bad := range map[string]string{
		"index 0": `"occurrences":[{"index":0,"refs":[1]}]`,
		"缺 refs":  `"occurrences":[{"index":1}]`,
		"refs 为空": `"occurrences":[{"index":1,"refs":[]}]`,
		"多余字段":    `"occurrences":[{"index":1,"refs":[1],"x":1}]`,
	} {
		in := `{"analysis_no":"VRA1","ref_images_from":["图1"],"annotations":{"dims":{"garment":{"mode":"replace",` + bad + `}}}}`
		if _, err := ParseArguments(tl, []byte(in)); err == nil {
			t.Errorf("%s：应被拒", name)
		}
	}
}
