package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
)

func TestAgentRunIDAttempts(t *testing.T) {
	a0, a1, a2 := agentRunID(7, "", "c", 0), agentRunID(7, "", "c", 1), agentRunID(7, "", "c", 2)
	if a0 == a1 || a1 == a2 || a0 == a2 {
		t.Fatalf("不同尝试序号应派生不同 run_id: %s %s %s", a0, a1, a2)
	}
	if agentRunID(7, "", "c", 1) != a1 {
		t.Fatal("同序号应稳定")
	}
	if agentRunID(7, "mine", "c", 3) != "mine" {
		t.Fatal("显式 run_id 不混序号")
	}
}

func TestAgentRunRetryable(t *testing.T) {
	mk := func(st int8, code, bill string) *model.NodeRun {
		return &model.NodeRun{Status: st, ErrorCode: sql.NullString{String: code, Valid: code != ""}, BillingStatus: bill}
	}
	cases := []struct {
		r    *model.NodeRun
		want bool
	}{
		{mk(model.RunFailed, "BILLING_REJECTED", model.BillNone), true},
		{mk(model.RunFailed, "SUBMIT_FAILED", model.BillRefunded), true},
		{mk(model.RunFailed, "SUBMIT_TIMEOUT", model.BillRefunded), true},
		{mk(model.RunFailed, "SUBMIT_FAILED", model.BillHeld), false},   // 仍冻结：可能扣点
		{mk(model.RunFailed, "SUBMIT_FAILED", model.BillReview), false}, // 待对账
		{mk(model.RunFailed, "SUBMIT_FAILED", model.BillJava), false},   // Java 自行计费
		{mk(model.RunFailed, "GEN_FAILED", model.BillRefunded), false},  // 生成阶段失败：不是受理失败
		{mk(model.RunQueued, "", model.BillHeld), false},                // 在途
		{mk(model.RunSucceeded, "", model.BillSettled), false},          // 已成功
		{mk(model.RunSubmitting, "", model.BillNone), false},            // 提交中
	}
	for i, c := range cases {
		if got, why := agentRunRetryable(c.r); got != c.want {
			t.Errorf("case %d: got %v (%s) want %v", i, got, why, c.want)
		}
	}
}

