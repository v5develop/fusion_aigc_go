package logic

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func TestAgentBudgetArgs(t *testing.T) {
	if _, ok := agentBudgetArgs(map[string]any{}); ok {
		t.Fatal("没给 max_points 不设上限")
	}
	if _, ok := agentBudgetArgs(map[string]any{"expected_points": float64(5)}); ok {
		t.Fatal("只给 expected_points 不设上限")
	}
	b, ok := agentBudgetArgs(map[string]any{"max_points": float64(100), "expected_points": float64(80)})
	if !ok || b.Max != 100 || !b.HasExpected || b.Expected != 80 {
		t.Fatalf("解析: %+v", b)
	}
	b, _ = agentBudgetArgs(map[string]any{"max_points": float64(0)})
	if b.Max != 0 || b.HasExpected {
		t.Fatalf("max_points=0 也是上限: %+v", b)
	}
}

// Java 计费工具（price_known=false）：没有 expected_points → PRICE_UNKNOWN_NEEDS_CONFIRM；有则按估价比较。免费工具按 0。没有上限不检查。
func TestCheckBudgetJavaAndFree(t *testing.T) {
	c := authTestCore()
	java := &runSpec{Backend: model.BackendJava}
	code := func(err error) string {
		var ae *agentError
		if errors.As(err, &ae) {
			return ae.ErrorCode
		}
		return ""
	}
	if err := c.checkBudget(context.Background(), 1, java); err != nil {
		t.Fatalf("没有上限不应检查: %v", err)
	}
	ctx := withBudget(context.Background(), runBudget{Max: 50})
	if got := code(c.checkBudget(ctx, 1, java)); got != agenttool.ErrPriceUnknownNeedsConfirm {
		t.Fatalf("Java 工具没估价应 PRICE_UNKNOWN_NEEDS_CONFIRM: %s", got)
	}
	err := c.checkBudget(withBudget(context.Background(), runBudget{Max: 50, Expected: 60, HasExpected: true}), 1, java)
	var ae *agentError
	if !errors.As(err, &ae) || ae.ErrorCode != agenttool.ErrBudgetExceeded || ae.QuotedPoints != 60 || ae.MaxPoints != 50 {
		t.Fatalf("估价超上限应 BUDGET_EXCEEDED 且带 quoted_points: %v", err)
	}
	if err := c.checkBudget(withBudget(context.Background(), runBudget{Max: 60, Expected: 60, HasExpected: true}), 1, java); err != nil {
		t.Fatalf("估价等于上限应放行: %v", err)
	}
	if err := c.checkBudget(withBudget(context.Background(), runBudget{Max: 0}), 1, &runSpec{Backend: model.BackendMedia}); err != nil {
		t.Fatalf("裁剪不计费，上限 0 也放行: %v", err)
	}
}

// 真库 + 本地 fusion_aigc 价目（shadow，只算价不动账）：报价 > max_points → BUDGET_EXCEEDED（不落 run、不冻结）；= 报价 → 正常执行且冻结 = 报价。
func TestAgentRunNodeMaxPoints(t *testing.T) {
	c := e2eCore(t)
	dsn := os.Getenv("CANVAS_BILLING_DSN")
	if dsn == "" {
		dsn = "root:123456@tcp(127.0.0.1:43306)/fusion_aigc?parseTime=true&loc=Asia%2FShanghai&charset=utf8mb4&timeout=2s"
	}
	conn := sqlx.NewMysql(dsn)
	var n int
	if err := conn.QueryRowCtx(context.Background(), &n, "SELECT COUNT(*) FROM afv_billing_offer WHERE entry_code = 'canvas_image' AND status='ENABLED' AND deleted=0"); err != nil || n < 1 {
		t.Skipf("本地 fusion_aigc 没有画布价目（%d）: %v", n, err)
	}
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeShadow}, conn)
	gw, _ := fakeServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAVAILABLE","message":"down"}}`))
	})
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: gw.URL, Secret: strings.Repeat("s", 32), Timeout: 2 * time.Second})
	c.s.Config.Poll.MaxSubmitAttempts = 5

	a := agentCaller{t: t, c: c, uid: 9401}
	cid, refs := a.build([]map[string]any{
		{"ref": "txt", "type": "text", "text": "一只橘猫"},
		{"ref": "pic", "type": "image_model", "count": 2},
	}, []map[string]any{{"source": "txt", "target": "pic", "target_handle": "prompt"}})
	args := map[string]any{"canvas_id": cid, "node_id": refs["pic"]}
	q, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolQuoteNode, args, "")
	points, _ := q["points"].(float64)
	if isErr || points <= 0 {
		t.Fatalf("报价: %v", q)
	}
	over := map[string]any{"canvas_id": cid, "node_id": refs["pic"], "max_points": points - 1}
	out, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolRunNode, over, "budget-1")
	if !isErr || out["error_code"] != agenttool.ErrBudgetExceeded || out["quoted_points"] != points || out["max_points"] != points-1 {
		t.Fatalf("超上限应 BUDGET_EXCEEDED: %v", out)
	}
	if runs, _ := c.ListRuns(context.Background(), &canvas.ListRunsReq{UserId: 9401, CanvasId: cid}); len(runs.List) != 0 {
		t.Fatalf("超上限不应落 run: %d", len(runs.List))
	}
	ok := map[string]any{"canvas_id": cid, "node_id": refs["pic"], "max_points": points}
	out, isErr = a.call(mcptoken.AuthSourceSvc, agenttool.ToolRunNode, ok, "budget-2")
	if isErr || out["credits"] != points {
		t.Fatalf("上限等于报价应执行且冻结 = 报价: %v", out)
	}
}
