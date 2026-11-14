package repo

import (
	"context"

	"aigc-local-life/app/canvas/model"

	"github.com/uptrace/bun"
)

// TableMcpToken 画布 MCP 访问令牌表（Go_V1.0.42）。
const TableMcpToken = "afv_canvas_mcp_token"

// McpTokenRepo afv_canvas_mcp_token。
type McpTokenRepo struct{ db bun.IDB }

func (r *McpTokenRepo) sel(x any) *bun.SelectQuery {
	return r.db.NewSelect().Model(x).ColumnExpr("mt.*").ColumnExpr("(mt.expires_at > NOW(3)) AS alive")
}

// CountActiveForUpdate 用户生效中（未吊销、未过期）的令牌数，并锁住扫描到的行（须在事务里调，串行化同一用户的并发创建）。
func (r *McpTokenRepo) CountActiveForUpdate(ctx context.Context, userID int64) (int, error) {
	var ids []int64
	err := r.db.NewSelect().Table(TableMcpToken).Column("id").
		Where("user_id = ?", userID).Where("revoked_at IS NULL").Where("expires_at > NOW(3)").
		For("UPDATE").Scan(ctx, &ids)
	return len(ids), err
}

// Insert 新建；expires_at 由库按 NOW(3) + days 计算。返回带 id 的行（重新读出，时间以库为准）。
func (r *McpTokenRepo) Insert(ctx context.Context, x *model.McpToken, days int32) (*model.McpToken, error) {
	res, err := r.db.NewInsert().Model(x).
		ExcludeColumn("id", "last_used_at", "revoked_at", "created_at", "updated_at").
		Value("expires_at", "DATE_ADD(NOW(3), INTERVAL ? DAY)", days).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, x.UserID, id)
}

// Get 取用户自己的一枚（含已吊销）；找不到 (nil, nil)。
func (r *McpTokenRepo) Get(ctx context.Context, userID, id int64) (*model.McpToken, error) {
	x := new(model.McpToken)
	return noRows(x, r.sel(x).Where("mt.id = ?", id).Where("mt.user_id = ?", userID).Limit(1).Scan(ctx))
}

// ListLive 用户未吊销的令牌（含已过期），新的在前，最多 limit 条。
func (r *McpTokenRepo) ListLive(ctx context.Context, userID int64, limit int) ([]model.McpToken, error) {
	var xs []model.McpToken
	err := r.sel(&xs).Where("mt.user_id = ?", userID).Where("mt.revoked_at IS NULL").
		OrderExpr("mt.id DESC").Limit(int64(limit)).Scan(ctx)
	return xs, err
}

// TableAppUser Java 用户表（只读：校验令牌时联查所有者状态）。
const TableAppUser = "afv_app_user"

// GetByHash 按哈希取（含已吊销 / 已过期，调用方判断），顺带联查所有者在 afv_app_user 的状态（UserStatus）；找不到 (nil, nil)。
func (r *McpTokenRepo) GetByHash(ctx context.Context, hash string) (*model.McpToken, error) {
	x := new(model.McpToken)
	return noRows(x, r.sel(x).
		ColumnExpr("(SELECT u.status FROM "+TableAppUser+" AS u WHERE u.id = mt.user_id AND u.deleted = 0 LIMIT 1) AS user_status").
		Where("mt.token_hash = ?", hash).Limit(1).Scan(ctx))
}

// Revoke 吊销用户自己的一枚；ok=false 表示不存在或已吊销。
func (r *McpTokenRepo) Revoke(ctx context.Context, userID, id int64) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(TableMcpToken).Set("revoked_at = NOW(3)").Set("updated_at = NOW(3)").
		Where("id = ?", id).Where("user_id = ?", userID).Where("revoked_at IS NULL").Exec(ctx))
	return n == 1, err
}

// Touch 记最后使用时间与 IP。
func (r *McpTokenRepo) Touch(ctx context.Context, id int64, ip string) error {
	_, err := r.db.NewUpdate().Table(TableMcpToken).Set("last_used_at = NOW(3)").Set("last_used_ip = ?", ip).
		Where("id = ?", id).Exec(ctx)
	return err
}
