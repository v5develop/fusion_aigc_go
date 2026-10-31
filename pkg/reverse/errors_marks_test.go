package reverse

import "testing"

// 新旧两版 sidecar 的「未配置」文案都要判为 KindNotConfigured（不可重试），其余 502 仍可重试。
func TestClassifyNotConfiguredMarks(t *testing.T) {
	cases := []struct {
		msg  string
		want Kind
	}{
		{"裁切质检失败：未配置 AI 网关（AIG_BASE_URL / AIG_SECRET）", KindNotConfigured},
		{"AI 网关未配置：AIG_SECRET", KindNotConfigured},
		{"裁切质检失败：未配置 DeepSeek Key", KindNotConfigured},
		{"分析失败：上游超时", KindUpstream},
	}
	for _, c := range cases {
		if got := classify("/api/reverse/crop-check", 502, c.msg).Kind; got != c.want {
			t.Errorf("%q: got %v want %v", c.msg, got, c.want)
		}
	}
}
