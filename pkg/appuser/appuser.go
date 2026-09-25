// Package appuser 本服务的 App 用户：复用现有系统的 afv_app_user 表。
//
// 用户从联盟（xxlm）过来，落库口径完全照现有 Java 服务（见根目录《联盟用户注册登录流程与写表.md》的
// createUnionUserWithProfile）：username = union_<联盟用户ID>、login_source=UNION、data_source=2、
// source_app_key/external_user_id 成对写、密码是随机串的 BCrypt（本地登不进来），
// 并且**建号和建团队在一个事务里**（afv_team + afv_team_member），
// 这样同一个人在现有系统和本服务里是同一行、也有团队，两边都认。
//
// 配了 Java 服务的开放接口（见 WithRegistrar / pkg/fusion）时，建档直接交给它，
// 我们只负责回查——这样连口径都不用自己维护。下面那套写表是没配时的兜底。
package appuser

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"golang.org/x/crypto/bcrypt"
)

// StatusEnabled afv_app_user.status：1 启用、0 禁用
const StatusEnabled = 1

// roleOwner afv_team_member.role：1 拥有者
const roleOwner = 1

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
	Birthday       string
	Province       string
	City           string
	RealNameStatus string
	Roles          []string
	Status         string // 联盟侧原值：active / frozen / deactivating / deactivated
	Enabled        bool   // 上面那个状态是否算正常，用 Active() 判
}

// Registrar 把建档交给现有 Java 服务（它写同一张表，还会建团队等）。
// 没配就我们自己写，见 create()。
type Registrar interface {
	Enabled() bool
	// Register 建档/同步；人已经存在（并发撞键）也算成功。
	Register(ctx context.Context, p Profile) error
}

// Active 把联盟的状态映射成启用与否：frozen / deactivating / deactivated 是禁用，其余（含空）都算正常。
func Active(unionStatus string) bool {
	switch strings.TrimSpace(unionStatus) {
	case "frozen", "deactivating", "deactivated":
		return false
	default:
		return true
	}
}

type Model struct {
	conn      sqlx.SqlConn
	registrar Registrar

	appKeyOnce sync.Once
	appKey     string // 内置应用的 app_key，建号时写进 source_app_key
}

func New(conn sqlx.SqlConn) *Model { return &Model{conn: conn} }

// WithRegistrar 配了 Java 服务就让它建档，我们只负责回查。
func (m *Model) WithRegistrar(r Registrar) *Model {
	if r != nil && r.Enabled() {
		m.registrar = r
	}
	return m
}

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

// Upsert 有则更新资料并返回，没有则按联盟资料建一个（连同团队）。
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
	if err := m.register(ctx, p); err != nil {
		// 并发下可能刚被另一个请求建上（username / union_user_id 唯一）。
		// 撞键这条路径不补建团队——已有的那行本来就有。
		if again, ferr := m.FindByUnionID(ctx, p.UnionUserID); ferr == nil && again != nil {
			return again, nil
		}
		return nil, err
	}
	u, err = m.FindByUnionID(ctx, p.UnionUserID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("appuser: 建档后回查不到用户 %s", p.UnionUserID)
	}
	return u, nil
}

// register 建档：配了 Java 服务就交给它（它写同一张表），否则我们自己写。
func (m *Model) register(ctx context.Context, p Profile) error {
	if m.registrar != nil {
		return m.registrar.Register(ctx, p)
	}
	return m.create(ctx, p)
}

// create 建号 + 建团队，一个事务：Java 那边所有用户都有团队，少建会让 currentTeamId 取空。
func (m *Model) create(ctx context.Context, p Profile) error {
	if err := m.ready(); err != nil {
		return err
	}
	pwd, err := randomPassword()
	if err != nil {
		return err
	}
	status := StatusEnabled
	if !p.Enabled {
		status = 0
	}
	appKey := m.builtinAppKey(ctx)

	return m.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		q := "INSERT INTO `afv_app_user` (`username`,`password`,`nickname`,`avatar`,`phone`,`gender`,`birthday`," +
			"`province`,`city`,`real_name_status`,`source_app_key`,`external_user_id`,`union_user_id`," +
			"`login_source`,`union_bound`,`union_bound_time`,`data_source`,`union_nickname`,`union_avatar`,`status`)" +
			" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'UNION',1,?,2,?,?,?)"
		res, err := s.ExecCtx(ctx, q, "union_"+p.UnionUserID, pwd, nullStr(p.Nickname), nullStr(p.Avatar),
			nullStr(p.Phone), p.Gender, nullStr(p.Birthday), nullStr(p.Province), nullStr(p.City),
			nullStr(p.RealNameStatus), nullStr(appKey), p.UnionUserID, p.UnionUserID,
			time.Now(), nullStr(p.Nickname), nullStr(p.Avatar), status)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// 联盟没给昵称的，用「用户{id}」兜底（和 Java 一致，id 得先拿到才知道）
		nickname := p.Nickname
		if nickname == "" {
			nickname = fmt.Sprintf("用户%d", id)
			if _, err := s.ExecCtx(ctx, "UPDATE `afv_app_user` SET `nickname` = ? WHERE `id` = ?", nickname, id); err != nil {
				return err
			}
		}
		return createTeam(ctx, s, id, nickname)
	})
}

