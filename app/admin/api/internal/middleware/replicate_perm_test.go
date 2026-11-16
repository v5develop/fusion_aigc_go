package middleware

import (
	"net/http/httptest"
	"testing"
)

// 视频做同款后台（Go_V1.0.39）：GET = replicate:view；退点 replicate:refund；强制收尾 replicate:force；不规范路径按 Clean 后取。
func TestPermForReplicate(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"GET", "/api/v1/replicate/admin/jobs", "replicate:view"},
		{"GET", "/api/v1/replicate/admin/jobs/42", "replicate:view"},
		{"POST", "/api/v1/replicate/admin/jobs/42/refund", "replicate:refund"},
		{"POST", "/api/v1/replicate/admin/jobs/42/force-finish", "replicate:force"},
		{"POST", "/api/v1/replicate/admin/jobs/42/other", "replicate:force"}, // 未知写接口从严
		{"POST", "/api/v1/replicate/admin//jobs/42/./refund", "replicate:refund"},
		{"POST", "/api/v1/local-life/../replicate/admin/jobs/1/force-finish", "replicate:force"},
		{"GET", "/api/v1/replicate/admin/jobs/42/refund", "replicate:view"}, // GET 不是退点
		{"POST", "/api/v1/replicate/jobs/42/refund", "local_life:update"},   // 用户端路径不在后台前缀内
		// 示例管理（Go_V1.0.41）：读 = view，写 = sample（不再要求 force）
		{"GET", "/api/v1/replicate/admin/samples", "replicate:view"},
		{"POST", "/api/v1/replicate/admin/samples", "replicate:sample"},
		{"PUT", "/api/v1/replicate/admin/samples/7", "replicate:sample"},
		{"DELETE", "/api/v1/replicate/admin/samples/7", "replicate:sample"},
		{"POST", "/api/v1/replicate/admin/samples/7/analyze", "replicate:sample"},
		{"PUT", "/api/v1/replicate/admin/jobs/42", "replicate:force"}, // 非示例的其它写仍从严
		// 示例素材：上传 = sample；解析预览只读 = view
		{"POST", "/api/v1/replicate/admin/samples/media/upload", "replicate:sample"},
		{"POST", "/api/v1/replicate/admin/samples/media/resolve", "replicate:view"},
		// 总开关（Go_V1.0.50）：读 = view，写 = switch
		{"GET", "/api/v1/replicate/admin/switch", "replicate:view"},
		{"PUT", "/api/v1/replicate/admin/switch", "replicate:switch"},
		{"DELETE", "/api/v1/replicate/admin/switch", "replicate:switch"},
	}
	for _, c := range cases {
		if got := permFor(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s = %s, want %s", c.method, c.path, got, c.want)
		}
	}
}
