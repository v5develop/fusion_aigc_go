package session

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 端到端：真实 Redis（Java 同款 key）+ 真实 MySQL（Java 同款 RBAC 表）。
// 需要两个环境变量才会跑：ADMIN_E2E_REDIS=127.0.0.1:16390  ADMIN_E2E_MYSQL='root:pw@tcp(127.0.0.1:33307)/aigc_e2e?parseTime=true'
// 表结构只建这次用到的列，与 Java 迁移里的同名表一致。
func TestAdminSharedSession(t *testing.T) {
	addr, dsn := os.Getenv("ADMIN_E2E_REDIS"), os.Getenv("ADMIN_E2E_MYSQL")
	if addr == "" || dsn == "" {
		t.Skip("需要 ADMIN_E2E_REDIS 与 ADMIN_E2E_MYSQL")
	}
	ctx := context.Background()
	st := New(Conf{RedisAddr: addr, RedisDB: 0})
	conn := sqlx.NewMysql(dsn)

	for _, ddl := range []string{
		"DROP TABLE IF EXISTS afv_admin_role_permission, afv_admin_user_role, afv_admin_permission, afv_admin_role, afv_admin_user",
		"CREATE TABLE afv_admin_user (id BIGINT PRIMARY KEY, username VARCHAR(64), status TINYINT, deleted TINYINT)",
		"CREATE TABLE afv_admin_role (id BIGINT PRIMARY KEY, role_code VARCHAR(64), status TINYINT, deleted TINYINT)",
		"CREATE TABLE afv_admin_user_role (admin_id BIGINT, role_id BIGINT)",
		"CREATE TABLE afv_admin_permission (id BIGINT PRIMARY KEY, permission_code VARCHAR(128), deleted TINYINT)",
		"CREATE TABLE afv_admin_role_permission (role_id BIGINT, permission_id BIGINT)",
		"INSERT INTO afv_admin_user VALUES (1,'ops_view',1,0),(2,'ops_super',1,0),(3,'ops_disabled',0,0)",
		"INSERT INTO afv_admin_role VALUES (10,'operator',1,0),(20,'super_admin',1,0)",
		"INSERT INTO afv_admin_user_role VALUES (1,10),(2,20),(3,20)",
		"INSERT INTO afv_admin_permission VALUES (100,'local_life:view',0),(101,'local_life:update',0)",
		"INSERT INTO afv_admin_role_permission VALUES (10,100),(20,100),(20,101)",
	} {
		if _, err := conn.Exec(ddl); err != nil {
			t.Fatalf("准备数据失败 %q: %v", ddl, err)
		}
	}

	check := func(admin int64, code string, want bool) {
		t.Helper()
		got, err := AllowAdmin(ctx, conn, admin, code)
		if err != nil {
			t.Fatalf("AllowAdmin(%d,%s): %v", admin, code, err)
		}
		if got != want {
			t.Fatalf("AllowAdmin(%d,%s)=%v，应为 %v", admin, code, got, want)
		}
	}
	check(1, "local_life:view", true)    // operator：只有查看
	check(1, "local_life:update", false) // operator 不能改
	check(2, "local_life:update", true)  // super_admin 能改
	check(3, "local_life:view", false)   // 账号停用，即使角色有权限也拒绝

	// 与 Java 的 AdminTokenSession 同样的 JSON，key 为 fusion:admin:access:<token>
	rds := st.rds
	token := "0123456789abcdef0123456789abcdef"
	if err := rds.Set(ctx, "fusion:admin:access:"+token,
		`{"adminId":2,"username":"ops_super","roleCode":"super_admin","issuedAt":1700000000}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	c, err := st.ParseAdmin(ctx, token)
	if err != nil || c.AdminID != 2 || c.RoleCode != "super_admin" {
		t.Fatalf("ParseAdmin 结果不对: %+v, %v", c, err)
	}
	if _, err := st.ParseAdmin(ctx, "ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("不存在的令牌应为 ErrInvalid，实际 %v", err)
	}
	rds.Del(ctx, "fusion:admin:access:"+token)
}
