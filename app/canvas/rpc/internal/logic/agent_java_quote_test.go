package logic

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
)

// Java 计费工具的准确报价：svc 带该用户缓存 Bearer → 调 Java {path}/quote（price_known=true，points = Java 报价，请求体 = java_request.body）；
// mcp_token 不借 Bearer、不到 Java；Java 没有报价接口（404）→ price_known=false；max_points 用该报价比较。
func TestAgentQuoteJavaToolViaJavaQuote(t *testing.T) {
	c := e2eCore(t)
	var (
		mu     sync.Mutex
		auths  []string
		bodies []string
		gone   bool // 模拟旧版 Java（404）
	)
	java, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, javatool.QuoteSuffix) || gone {
			http.NotFound(w, r)
			return
		}
		auths, bodies = append(auths, r.Header.Get("Authorization")), append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"points":30,"priceBasis":{"priceUnit":"PER_IMAGE","quantity":1},"freeQuotaApplied":false}}`))
	})
	c.s.Java = javatool.New(java.URL, 5*time.Second)
	a := agentCaller{t: t, c: c, uid: 9311}
	cid, refs := a.build([]map[string]any{
		{"ref": "img", "type": "image", "image_url": "https://img.example.com/a.png"},
		{"ref": "up", "type": "tool", "tool": "upscale"},
	}, []map[string]any{{"source": "img", "target": "up", "target_handle": "image"}})
	args := map[string]any{"canvas_id": cid, "node_id": refs["up"]}

	// mcp_token：不借 Bearer（即便缓存里有），不到 Java，价格需要网页登录后查看
	c.s.Bearers.Put(a.uid, "tok-9311")
	q, isErr := a.call(mcptoken.AuthSourceMcpToken, agenttool.ToolQuoteNode, args, "")
	if isErr || q["price_known"] != false || q["available"] != false || q["reason"] != "这个工具的价格需要在网页里登录后查看" {
		t.Fatalf("mcp_token 报价: %v", q)
	}
	mu.Lock()
	if len(auths) != 0 {
		t.Fatalf("mcp_token 不应到 Java: %v", auths)
	}
	mu.Unlock()

	// svc + 缓存 Bearer：Java 报价
	q, isErr = a.call(mcptoken.AuthSourceSvc, agenttool.ToolQuoteNode, args, "")
	jr, _ := q["java_request"].(map[string]any)
	if isErr || q["available"] != true || q["price_known"] != true || q["points"] != float64(30) || q["billing"] != "java" || jr == nil {
		t.Fatalf("svc 报价应取 Java 点数: %v", q)
	}
	mu.Lock()
	if len(auths) != 1 || auths[0] != "Bearer tok-9311" || !strings.Contains(bodies[0], `"imageUrl":"https://img.example.com/a.png"`) {
		t.Fatalf("应带该用户 Bearer、请求体同 java_request: %v %v", auths, bodies)
	}
	mu.Unlock()

	// max_points：用 Java 报价比较（30 > 20 → BUDGET_EXCEEDED，不落 run、不到提交）；不给 expected_points 也不再 PRICE_UNKNOWN
	over := map[string]any{"canvas_id": cid, "node_id": refs["up"], "max_points": 20}
	out, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolRunNode, over, "jq-1")
	if !isErr || out["error_code"] != agenttool.ErrBudgetExceeded || out["quoted_points"] != float64(30) {
		t.Fatalf("Java 报价超上限应 BUDGET_EXCEEDED: %v", out)
	}

	// 旧版 Java（404）：回落 price_known=false；max_points 沿用 expected_points 规则
	mu.Lock()
	gone = true
	mu.Unlock()
	q, isErr = a.call(mcptoken.AuthSourceSvc, agenttool.ToolQuoteNode, args, "")
	if isErr || q["available"] != true || q["price_known"] != false || q["java_request"] == nil {
		t.Fatalf("旧版 Java 应回落 price_known=false: %v", q)
	}
	out, isErr = a.call(mcptoken.AuthSourceSvc, agenttool.ToolRunNode, over, "jq-2")
	if !isErr || out["error_code"] != agenttool.ErrPriceUnknownNeedsConfirm {
		t.Fatalf("没有 Java 报价也没有 expected_points 应 PRICE_UNKNOWN_NEEDS_CONFIRM: %v", out)
	}
}
