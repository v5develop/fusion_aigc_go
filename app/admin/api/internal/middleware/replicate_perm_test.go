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
		// 用户投稿（Go_V1.0.58）：读 = view，写（通过 / 驳回 / 标记奖励）= sample
		{"GET", "/api/v1/replicate/admin/shares", "replicate:view"},
		{"GET", "/api/v1/replicate/admin/shares/3", "replicate:view"},
		{"POST", "/api/v1/replicate/admin/shares/3/approve", "replicate:sample"},
		{"POST", "/api/v1/replicate/admin/shares/3/reject", "replicate:sample"},
		{"POST", "/api/v1/replicate/admin/shares/3/reward", "replicate:sample"},
		{"POST", "/api/v1/replicate/admin/shares/3/reward/claim", "replicate:sample"},
		{"POST", "/api/v1/replicate/admin/shares/3/reward/release", "replicate:sample"},
		// 分享奖励规则（CONTRACT §U）：读 = view，写 = sample
		{"GET", "/api/v1/replicate/admin/share-reward-settings", "replicate:view"},
		{"PUT", "/api/v1/replicate/admin/share-reward-settings", "replicate:sample"},
		{"DELETE", "/api/v1/replicate/admin/share-reward-settings", "replicate:sample"},
		// 成片反馈（CONTRACT §AB）：只读，沿用 view；写（本不存在）从严
		{"GET", "/api/v1/replicate/admin/feedback", "replicate:view"},
		{"GET", "/api/v1/replicate/admin/feedback/summary", "replicate:view"},
		{"POST", "/api/v1/replicate/admin/feedback", "replicate:force"},
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
