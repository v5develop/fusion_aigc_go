package model

import (
	"database/sql"
	"time"

	"github.com/uptrace/bun"
)

// McpToken afv_canvas_mcp_token（Go_V1.0.42）：画布 MCP 访问令牌。明文不落库，只存 sha256 与前缀。
type McpToken struct {
	bun.BaseModel `bun:"table:afv_canvas_mcp_token,alias:mt"`

	ID          int64        `bun:"id,pk,autoincrement"`
	UserID      int64        `bun:"user_id,notnull"`
	Name        string       `bun:"name,notnull"`
	TokenPrefix string       `bun:"token_prefix,notnull"`
	TokenHash   string       `bun:"token_hash,notnull"`
	Scope       string       `bun:"scope,notnull"`
	ExpiresAt   time.Time    `bun:"expires_at,notnull"`
	LastUsedAt  sql.NullTime `bun:"last_used_at"`
	LastUsedIP  string       `bun:"last_used_ip,notnull"`
	CreatedIP   string       `bun:"created_ip,notnull"`
	RevokedAt   sql.NullTime `bun:"revoked_at"`
	CreatedAt   time.Time    `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt   time.Time    `bun:"updated_at,notnull,default:current_timestamp(3)"`

	// Alive 查询时由库按 NOW(3) 算出的「未过期」（不落库；避免应用与库时区 / 时钟不一致）
	Alive bool `bun:"alive,scanonly"`
	// UserStatus 所有者在 afv_app_user（Java 用户表，未逻辑删除）的 status：1 启用、0 禁用；NULL = 用户不存在或已删除。
	// 只在按哈希校验时联查（判据与 Java InternalAuthIntrospectController.introspectUser 同源）
	UserStatus sql.NullInt64 `bun:"user_status,scanonly"`
}
