package logic

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/model"
)

// L7 真库：过期接管后旧执行者既不能再执行（StillOwned=false），也写不进收尾（Finish 影响 0 行）；新占位令牌不同、收尾成功。
func TestAgentClaimStaleTakeoverLeaseToken(t *testing.T) {
	c := e2eCore(t)
	ctx := context.Background()
	newRow := func() *model.AgentCall {
		return &model.AgentCall{UserID: 9101, Tool: agenttool.ToolAddNodes, ArgsHash: strings.Repeat("c", 64), Status: model.AgentCallRunning,
			CallID: sql.NullString{String: "k-stale", Valid: true}, IdemKey: sql.NullString{String: "k-stale", Valid: true}}
	}
	old := newRow()
	if resp, err := c.agentClaim(ctx, old); err != nil || resp != nil {
		t.Fatalf("首次占位应成功: %+v %v", resp, err)
	}
	if len(old.LeaseToken) != 32 || old.ID <= 0 {
		t.Fatalf("占位应带令牌: %+v", old)
	}
	if ok, err := c.s.Repo.AgentCall.StillOwned(ctx, old.ID, old.LeaseToken); err != nil || !ok {
		t.Fatalf("自己的占位应仍属于自己: %v %v", ok, err)
	}
	if ok, _ := c.s.Repo.AgentCall.StillOwned(ctx, old.ID, "other"); ok {
		t.Fatal("别人的令牌不应算属于自己")
	}
	// 同 key 再来：未过期 → 进行中
	if resp, err := c.agentClaim(ctx, newRow()); err != nil || resp == nil || !strings.Contains(resp.Result, agenttool.ErrInProgress) {
		t.Fatalf("未过期应报进行中: %+v %v", resp, err)
	}
	// 占位老化 → 新请求接管
	if _, err := c.s.Conn.ExecCtx(ctx, "UPDATE afv_canvas_agent_call SET created_at = NOW(3) - INTERVAL 10 MINUTE WHERE id = ?", old.ID); err != nil {
		t.Fatal(err)
	}
	cur := newRow()
	if resp, err := c.agentClaim(ctx, cur); err != nil || resp != nil {
		t.Fatalf("过期占位应被接管: %+v %v", resp, err)
	}
	if cur.ID == old.ID || cur.LeaseToken == old.LeaseToken {
		t.Fatalf("接管后应是新行新令牌: old=%d/%s new=%d/%s", old.ID, old.LeaseToken, cur.ID, cur.LeaseToken)
	}
	// 旧执行者：执行前复核失败；收尾影响 0 行（WARN），不改被接管的行
	if ok, _ := c.s.Repo.AgentCall.StillOwned(ctx, old.ID, old.LeaseToken); ok {
		t.Fatal("被接管后旧占位不应再属于旧执行者")
	}
	if ok, err := c.s.Repo.AgentCall.Finish(ctx, old.ID, old.LeaseToken, repoFinishOK()); err != nil || ok {
		t.Fatalf("被接管后旧执行者收尾应影响 0 行: %v %v", ok, err)
	}
	c.agentAudit(ctx, old, repoFinishOK()) // 只打 WARN，不报错
	var st int8
	var code string
	if err := c.s.Conn.QueryRowCtx(ctx, &st, "SELECT status FROM afv_canvas_agent_call WHERE id = ?", old.ID); err != nil || st != model.AgentCallFailed {
		t.Fatalf("被接管的行应保持失败: %d %v", st, err)
	}
	if err := c.s.Conn.QueryRowCtx(ctx, &code, "SELECT COALESCE(error_code, '') FROM afv_canvas_agent_call WHERE id = ?", old.ID); err != nil || code != "STALE" {
		t.Fatalf("被接管的行 error_code 应为 STALE: %v %v", code, err)
	}
	// 新执行者：令牌错不能收尾，令牌对可以
	if ok, _ := c.s.Repo.AgentCall.Finish(ctx, cur.ID, old.LeaseToken, repoFinishOK()); ok {
		t.Fatal("令牌不对不应收尾成功")
	}
	if ok, err := c.s.Repo.AgentCall.Finish(ctx, cur.ID, cur.LeaseToken, repoFinishOK()); err != nil || !ok {
		t.Fatalf("新执行者收尾应成功: %v %v", ok, err)
	}
	// 成功后同 key 回放
	if resp, err := c.agentClaim(ctx, newRow()); err != nil || resp == nil || !resp.Replayed {
		t.Fatalf("成功后同 key 应回放: %+v %v", resp, err)
	}
}
