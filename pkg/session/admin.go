package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// AdminClaims 后台令牌：Java 后台写在 Redis 的 fusion:admin:access:<token>（JSON）。
// 字段名必须与 Java 的 AdminTokenSession 一致。Go 后台与 Java 后台共用这一份会话，不另起登录。
type AdminClaims struct {
	AdminID  int64  `json:"adminId"`
	Username string `json:"username"`
	RoleCode string `json:"roleCode"`
	IssuedAt int64  `json:"issuedAt"`
}

// ParseAdmin 按 Java 的同一套 key 读取后台会话。不存在或已过期返回 ErrInvalid。
func (s *Store) ParseAdmin(ctx context.Context, access string) (*AdminClaims, error) {
	raw, err := s.rds.Get(ctx, "fusion:admin:access:"+access).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	var c AdminClaims
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("session: 解析后台会话失败: %w", err)
	}
	if c.AdminID <= 0 {
		return nil, ErrInvalid
	}
	return &c, nil
}

// AllowAdmin 按 Java 的 RBAC 表判断：账号启用、角色启用、角色持有该权限码。
// 表与 Java 相同：afv_admin_user_role → afv_admin_role_permission → afv_admin_permission（均未逻辑删除）。
func AllowAdmin(ctx context.Context, conn sqlx.SqlConn, adminID int64, permissionCode string) (bool, error) {
	const q = "SELECT COUNT(*) FROM afv_admin_user u " +
		"JOIN afv_admin_user_role ur ON ur.admin_id = u.id " +
		"JOIN afv_admin_role r ON r.id = ur.role_id AND r.deleted = 0 AND r.status = 1 " +
		"JOIN afv_admin_role_permission rp ON rp.role_id = r.id " +
		"JOIN afv_admin_permission p ON p.id = rp.permission_id AND p.deleted = 0 " +
		"WHERE u.id = ? AND u.deleted = 0 AND u.status = 1 AND p.permission_code = ?"
	var n int64
	if err := conn.QueryRowCtx(ctx, &n, q, adminID, permissionCode); err != nil {
		return false, err
	}
	return n > 0, nil
}