// createTeam 建个人团队：用户是拥有者。
func createTeam(ctx context.Context, s sqlx.Session, userID int64, nickname string) error {
	res, err := s.ExecCtx(ctx, "INSERT INTO `afv_team` (`name`,`owner_user_id`,`status`) VALUES (?,?,1)",
		teamName(nickname), userID)
	if err != nil {
		return err
	}
	teamID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = s.ExecCtx(ctx, "INSERT INTO `afv_team_member` (`team_id`,`user_id`,`role`,`status`,`join_time`)"+
		" VALUES (?,?,?,1,?)", teamID, userID, roleOwner, time.Now())
	return err
}

// teamName 团队名取昵称，afv_team.name 是 varchar(128) 且非空，按字符截断兜底。
func teamName(nickname string) string {
	r := []rune(strings.TrimSpace(nickname))
	if len(r) == 0 {
		return "我的团队"
	}
	if len(r) > 40 {
		r = r[:40]
	}
	return string(r)
}

// builtinAppKey 内置应用的 app_key（afv_open_app.is_builtin=1）。
// 它和 external_user_id 一起构成唯一键 uk_app_external_user，少写一个唯一键就等于没有。
func (m *Model) builtinAppKey(ctx context.Context) string {
	m.appKeyOnce.Do(func() {
		var key string
		q := "SELECT `app_key` FROM `afv_open_app` WHERE `is_builtin` = 1 AND `status` = 1 ORDER BY `id` LIMIT 1"
		if err := m.conn.QueryRowCtx(ctx, &key, q); err != nil {
			logx.Errorf("appuser: 查不到内置应用 app_key（source_app_key 只能留空，唯一键会失效）: %v", err)
			return
		}
		m.appKey = key
	})
	return m.appKey
}

// syncProfile 每次登录把联盟那边的资料同步过来，但**空值不覆盖**：
// 上游没设头像、昵称是很常见的，无条件写会把库里已有的资料抹成空。
// 状态只收紧不放开：联盟说禁用就禁用，联盟说正常不动本地（本地可能是运营手工禁的）。
//
// **只同步身份资料，资产一律不碰**：点数（afv_points_account / afv_points_batch）和
// 创作权益（afv_app_user_feature）的真相在我们库里，联盟那边根本不存——它 App 上显示余额
// 也是回头查我们。发点数只有一条合法路径：对方用签名调 /api/v1/union/point-recharges，
// 带 rechargeId 幂等键。哪天他们在用户信息里加了 points 之类的字段，这里也**不要**跟着写，
// 否则等于凭对方一句话发钱。
func (m *Model) syncProfile(ctx context.Context, id int64, p Profile) error {
	set := make([]string, 0, 10)
	args := make([]any, 0, 12)
	put := func(col string, v any) { set = append(set, "`"+col+"` = ?"); args = append(args, v) }

	if p.Nickname != "" {
		put("nickname", p.Nickname)
		put("union_nickname", p.Nickname)
	}
	if p.Avatar != "" {
		put("avatar", p.Avatar)
		put("union_avatar", p.Avatar)
	}
	if p.Phone != "" {
		put("phone", p.Phone)
	}
	if p.Gender > 0 { // 0 是「未知」，分不清是没给还是真选了未知，不覆盖
		put("gender", p.Gender)
	}
	if p.Birthday != "" {
		put("birthday", p.Birthday)
	}
	if p.Province != "" {
		put("province", p.Province)
	}
	if p.City != "" {
		put("city", p.City)
	}
	if p.RealNameStatus != "" {
		put("real_name_status", p.RealNameStatus)
	}
	if !p.Enabled {
		put("status", 0)
	}
	if len(set) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := m.conn.ExecCtx(ctx, "UPDATE `afv_app_user` SET "+strings.Join(set, ", ")+" WHERE `id` = ?", args...)
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

// CurrentTeamID 当前团队：该用户 afv_team_member 里 status=1 的记录，
// 按 team_id 升序、再按 id 升序取第一条——和 Java 的取法一致（会话里要带这个）。
// 没有团队时返回 0。
func (m *Model) CurrentTeamID(ctx context.Context, userID int64) (int64, error) {
	if err := m.ready(); err != nil {
		return 0, err
	}
	var id int64
	q := "SELECT `team_id` FROM `afv_team_member` WHERE `user_id` = ? AND `status` = 1 AND `deleted` = 0" +
		" ORDER BY `team_id`, `id` LIMIT 1"
	switch err := m.conn.QueryRowCtx(ctx, &id, q, userID); err {
	case nil, sqlx.ErrNotFound:
		return id, nil
	default:
		return 0, err
	}
}

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
