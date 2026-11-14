package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"

	"google.golang.org/protobuf/proto"
)

// 真库：服务间 / MCP 令牌调 canvas_run_node 的去重——在途不限时间，已成功 120 秒窗口；指纹含上游实际产物。
func TestAgentDedupeRunWindow(t *testing.T) {
	c := e2eCore(t)
	const uid, cid, nid = int64(9101), "cv_dedupe", "m1"
	req := &canvas.RunNodeRequest{Kind: "image", Model: "test-model", Prompt: "一只猫", Count: 1}
	norm := proto.Clone(req).(*canvas.RunNodeRequest)
	c.normalizeRunSize(norm)
	stored, _ := json.Marshal(toStored(norm))
	seq := 0
	insertAt := func(node string, status int8, request []byte, created time.Time) int64 {
		t.Helper()
		seq++
		runID := "dd_" + strconv.Itoa(seq) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
		r := &model.NodeRun{CanvasID: cid, NodeID: node, UserID: uid, RunID: runID, Kind: model.KindImage, Request: request,
			ModelGroup: "img.x", Backend: model.BackendGateway, Status: status, BillingStatus: model.BillSettled, CostCNY: "0", Points: 30}
		if err := c.s.Repo.InsertRun(context.Background(), r, func(int64) (string, string) { return "t_" + runID, "b_" + runID }); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if !created.IsZero() { // 把 created_at 挪到过去（库内按会话时区换算）
			if _, err := c.s.DB.ExecContext(context.Background(), "UPDATE afv_canvas_node_run SET created_at = FROM_UNIXTIME(? / 1000) WHERE id = ?", created.UnixMilli(), r.ID); err != nil {
				t.Fatal(err)
			}
		}
		return r.ID
	}
	insert := func(status int8, request []byte) int64 { return insertAt(nid, status, request, time.Time{}) }
	svcCtx := withAuthSource(context.Background(), mcptoken.AuthSourceSvc)
	mcpCtx := withAuthSource(context.Background(), mcptoken.AuthSourceMcpToken)
	userCtx := withAuthSource(context.Background(), mcptoken.AuthSourceUser)
	dedupe := func(ctx context.Context, u int64, node string, r *canvas.RunNodeRequest, up map[string]int64) (map[string]any, *agentMeta) {
		t.Helper()
		meta := &agentMeta{}
		out, err := c.agentDedupeRun(ctx, u, cid, node, r, up, meta)
		if err != nil {
			t.Fatal(err)
		}
		return out, meta
	}

	if out, _ := dedupe(svcCtx, uid, nid, req, nil); out != nil {
		t.Fatal("无旧 run 不应命中")
	}
	insert(model.RunFailed, stored)
	if out, _ := dedupe(svcCtx, uid, nid, req, nil); out != nil {
		t.Fatal("失败 run 不应命中")
	}
	okID := insert(model.RunSucceeded, stored)
	for name, ctx := range map[string]context.Context{"svc": svcCtx, "mcp": mcpCtx} {
		out, meta := dedupe(ctx, uid, nid, req, nil)
		if out == nil || out["replayed"] != true || meta.runRequestID != okID {
			t.Fatalf("%s 应命中 120 秒内已成功的 run %d: %v", name, okID, out)
		}
	}
	if out, _ := dedupe(userCtx, uid, nid, req, nil); out != nil {
		t.Fatal("登录令牌不应去重")
	}
	if out, _ := dedupe(context.Background(), uid, nid, req, nil); out != nil {
		t.Fatal("未标来源不应去重")
	}
	other := proto.Clone(req).(*canvas.RunNodeRequest)
	other.Prompt = "一只狗"
	if out, _ := dedupe(svcCtx, uid, nid, other, nil); out != nil {
		t.Fatal("指纹不同不应命中")
	}
	if out, _ := dedupe(svcCtx, uid+1, nid, req, nil); out != nil {
		t.Fatal("别的用户不应命中")
	}

	// E：已成功超过 120 秒不命中；在途的 run 即便受理于很久以前也命中
	insertAt("m_old_ok", model.RunSucceeded, stored, time.Now().Add(-10*time.Minute))
	if out, _ := dedupe(svcCtx, uid, "m_old_ok", req, nil); out != nil {
		t.Fatal("超过窗口的已成功 run 不应命中")
	}
	oldRunning := insertAt("m_old_run", model.RunRunning, stored, time.Now().Add(-30*time.Minute))
	if out, meta := dedupe(mcpCtx, uid, "m_old_run", req, nil); out == nil || meta.runRequestID != oldRunning {
		t.Fatalf("在途 run 不受 120 秒限制应命中: %v", out)
	}

	// D：指纹含上游实际产物——候选 run 冻结的上游 run 与当前解析一致才命中；上游重做（新 run id）后不命中；旧 run 无冻结记录不命中
	ci := int32(0)
	refReq := proto.Clone(req).(*canvas.RunNodeRequest)
	refReq.References = []*canvas.RunReference{{Handle: "image", NodeId: "m_up", Index: 1, CandidateIndex: &ci}}
	refNorm := proto.Clone(refReq).(*canvas.RunNodeRequest)
	c.normalizeRunSize(refNorm)
	st := toStored(refNorm)
	st.Frozen = &frozenRefs{Refs: map[string]frozenRef{"references.0": {Ref: "mr://up1", RunID: 501, Index: 0}}}
	withFrozen, _ := json.Marshal(st)
	refRun := insertAt("m_down", model.RunSucceeded, withFrozen, time.Time{})
	if out, meta := dedupe(svcCtx, uid, "m_down", refReq, map[string]int64{"references.0": 501}); out == nil || meta.runRequestID != refRun {
		t.Fatalf("上游未变应命中: %v", out)
	}
	if out, _ := dedupe(svcCtx, uid, "m_down", refReq, map[string]int64{"references.0": 777}); out != nil {
		t.Fatal("上游重做后（上游 run 变了）不应命中")
	}
	st.Frozen = nil
	legacy, _ := json.Marshal(st)
	insertAt("m_legacy", model.RunSucceeded, legacy, time.Time{})
	if out, _ := dedupe(svcCtx, uid, "m_legacy", refReq, map[string]int64{"references.0": 501}); out != nil {
		t.Fatal("没有冻结记录的旧 run 不应命中")
	}
}

