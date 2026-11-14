package repo

import (
	"context"
	"encoding/json"
	"time"

	"aigc-local-life/app/canvas/model"

	"github.com/uptrace/bun"
)

// TableAgentCall 智能体工具调用审计 / 幂等表。
const TableAgentCall = "afv_canvas_agent_call"

// AgentCallRepo afv_canvas_agent_call。
type AgentCallRepo struct{ db bun.IDB }

// Insert 落一行（执行中占位或终态审计）。idem_key 撞唯一键返回 IsDuplicate 错误。
func (r *AgentCallRepo) Insert(ctx context.Context, x *model.AgentCall) error {
	_, err := r.db.NewInsert().Model(x).ExcludeColumn("created_at", "updated_at").Exec(ctx)
	return err
}

// GetByIdem 按 (user_id, idem_key) 取占位 / 成功记录。
func (r *AgentCallRepo) GetByIdem(ctx context.Context, userID int64, key string) (*model.AgentCall, error) {
	x := new(model.AgentCall)
	return noRows(x, r.db.NewSelect().Model(x).Where("ac.user_id = ?", userID).Where("ac.idem_key = ?", key).Limit(1).Scan(ctx))
}

// AgentFinish 一次调用的收尾字段。
type AgentFinish struct {
	Status         int8
	CanvasID       string
	ErrorCode      string
	ErrorMsg       string
	RevisionBefore string
	RevisionAfter  string
	RunRequestID   int64
	Result         json.RawMessage
	DurationMS     int32
	ReleaseIdem    bool // 失败：释放 idem_key，允许同 call_id 重试
}

// Finish 占位行收尾：只改执行中、且占位令牌仍是 token 的行。ok=false 表示占位已被过期接管（置失败）或不存在。
func (r *AgentCallRepo) Finish(ctx context.Context, id int64, token string, f AgentFinish) (bool, error) {
	q := r.db.NewUpdate().Table(TableAgentCall).
		Set("status = ?", f.Status).
		Set("canvas_id = COALESCE(?, canvas_id)", nullable(f.CanvasID)).
		Set("error_code = ?", nullable(f.ErrorCode)).
		Set("error_msg = ?", nullable(f.ErrorMsg)).
		Set("revision_before = ?", nullable(f.RevisionBefore)).
		Set("revision_after = ?", nullable(f.RevisionAfter)).
		Set("run_request_id = ?", nullInt(f.RunRequestID)).
		Set("result = ?", nullJSON(f.Result)).
		Set("duration_ms = ?", f.DurationMS).
		Set("updated_at = NOW(3)")
	if f.ReleaseIdem {
		q = q.Set("idem_key = NULL")
	}
	n, err := affected(q.Where("id = ?", id).Where("lease_token = ?", token).Where("status = ?", model.AgentCallRunning).Exec(ctx))
	return n == 1, err
}

// StillOwned 执行前复核：占位行仍是执行中、令牌仍是 token（没被过期接管）。
func (r *AgentCallRepo) StillOwned(ctx context.Context, id int64, token string) (bool, error) {
	n, err := r.db.NewSelect().Table(TableAgentCall).Where("id = ?", id).Where("lease_token = ?", token).
		Where("status = ?", model.AgentCallRunning).Count(ctx)
	return n == 1, err
}

// ReleaseStale 接管崩溃残留的占位：执行中且早于 before 的行置失败并释放 idem_key。ok=true 表示接管成功。
func (r *AgentCallRepo) ReleaseStale(ctx context.Context, id int64, before time.Time) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableAgentCall).
		Set("status = ?", model.AgentCallFailed).Set("idem_key = NULL").Set("error_code = 'STALE'").Set("updated_at = NOW(3)").
		Where("id = ?", id).Where("status = ?", model.AgentCallRunning).
		Where("created_at < FROM_UNIXTIME(? / 1000)", before.UnixMilli()).Exec(ctx))
	return n == 1, err
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
