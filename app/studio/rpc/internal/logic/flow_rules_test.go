package logic

import (
	"database/sql"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/pkg/aigateway"
)

func TestBatchFinalStatus(t *testing.T) {
	cases := []struct {
		name   string
		counts map[int8]int
		want   int8
		final  bool
	}{
		{"还有排队", map[int8]int{model.TaskQueued: 1, model.TaskSucceeded: 3}, 0, false},
		{"还有生成中", map[int8]int{model.TaskRunning: 1}, 0, false},
		{"空批次", map[int8]int{}, 0, false},
		{"全部成功", map[int8]int{model.TaskSucceeded: 4}, model.BatchDone, true},
		{"全部失败", map[int8]int{model.TaskFailed: 4}, model.BatchFailed, true},
		{"成功+失败", map[int8]int{model.TaskSucceeded: 1, model.TaskFailed: 3}, model.BatchPartial, true},
		{"成功+取消", map[int8]int{model.TaskSucceeded: 1, model.TaskCanceled: 1}, model.BatchPartial, true},
		{"全部取消", map[int8]int{model.TaskCanceled: 2}, model.BatchCanceled, true},
		{"失败+取消（用户取消过）", map[int8]int{model.TaskFailed: 1, model.TaskCanceled: 1}, model.BatchCanceled, true},
	}
	for _, c := range cases {
		got, ok := batchFinalStatus(c.counts)
		if ok != c.final || (ok && got != c.want) {
			t.Errorf("%s: got (%d,%v) want (%d,%v)", c.name, got, ok, c.want, c.final)
		}
	}
}

func task(kind model.Kind, m, s, p int32, billing string) *model.Task {
	return &model.Task{Kind: kind, ModelIndex: m, ShotIndex: s, PoseIndex: p,
		BillingRequestID: sql.NullString{String: billing, Valid: billing != ""}}
}

func TestIdemKey(t *testing.T) {
	cases := []struct {
		t    *model.Task
		want string
	}{
		{task(model.KindTryon, 2, 1, 0, "studio-tyo1"), "studio:9:2:1"},
		{task(model.KindPose, 0, 0, 3, "studio-pse1"), "studio:9:3:0"},
		{task(model.KindImageGen, 0, 4, 0, ""), "studio:9:0:4"},
		{task(model.KindTryon, 1, 0, 0, "studio-tyo1-r2"), "studio:9:1:0:r2"}, // 后台重做换键
	}
	for _, c := range cases {
		if got := idemKey(9, c.t); got != c.want {
			t.Errorf("idemKey = %q want %q", got, c.want)
		}
	}
	// 必须满足 gateway 幂等键格式
	if !regexpMatch(`^[A-Za-z0-9._:/#@-]{1,255}$`, idemKey(123456789, task(model.KindTryon, 8, 8, 0, "studio-x-r12"))) {
		t.Fatal("幂等键不符合 gateway 格式")
	}
	if retryRound("studio-tyo1-r3") != 3 || retryRound("studio-tyo1") != 0 || retryBillingRequestID("tyo1", 3) != "studio-tyo1-r3" {
		t.Fatal("重做轮次解析不对")
	}
}

func failed(class string, retrySafe bool, runNo int) *aigateway.Call {
	return &aigateway.Call{Status: aigateway.StatusFailed, RetrySafe: retrySafe, RunNo: runNo, Failure: &aigateway.Failure{Class: class}}
}

func TestDecideTerminal(t *testing.T) {
	if a, _ := decideTerminal(&aigateway.Call{Status: aigateway.StatusSucceeded}); a != actDeliver {
		t.Fatal("成功应交付")
	}
	// UNKNOWN：标失败但不退款，不重试
	if a, _ := decideTerminal(&aigateway.Call{Status: aigateway.StatusUnknown, MoneyAtRisk: true}); a != actUnknown {
		t.Fatal("UNKNOWN 应走待对账")
	}
	if a, _ := decideTerminal(&aigateway.Call{Status: aigateway.StatusCancelled, RetrySafe: true}); a != actFail {
		t.Fatal("CANCELLED 应失败退款")
	}
	// 可重试：429/5xx/超时类 + retry_safe，按 run_no 退避 1.2/2.4/4.8s，第 4 轮起不再重试
	wants := []time.Duration{1200 * time.Millisecond, 2400 * time.Millisecond, 4800 * time.Millisecond}
	for i, w := range wants {
		a, d := decideTerminal(failed("VENDOR_FAILED", true, i+1))
		if a != actRetry || d != w {
			t.Fatalf("run_no=%d: got (%d,%v)", i+1, a, d)
		}
	}
	if a, _ := decideTerminal(failed("VENDOR_FAILED", true, 4)); a != actFail {
		t.Fatal("重试 3 次后应失败")
	}
	if a, _ := decideTerminal(failed("VENDOR_FAILED", false, 1)); a != actFail {
		t.Fatal("retry_safe=false 不能重试")
	}
	if a, _ := decideTerminal(failed("CONTENT_POLICY", true, 1)); a != actFail {
		t.Fatal("内容审核不重试")
	}
	if a, _ := decideTerminal(failed("DEADLINE_EXCEEDED", true, 2)); a != actRetry {
		t.Fatal("超时应重试")
	}
}

