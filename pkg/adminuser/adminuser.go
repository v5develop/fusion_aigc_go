// Package adminuser 后台管理员：复用现有系统的 afv_admin_user / afv_admin_role。
//
// 账号是现成的（库里 10 个，密码是 BCrypt），我们只做「校验密码 → 发令牌」，
// 不自己建账号、不改密码——那些还在现有后台里做。
//
// 表上带了 login_fail_count / locked_until 两列，说明现有系统有「连错几次锁一会儿」的机制，
// 这边沿用同一套字段，两边都认得。
package adminuser

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"golang.org/x/crypto/bcrypt"
)

// 连错 maxFailures 次锁 lockFor；锁定期间即使密码对也不放行。
const (
	maxFailures = 5
	lockFor     = 15 * time.Minute
)

var (
	// ErrNoDB 没配数据库时后台登录直接拒绝，而不是崩掉
	ErrNoDB = errors.New("adminuser: 未配置数据库")
	// ErrWrong 用户名或密码不对。**不区分**是哪个，免得被拿来试探账号是否存在
	ErrWrong = errors.New("adminuser: 用户名或密码不正确")
	// ErrDisabled 账号被停用
	ErrDisabled = errors.New("adminuser: 账号已停用")
	// ErrLocked 连续失败次数过多，暂时锁定
	ErrLocked = errors.New("adminuser: 账号已锁定")
)

type Admin struct {
	Id       int64          `db:"id"`
	Username string         `db:"username"`
	RealName sql.NullString `db:"real_name"`
	Status   int64          `db:"status"`
	Roles    []string       `db:"-"` // 角色编码，登录后一起返回，前端据此显示菜单
}

type Model struct {
	conn sqlx.SqlConn
}

func New(conn sqlx.SqlConn) *Model { return &Model{conn: conn} }

func (m *Model) ready() error {
	if m == nil || m.conn == nil {
		return ErrNoDB
	}
	return nil
}

type row struct {
	Id             int64          `db:"id"`
	Username       string         `db:"username"`
	PasswordHash   string         `db:"password_hash"`
	RealName       sql.NullString `db:"real_name"`
	Status         int64          `db:"status"`
	LoginFailCount int64          `db:"login_fail_count"`
	LockedUntil    sql.NullTime   `db:"locked_until"`
}

const cols = "`id`,`username`,`password_hash`,`real_name`,`status`,`login_fail_count`,`locked_until`"

// Login 校验用户名密码，成功返回管理员（含角色）。
func (m *Model) Login(ctx context.Context, username, password, ip string) (*Admin, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	var r row
	q := "SELECT " + cols + " FROM `afv_admin_user` WHERE `username` = ? AND `deleted` = 0 LIMIT 1"
	switch err := m.conn.QueryRowCtx(ctx, &r, q, username); err {
	case nil:
	case sqlx.ErrNotFound:
		// 账号不存在也走一次 BCrypt，别让响应快慢暴露账号是否存在
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$"+
			"N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"), []byte(password))
		return nil, ErrWrong
	default:
		return nil, err
	}
	if r.LockedUntil.Valid && r.LockedUntil.Time.After(time.Now()) {
		return nil, ErrLocked
	}
	if r.Status != 1 {
		return nil, ErrDisabled
	}
	if err := bcrypt.CompareHashAndPassword([]byte(r.PasswordHash), []byte(password)); err != nil {
		if ferr := m.countFailure(ctx, r); ferr != nil {
			return nil, ferr
		}
		return nil, ErrWrong
	}
	if err := m.markSuccess(ctx, r.Id, ip); err != nil {
		return nil, err
	}
	roles, err := m.roles(ctx, r.Id)
	if err != nil {
		return nil, err
	}
	return &Admin{Id: r.Id, Username: r.Username, RealName: r.RealName, Status: r.Status, Roles: roles}, nil
}

// countFailure 记一次失败；到次数就锁一段时间。
func (m *Model) countFailure(ctx context.Context, r row) error {
	if r.LoginFailCount+1 >= maxFailures {
		_, err := m.conn.ExecCtx(ctx, "UPDATE `afv_admin_user` SET `login_fail_count` = 0, `locked_until` = ?"+
			" WHERE `id` = ?", time.Now().Add(lockFor), r.Id)
		return err
	}
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE `afv_admin_user` SET `login_fail_count` = `login_fail_count` + 1 WHERE `id` = ?", r.Id)
	return err
}

func (m *Model) markSuccess(ctx context.Context, id int64, ip string) error {
	_, err := m.conn.ExecCtx(ctx, "UPDATE `afv_admin_user` SET `login_fail_count` = 0, `locked_until` = NULL,"+
		" `last_login_time` = ?, `last_login_ip` = ? WHERE `id` = ?", time.Now(), nullStr(ip), id)
	return err
}

// FindByID 按管理员 id 查（带角色）；查不到或已停用返回 nil。
func (m *Model) FindByID(ctx context.Context, id int64) (*Admin, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	var r row
	q := "SELECT " + cols + " FROM `afv_admin_user` WHERE `id` = ? AND `deleted` = 0 LIMIT 1"
	switch err := m.conn.QueryRowCtx(ctx, &r, q, id); err {
	case nil:
	case sqlx.ErrNotFound:
		return nil, nil
	default:
		return nil, err
	}
	if r.Status != 1 {
		return nil, nil
	}
	roles, err := m.roles(ctx, r.Id)
	if err != nil {
		return nil, err
	}
	return &Admin{Id: r.Id, Username: r.Username, RealName: r.RealName, Status: r.Status, Roles: roles}, nil
}

// roles 取角色编码，如 super_admin / billing_admin / operator。
func (m *Model) roles(ctx context.Context, id int64) ([]string, error) {
	var out []string
	// 关联列叫 admin_id，不是 user_id
	q := "SELECT r.`role_code` FROM `afv_admin_user_role` ur JOIN `afv_admin_role` r ON r.`id` = ur.`role_id`" +
		" WHERE ur.`admin_id` = ? AND r.`status` = 1 AND r.`deleted` = 0 ORDER BY r.`id`"
	if err := m.conn.QueryRowsCtx(ctx, &out, q, id); err != nil {
		return nil, err
	}
	return out, nil
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