func TestUpstreamRunsAndExplicitRunID(t *testing.T) {
	ci := int32(1)
	req := &canvas.RunNodeRequest{References: []*canvas.RunReference{
		{Handle: "prompt", NodeId: "t1", Text: "x"},
		{Handle: "image", NodeId: "m_up", CandidateIndex: &ci},
		{Handle: "image", NodeId: "img", Image: "https://x/a.png"},
		{Handle: "video", NodeId: "v_up"},
	}}
	done := map[string]*model.NodeRun{"m_up": {ID: 11}, "v_up": {ID: 22}, "t1": {ID: 33}}
	got := upstreamRuns(req, done)
	if len(got) != 2 || got["references.1"] != 11 || got["references.3"] != 22 {
		t.Fatalf("上游槽位不对: %v", got)
	}
	if !explicitRunID(map[string]any{"run_id": "mine"}) || explicitRunID(map[string]any{}) || explicitRunID(map[string]any{"run_id": " "}) {
		t.Fatal("explicitRunID")
	}
}

// 中1（真库）：MCP 令牌取消运行中的 Java 工具 run → MCP_TOKEN_JAVA_TOOL_UNSUPPORTED，不借用缓存里的网页登录令牌、run 不变。
func TestCancelJavaRunViaMcpToken(t *testing.T) {
	c := e2eCore(t)
	const uid = int64(9201)
	c.s.Bearers.Put(uid, "Bearer web-session")
	r := &model.NodeRun{CanvasID: "cv_java", NodeID: "tool_1", UserID: uid, RunID: "java_run_1", Kind: model.KindImage, Request: []byte(`{}`),
		ModelGroup: "", Backend: model.BackendJava, Status: model.RunRunning, BillingStatus: model.BillJava, CostCNY: "0",
		BackendRef: sql.NullString{String: "AiCreation_x", Valid: true}}
	if err := c.s.Repo.InsertRun(context.Background(), r, func(int64) (string, string) { return "t_java", "b_java" }); err != nil {
		t.Fatal(err)
	}
	_, err := c.CancelRun(withAuthSource(context.Background(), mcptoken.AuthSourceMcpToken), &canvas.RunIdReq{UserId: uid, Id: r.ID})
	if ae := asAgentErr(context.Background(), "canvas_cancel_run", err); ae == nil || ae.ErrorCode != "MCP_TOKEN_JAVA_TOOL_UNSUPPORTED" {
		t.Fatalf("应回 MCP_TOKEN_JAVA_TOOL_UNSUPPORTED: %v", err)
	}
	got, _ := c.s.Repo.Run.Get(context.Background(), r.ID)
	if got == nil || got.Status != model.RunRunning {
		t.Fatalf("run 不应被改动: %+v", got)
	}
}