func TestFailureReasonAndCost(t *testing.T) {
	if failureReason(failed("CONTENT_POLICY", true, 1)) != "内容未通过审核，点数已退回" {
		t.Fatal("内容审核文案")
	}
	if failureReason(&aigateway.Call{Status: aigateway.StatusCancelled}) != reasonGatewayCancel {
		t.Fatal("取消文案")
	}
	if c, ok := costCNYText(&aigateway.Cost{Amount: "0.04", Currency: "USD", AmountCNY: "0.29"}); c != "0.29" || !ok {
		t.Fatal("应取 amount_cny")
	}
	if c, ok := costCNYText(&aigateway.Cost{Amount: "0.10", Currency: "CNY"}); c != "0.10" || !ok {
		t.Fatal("CNY 且 amount_cny 为空时退用 amount")
	}
	if c, ok := costCNYText(&aigateway.Cost{Amount: "0.04", Currency: "USD"}); c != "0" || ok {
		t.Fatal("USD 无 amount_cny 应记 0 并告警")
	}
	if c, ok := costCNYText(nil); c != "0" || ok {
		t.Fatal("无成本应记 0 并告警")
	}
}

func TestStepLogRetries(t *testing.T) {
	logs := []model.StepLog{{Step: "submitted"}, {Step: stepRetry}, {Step: "cropping"}, {Step: stepRetry}}
	if stepLogRetries(logs) != 2 {
		t.Fatal("retry 计数不对")
	}
	if len(parseStepLogs([]byte("not json"))) != 0 {
		t.Fatal("坏 step_log 应当空")
	}
}

func TestParseRewrite(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"prompt\": \"一只猫\"}\n```": "一只猫",
		"{\"prompt\":\" 狗 \"}":                "狗",
		"直接返回的文本":                             "直接返回的文本",
		"{\"bad\":":                           "",
	}
	for in, want := range cases {
		if got := parseRewrite(in); got != want {
			t.Errorf("parseRewrite(%q) = %q want %q", in, got, want)
		}
	}
}

// 重建请求必须与编排时一致：prompt_text 已含图序，再过一遍 BuildImageEditRequest 不应叠第二行。
func TestBuildTaskRequestStable(t *testing.T) {
	b := &model.Batch{ID: 7, Payload: []byte(`{"resolution":"1k","aspect_ratio":"3:4"}`)}
	req, _, err := pipeline.BuildImageEditRequest(pipeline.KindPose, "img.seedream-5-pro", "正文", []string{"mr://a", "mr://b"}, "1k", "3:4", nil)
	if err != nil {
		t.Fatal(err)
	}
	full, _ := promptOf(req.Input)
	tk := task(model.KindPose, 0, 0, 1, "studio-pse1")
	tk.ModelGroup, tk.PromptText = "img.seedream-5-pro", sql.NullString{String: full, Valid: true}
	tk.InputImages = []byte(`[{"url":"mr://a"},{"url":"mr://b"}]`)
	again, err := buildTaskRequest(tk, b)
	if err != nil {
		t.Fatal(err)
	}
	if p, n := promptOf(again.Input); p != full || n != 2 {
		t.Fatalf("重建后 prompt 变了：%q", p)
	}
	if again.Labels["batch"] != "7" || again.Labels["kind"] != "pose" {
		t.Fatalf("labels 不对：%v", again.Labels)
	}
	// 纯文生图不加图序
	tk2 := task(model.KindImageGen, 0, 0, 0, "")
	tk2.ModelGroup, tk2.PromptText = "img.gpt-image-2", sql.NullString{String: "一只猫", Valid: true}
	r2, err := buildTaskRequest(tk2, b)
	if err != nil || r2.Operation != pipeline.OpImageGenerate {
		t.Fatalf("文生图应走 image.generate：%v %v", r2.Operation, err)
	}
}

func regexpMatch(pat, s string) bool { return regexp.MustCompile(pat).MatchString(s) }

// 编排时定下的生图参数（prompt_meta.gen）优先于 payload；Seedream 4k 两次构造都降 2K，参考图带 sha256。
func TestBuildTaskRequestUsesMeta(t *testing.T) {
	b := &model.Batch{ID: 8, Payload: []byte(`{"resolution":"4k","aspect_ratio":""}`)}
	tk := task(model.KindTryon, 0, 0, 0, "studio-tyo1")
	tk.ModelGroup, tk.PromptText = "img.seedream-5-pro", sql.NullString{String: "图序：图1 是模特。\n正文", Valid: true}
	tk.InputImages = []byte(`[{"url":"mr://m","sha256":"s1"}]`)
	tk.PromptMeta = []byte(`{"gen":{"aspect_ratio":"3:4","resolution":"4k"}}`)
	req, err := buildTaskRequest(tk, b)
	if err != nil {
		t.Fatal(err)
	}
	in := req.Input.(pipeline.ImageEditInput)
	if in.AspectRatio != "3:4" || in.Resolution != "2K" || in.Images[0].Sha256 != "s1" || in.Quality != "" {
		t.Fatalf("input = %+v", in)
	}
	tk.ModelGroup, tk.PromptMeta = "img.gpt-image-2", []byte(`{"gen":{"aspect_ratio":"3:4","resolution":"1k","quality":"high"}}`)
	req, _ = buildTaskRequest(tk, b)
	if q := req.Input.(pipeline.ImageEditInput).Quality; q != "high" {
		t.Fatalf("GPT 商拍应带 quality=high，得到 %q", q)
	}
}

// 编排重试计数只数最近一次后台重做之后的；交付中的子任务不算终态。
func TestStepLogRetriesAfterAdminRetry(t *testing.T) {
	logs := []model.StepLog{{Step: stepRetry}, {Step: stepRetry}, {Step: stepRetry}, {Step: stepAdminRetry}, {Step: stepRetry}}
	if n := stepLogRetries(logs); n != 1 {
		t.Fatalf("got %d want 1", n)
	}
	if _, ok := batchFinalStatus(map[int8]int{model.TaskSucceeded: 1, model.TaskDelivering: 1}); ok {
		t.Fatal("有交付中的子任务不应收敛")
	}
}
