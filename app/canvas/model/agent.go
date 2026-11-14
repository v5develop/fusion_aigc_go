package model

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 智能体工具调用状态（afv_canvas_agent_call.status，Go_V1.0.38）。
const (
	AgentCallRunning int8 = 0 // 执行中（幂等占位）
	AgentCallOK      int8 = 1 // 成功
	AgentCallFailed  int8 = 2 // 失败（idem_key 已释放，可用同 call_id 重试）
)

// AgentCall afv_canvas_agent_call：一次写 / 生成类智能体工具调用的审计与幂等记录。
type AgentCall struct {
	bun.BaseModel `bun:"table:afv_canvas_agent_call,alias:ac"`

	ID             int64           `bun:"id,pk,autoincrement"`
	UserID         int64           `bun:"user_id,notnull"`
	CanvasID       sql.NullString  `bun:"canvas_id"`
	Tool           string          `bun:"tool,notnull"`
	Client         string          `bun:"client,notnull"`
	Agent          string          `bun:"agent,notnull"`
	CallID         sql.NullString  `bun:"call_id"`
	IdemKey        sql.NullString  `bun:"idem_key"`            // = call_id；失败时置 NULL 释放
	LeaseToken     string          `bun:"lease_token,notnull"` // 占位令牌：执行前复核、收尾按它 CAS（被过期接管后旧执行者写不进）
	ArgsHash       string          `bun:"args_hash,notnull"`
	Args           json.RawMessage `bun:"args,type:json,nullzero"`
	Status         int8            `bun:"status,notnull"`
	ErrorCode      sql.NullString  `bun:"error_code"`
	ErrorMsg       sql.NullString  `bun:"error_msg"`
	RevisionBefore sql.NullString  `bun:"revision_before"`
	RevisionAfter  sql.NullString  `bun:"revision_after"`
	RunRequestID   sql.NullInt64   `bun:"run_request_id"`
	Result         json.RawMessage `bun:"result,type:json,nullzero"`
	DurationMS     int32           `bun:"duration_ms,notnull"`
	ClientIP       string          `bun:"client_ip,notnull"`
	CreatedAt      time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt      time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// CanvasRevision afv_canvas_revision（Go_V1.0.40）：画布最近若干个 revision 的快照（三方合并的 base）。
// Snapshot 是存储态快照（运行态已剥离）的 gzip；每张画布只留最近 Limits.RevisionKeep 个，写入后按画布裁剪。
type CanvasRevision struct {
	bun.BaseModel `bun:"table:afv_canvas_revision,alias:cr"`

	ID            int64     `bun:"id,pk,autoincrement"`
	CanvasID      string    `bun:"canvas_id,notnull"`
	UserID        int64     `bun:"user_id,notnull"`
	Revision      string    `bun:"revision,notnull"`
	Source        string    `bun:"source,notnull"`
	Actor         string    `bun:"actor,notnull"`
	NodeCount     int32     `bun:"node_count,notnull"`
	SnapshotBytes int32     `bun:"snapshot_bytes,notnull"` // 解压后字节数
	Snapshot      []byte    `bun:"snapshot,notnull"`       // gzip(存储态快照 JSON)
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp(3)"`
}
