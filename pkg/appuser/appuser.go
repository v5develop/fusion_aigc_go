// Package appuser 本服务的 App 用户：复用现有系统的 afv_app_user 表。
//
// 用户从联盟（xxlm）过来，按现有数据的约定落库：
// username = union_<联盟用户ID>、login_source=UNION、data_source=2、密码是随机串的 BCrypt（本地登不进来）。
// 这样同一个人在现有系统和本服务里是**同一行**，不会出两条记录。
package appuser

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"golang.org/x/crypto/bcrypt"
)

// StatusEnabled afv_app_user.status：1 启用、0 禁用
const StatusEnabled = 1

type User struct {
	Id            int64          `db:"id"`
	Username      string         `db:"username"`
	Nickname      sql.NullString `db:"nickname"`
	Avatar        sql.NullString `db:"avatar"`
	Phone         sql.NullString `db:"phone"`
	UnionUserId   sql.NullString `db:"union_user_id"`
	Status        int64          `db:"status"`
	RealNameState sql.NullString `db:"real_name_status"`
}

// Profile 联盟同步过来的资料（手机号是脱敏的）。
type Profile struct {
	UnionUserID    string
	Nickname       string
	Avatar         string
	Phone          string
	Gender         int32
	Province       string
	City           string
	RealNameStatus string
	Enabled        bool // 联盟侧状态是否正常（active）
}

type Model struct {
	conn sqlx.SqlConn
}

func New(conn sqlx.SqlConn) *Model { return &Model{conn: conn} }

// ErrNoDB 网关没配 DataSource 时（本地存储模式可能不配），登录相关接口直接拒绝而不是崩掉。
var ErrNoDB = errors.New("appuser: 未配置数据库")

func (m *Model) ready() error {
	if m == nil || m.conn == nil {
		return ErrNoDB
	}
	return nil
}

const cols = "`id`,`username`,`nickname`,`avatar`,`phone`,`union_user_id`,`status`,`real_name_status`"

func (m *Model) FindByUnionID(ctx context.Context, unionID string) (*User, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	var u User
	q := "SELECT " + cols + " FROM `afv_app_user` WHERE `union_user_id` = ? AND `deleted` = 0 LIMIT 1"
	switch err := m.conn.QueryRowCtx(ctx, &u, q, unionID); err {
	case nil:
		return &u, nil
	case sqlx.ErrNotFound:
		return nil, nil // 没有就是新用户
	default:
		return nil, err
	}
}

// Upsert 有则更新资料并返回，没有则按联盟资料建一个。
func (m *Model) Upsert(ctx context.Context, p Profile) (*User, error) {
	u, err := m.FindByUnionID(ctx, p.UnionUserID)
	if err != nil {
		return nil, err
	}
	if u != nil {
		if err := m.syncProfile(ctx, u.Id, p); err != nil {
			return nil, err
		}
		return m.FindByUnionID(ctx, p.UnionUserID)
	}
	if err := m.insert(ctx, p); err != nil {
		// 并发下可能刚被另一个请求建上（username / union_user_id 唯一）
		if again, ferr := m.FindByUnionID(ctx, p.UnionUserID); ferr == nil && again != nil {
			return again, nil
		}
		return nil, err
	}
	return m.FindByUnionID(ctx, p.UnionUserID)
}

func (m *Model) insert(ctx context.Context, p Profile) error {
	pwd, err := randomPassword()
	if err != nil {
		return err
	}
	status := StatusEnabled
	if !p.Enabled {
		status = 0
	}
	q := "INSERT INTO `afv_app_user` (`username`,`password`,`nickname`,`avatar`,`phone`,`gender`,`province`,`city`," +
		"`real_name_status`,`union_user_id`,`login_source`,`union_bound`,`union_bound_time`,`data_source`," +
		"`union_nickname`,`union_avatar`,`status`) VALUES (?,?,?,?,?,?,?,?,?,?,'UNION',1,?,2,?,?,?)"
	_, err = m.conn.ExecCtx(ctx, q, "union_"+p.UnionUserID, pwd, nullStr(p.Nickname), nullStr(p.Avatar), nullStr(p.Phone),
		p.Gender, nullStr(p.Province), nullStr(p.City), nullStr(p.RealNameStatus), p.UnionUserID,
		time.Now(), nullStr(p.Nickname), nullStr(p.Avatar), status)
	return err
}

// syncProfile 每次登录把联盟那边的昵称、头像等同步过来（用户在联盟改了资料，这边跟着变）。
func (m *Model) syncProfile(ctx context.Context, id int64, p Profile) error {
	q := "UPDATE `afv_app_user` SET `nickname` = ?, `avatar` = ?, `phone` = ?, `real_name_status` = ?, " +
		"`union_nickname` = ?, `union_avatar` = ? WHERE `id` = ?"
	_, err := m.conn.ExecCtx(ctx, q, nullStr(p.Nickname), nullStr(p.Avatar), nullStr(p.Phone),
		nullStr(p.RealNameStatus), nullStr(p.Nickname), nullStr(p.Avatar), id)
	return err
}

// randomPassword 随机串的 BCrypt：占住 NOT NULL 的密码列，同时保证没人能用密码登进来。
func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(b)), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("appuser: 生成密码失败: %w", err)
	}
	return string(h), nil
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// FindByID 按本服务的用户 id 查；查不到返回 nil。
func (m *Model) FindByID(ctx context.Context, id int64) (*User, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}
	var u User
	q := "SELECT " + cols + " FROM `afv_app_user` WHERE `id` = ? AND `deleted` = 0 LIMIT 1"
	switch err := m.conn.QueryRowCtx(ctx, &u, q, id); err {
	case nil:
		return &u, nil
	case sqlx.ErrNotFound:
		return nil, nil
	default:
		return nil, err
	}
}
