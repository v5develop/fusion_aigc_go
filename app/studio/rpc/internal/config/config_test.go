package config

import (
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
)

// 开发配置必须能加载（mkconf 以它为模板生成服务器配置），关键默认值与前缀不能丢。
func TestLoadDevConfig(t *testing.T) {
	var c Config
	if err := conf.Load("../../../../../deploy/etc/studio-rpc.yaml", &c); err != nil {
		t.Fatal(err)
	}
	if c.ListenOn != "0.0.0.0:9205" {
		t.Fatalf("端口应为 9205：%s", c.ListenOn)
	}
	if !c.Library.AsyncCopy || c.Library.Workers != 8 || c.Library.MaxAttempts != 10 || c.Library.ClaimSeconds != 300 || c.Library.SweepBatch != 50 {
		t.Fatalf("付费成品异步转存默认值不对：%+v", c.Library)
	}
	if c.TaskIDPrefix.Tryon != "tyo" || c.TaskIDPrefix.Pose != "pse" || c.TaskIDPrefix.ImageGen != "simg" {
		t.Fatalf("前缀不对：%+v", c.TaskIDPrefix)
	}
	if c.Reverse.BaseURL != "http://127.0.0.1:8090" || c.Timeouts.Generate != 300 || c.AiGateway.LongPollWait != 20 {
		t.Fatalf("默认值不对：%+v %+v %+v", c.Reverse, c.Timeouts, c.AiGateway)
	}
	if m := c.ModelGroupMap(); m["Banana"] != "img.nano-banana-2.1" || m["GPT"] != "img.gpt-image-2.6-sunburst" ||
		m["Seedream"] != "img.seedream-5.5" || m["通用"] != "img.gpt-image-2.5-sunburst" ||
		m["GPT25"] != "img.gpt-image-2.5-sunburst" || len(m) != 5 {
		t.Fatalf("模型映射不对：%v", c.ModelGroupMap())
	}
	if c.Reverse.Concurrency != 6 || c.Reverse.PreviewConcurrency != 2 || c.PreviewSidecarSlots() != 2 {
		t.Fatalf("预览名额不对：%+v slots=%d", c.Reverse, c.PreviewSidecarSlots())
	}
	if c.TryonPrompt.NewPercent != 0 {
		t.Fatalf("提示词灰度默认必须关闭：%d", c.TryonPrompt.NewPercent)
	}
	n := len(c.MethodTimeouts)
	c.EnsureMethodTimeouts()
	if len(c.MethodTimeouts) != n || c.MethodTimeouts[0].FullMethod != PreviewPoseMethod || c.MethodTimeouts[0].Timeout != PreviewPoseTimeout {
		t.Fatalf("配置里的预览超时应保留且不重复：%+v", c.MethodTimeouts)
	}
}

// 没配 MethodTimeouts 时补上预览超时；已配其它方法时追加。
func TestEnsureMethodTimeouts(t *testing.T) {
	var c Config
	c.EnsureMethodTimeouts()
	c.EnsureMethodTimeouts()
	if len(c.MethodTimeouts) != 1 || c.MethodTimeouts[0].FullMethod != PreviewPoseMethod || c.MethodTimeouts[0].Timeout != 240*time.Second {
		t.Fatalf("MethodTimeouts = %+v", c.MethodTimeouts)
	}
}

// 预览名额夹到 [1, Concurrency-1]：付费编排至少留一个 sidecar 名额。
func TestPreviewSidecarSlots(t *testing.T) {
	cases := []struct{ conc, preview, want int }{
		{6, 2, 2}, {6, 6, 5}, {3, 2, 2}, {2, 2, 1}, {1, 2, 1}, {0, 0, 2}, {6, 0, 2},
	}
	for _, cs := range cases {
		var c Config
		c.Reverse.Concurrency, c.Reverse.PreviewConcurrency = cs.conc, cs.preview
		if got := c.PreviewSidecarSlots(); got != cs.want {
			t.Fatalf("Concurrency=%d PreviewConcurrency=%d → %d，期望 %d", cs.conc, cs.preview, got, cs.want)
		}
	}
}

// 默认映射用规范组码；不出现任何 Dev 档组码 / 档名。
func TestDefaultModelGroupsCanonical(t *testing.T) {
	var c Config
	m := c.ModelGroupMap()
	if m["通用"] != "img.gpt-image-2.5-sunburst" || m["GPT25"] != "img.gpt-image-2.5-sunburst" || m["Banana"] != "img.nano-banana-2.1" {
		t.Fatalf("默认映射：%v", m)
	}
	if _, ok := m["GPT25Dev"]; ok {
		t.Fatalf("默认映射不应有旧键 GPT25Dev：%v", m)
	}
}

// 某环境 yaml 还是旧写法（GPT25Dev 键 + Dev 组码）：键映射为 GPT25、组码归一为规范组；同时配了 GPT25 以 GPT25 为准。
func TestModelGroupMapNormalizesLegacyYAML(t *testing.T) {
	c := Config{ModelGroups: map[string]string{
		"通用": "img.gpt-image-2.5-sunburst-developer", "Banana": "img.nano-banana-2.1-dev",
		"GPT25Dev": "img.gpt-image-2.5-sunburst-developer",
	}}
	m := c.ModelGroupMap()
	if m["通用"] != "img.gpt-image-2.5-sunburst" || m["Banana"] != "img.nano-banana-2.1" || m["GPT25"] != "img.gpt-image-2.5-sunburst" {
		t.Fatalf("旧写法未归一：%v", m)
	}
	if _, ok := m["GPT25Dev"]; ok || len(m) != 3 {
		t.Fatalf("旧键应映射掉：%v", m)
	}
	c = Config{ModelGroups: map[string]string{"GPT25": "img.gpt-x", "gpt25dev": "img.gpt-y"}}
	if m := c.ModelGroupMap(); m["GPT25"] != "img.gpt-x" || len(m) != 1 {
		t.Fatalf("GPT25 应优先：%v", m)
	}
}