// 真库：受理失败未计费 → 同 call_id 换派生 run_id 重跑；参数改过 → RunNode 回放报 CONFLICT；可能已计费 → 回放原 run。
func TestAgentPickRunIDRetriesUnbilledFailure(t *testing.T) {
	c := e2eCore(t)
	ctx := context.Background()
	const uid, cid, nid, callID = int64(9001), "cv_retry", "m1", "call-retry"
	req := &canvas.RunNodeRequest{Kind: "image", Model: "test-model", Prompt: "一只猫", Count: 1}
	stored, _ := json.Marshal(toStored(req))
	insert := func(runID string, status int8, code, bill string, request []byte) {
		t.Helper()
		r := &model.NodeRun{CanvasID: cid, NodeID: nid, UserID: uid, RunID: runID, Kind: model.KindImage, Request: request,
			ModelGroup: "img.x", Backend: model.BackendGateway, Status: status, BillingStatus: bill, CostCNY: "0",
			ErrorCode: sql.NullString{String: code, Valid: code != ""}}
		if err := c.s.Repo.InsertRun(ctx, r, func(int64) (string, string) { return "t_" + runID, "b_" + runID }); err != nil {
			t.Fatalf("insert run: %v", err)
		}
	}
	id0, id1, id2 := agentRunID(uid, "", callID, 0), agentRunID(uid, "", callID, 1), agentRunID(uid, "", callID, 2)

	// 没有旧 run：用序号 0（与旧派生一致）
	if got, err := c.agentPickRunID(ctx, uid, "", callID, cid, nid, req); err != nil || got != id0 {
		t.Fatalf("首跑应用序号 0: %s %v", got, err)
	}
	// 冻结失败（未冻结）→ 换序号 1
	insert(id0, model.RunFailed, "BILLING_REJECTED", model.BillNone, stored)
	if got, _ := c.agentPickRunID(ctx, uid, "", callID, cid, nid, req); got != id1 {
		t.Fatalf("冻结失败应换序号 1: %s", got)
	}
	// 序号 1 提交失败已退款 → 换序号 2
	insert(id1, model.RunFailed, "SUBMIT_FAILED", model.BillRefunded, stored)
	if got, _ := c.agentPickRunID(ctx, uid, "", callID, cid, nid, req); got != id2 {
		t.Fatalf("提交失败已退款应换序号 2: %s", got)
	}
	// 序号 2 已排队冻结 → 回放序号 2，不再派生（不重复扣点）
	insert(id2, model.RunQueued, "", model.BillHeld, stored)
	if got, _ := c.agentPickRunID(ctx, uid, "", callID, cid, nid, req); got != id2 {
		t.Fatalf("在途 run 应回放: %s", got)
	}
	res, err := c.RunNode(ctx, &canvas.RunNodeReq{UserId: uid, CanvasId: cid, NodeId: nid, RunId: id2, Request: req})
	if err != nil || !res.Replayed {
		t.Fatalf("同参数应回放原 run: %+v %v", res, err)
	}

	// 参数改过：序号 0 的失败 run 参数不同 → 返回序号 0，RunNode 回放报 CONFLICT
	changed := &canvas.RunNodeRequest{Kind: "image", Model: "test-model", Prompt: "一只狗", Count: 1}
	got, err := c.agentPickRunID(ctx, uid, "", callID, cid, nid, changed)
	if err != nil || got != id0 {
		t.Fatalf("参数改过应落回序号 0: %s %v", got, err)
	}
	_, err = c.RunNode(ctx, &canvas.RunNodeReq{UserId: uid, CanvasId: cid, NodeId: nid, RunId: got, Request: changed})
	if code, _, ok := xerr.Parse(err); !ok || code != xerr.Conflict {
		t.Fatalf("参数改过应 CONFLICT: %v", err)
	}
	// 换节点也算参数改过
	if got, _ := c.agentPickRunID(ctx, uid, "", callID, cid, "m2", req); got != id0 {
		t.Fatalf("换节点应落回序号 0: %s", got)
	}

	// 可能已计费的失败（冻结未退）→ 回放，不重跑
	const call2 = "call-held"
	h0 := agentRunID(uid, "", call2, 0)
	insert(h0, model.RunFailed, "SUBMIT_FAILED", model.BillHeld, stored)
	if got, _ := c.agentPickRunID(ctx, uid, "", call2, cid, nid, req); got != h0 {
		t.Fatalf("冻结未退的失败不应重跑: %s", got)
	}

	// 连续失败超过上限 → 拒绝
	const call3 = "call-many"
	for a := 0; a < agentRunMaxAttempts; a++ {
		insert(agentRunID(uid, "", call3, a), model.RunFailed, "BILLING_REJECTED", model.BillNone, stored)
	}
	_, err = c.agentPickRunID(ctx, uid, "", call3, cid, nid, req)
	if ae, ok := err.(*agentError); !ok || ae.ErrorCode != agenttool.ErrIdempotencyConflict || !strings.Contains(ae.Message, "call_id") {
		t.Fatalf("超过上限应拒绝: %v", err)
	}

	// 显式 run_id：不派生
	if got, _ := c.agentPickRunID(ctx, uid, "explicit-1", callID, cid, nid, req); got != "explicit-1" {
		t.Fatalf("显式 run_id 原样: %s", got)
	}
}

// L6：调用方 ctx 已取消，审计收尾仍要落库（占位行收尾、无占位落终态行）。
func TestAgentAuditSurvivesCanceledContext(t *testing.T) {
	c := e2eCore(t)
	ctx, cancel := context.WithCancel(context.Background())
	// 占位行：未取消时插入
	held := &model.AgentCall{UserID: 9002, Tool: agenttool.ToolAddNodes, ArgsHash: strings.Repeat("a", 64), Status: model.AgentCallRunning,
		CallID: sql.NullString{String: "k1", Valid: true}, IdemKey: sql.NullString{String: "k1", Valid: true}}
	if err := c.s.Repo.AgentCall.Insert(ctx, held); err != nil {
		t.Fatal(err)
	}
	cancel()
	c.agentAudit(ctx, held, repoFinishOK())
	got, err := c.s.Repo.AgentCall.GetByIdem(context.Background(), 9002, "k1")
	if err != nil || got == nil || got.Status != model.AgentCallOK {
		t.Fatalf("取消的 ctx 下占位行应收尾为成功: %+v %v", got, err)
	}
	// 无占位：落一行终态
	row := &model.AgentCall{UserID: 9003, Tool: agenttool.ToolAddNodes, ArgsHash: strings.Repeat("b", 64), Status: model.AgentCallRunning}
	c.agentAudit(ctx, row, repoFinishOK())
	if row.ID <= 0 {
		t.Fatal("取消的 ctx 下应落终态行")
	}
}

func repoFinishOK() repo.AgentFinish {
	return repo.AgentFinish{Status: model.AgentCallOK, Result: json.RawMessage(`{"ok":true}`)}
}
