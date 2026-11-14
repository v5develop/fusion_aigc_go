package plaza

import (
	"net/http/httptest"
	"testing"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/pkg/adminuser"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// L15：审计 operator_name 从管理员表取（real_name 优先，其次 username）；查不到 / 未装配留空。
func TestOperatorName(t *testing.T) {
	r := httptest.NewRequest("PUT", "/api/v1/plaza/admin/templates/1", nil)
	if got := (&handlers{c: &svc.ServiceContext{}}).operatorName(r, 7); got != "" {
		t.Fatalf("未装配 AdminUser 应留空: %q", got)
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &handlers{c: &svc.ServiceContext{AdminUser: adminuser.New(sqlx.NewSqlConnFromDB(db))}}
	cols := []string{"id", "username", "password_hash", "real_name", "status", "login_fail_count", "locked_until"}
	mock.ExpectQuery("FROM `afv_admin_user`").WillReturnRows(sqlmock.NewRows(cols).AddRow(7, "ops01", "x", "张三", 1, 0, nil))
	mock.ExpectQuery("afv_admin_user_role").WillReturnRows(sqlmock.NewRows([]string{"role_code"}))
	if got := h.operatorName(r, 7); got != "张三" {
		t.Fatalf("real_name 优先: %q", got)
	}
	mock.ExpectQuery("FROM `afv_admin_user`").WillReturnRows(sqlmock.NewRows(cols).AddRow(8, "ops02", "x", nil, 1, 0, nil))
	mock.ExpectQuery("afv_admin_user_role").WillReturnRows(sqlmock.NewRows([]string{"role_code"}))
	if got := h.operatorName(r, 8); got != "ops02" {
		t.Fatalf("无 real_name 用 username: %q", got)
	}
	if got := h.operatorName(r, 0); got != "" {
		t.Fatalf("无 id 留空: %q", got)
	}
}
