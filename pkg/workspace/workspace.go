// Package workspace 现有系统的「工作空间」（afv_workspace）。
//
// Java 的 V1.0.181 把素材的归属从 owner_type/owner_id 改成了 workspace_id：
// 每个用户注册时建一个个人空间（kind=PERSONAL），素材库按空间划可见范围。
// 我们两处要跟着它：建档时建个人空间；写素材库时填个人空间的 id。
// 口径照 Java 的 WorkspaceService.ensurePersonal：名字「{昵称}的个人空间」、
// 本人是 role=1 的成员、预置 4 个标签。
package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// DB 事务（sqlx.Session）和连接（sqlx.SqlConn）都满足。
type DB interface {
	ExecCtx(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowCtx(ctx context.Context, v any, query string, args ...any) error
}

// Java 的 DEFAULT_TAGS，建空间时一并预置。
var defaultTags = []string{"图片生成", "图片编辑", "视频生成", "设计"}

// PersonalID 用户的个人空间 id；没有返回 0。
func PersonalID(ctx context.Context, db DB, userID int64) (int64, error) {
	var id int64
	const q = "SELECT `id` FROM `afv_workspace` WHERE `owner_user_id` = ? AND `kind` = 'PERSONAL' AND `status` = 1 ORDER BY `id` LIMIT 1"
	err := db.QueryRowCtx(ctx, &id, q, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// EnsurePersonal 返回用户的个人空间 id，没有就建一个。
// displayName 留空时取 afv_app_user.nickname，再没有用「用户{id}」兜底（同 Java）。
// 要在事务里调：建空间是三张表（空间、成员、标签）。
func EnsurePersonal(ctx context.Context, db DB, userID int64, displayName string) (int64, error) {
	if id, err := PersonalID(ctx, db, userID); err != nil || id != 0 {
		return id, err
	}
	name := strings.TrimSpace(displayName)
	if name == "" {
		var nickname sql.NullString
		err := db.QueryRowCtx(ctx, &nickname, "SELECT `nickname` FROM `afv_app_user` WHERE `id` = ? AND `deleted` = 0", userID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		name = strings.TrimSpace(nickname.String)
	}
	if name == "" {
		name = fmt.Sprintf("用户%d", userID)
	}
	name = trimRunes(name, 240) + "的个人空间" // afv_workspace.name varchar(255)

	res, err := db.ExecCtx(ctx, "INSERT INTO `afv_workspace` (`name`,`owner_user_id`,`kind`,`status`) VALUES (?,?,'PERSONAL',1)", name, userID)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := db.ExecCtx(ctx, "INSERT INTO `afv_workspace_member` (`workspace_id`,`user_id`,`role`,`status`) VALUES (?,?,1,1)", id, userID); err != nil {
		return 0, err
	}
	for _, t := range defaultTags {
		if _, err := db.ExecCtx(ctx, "INSERT INTO `afv_workspace_tag` (`workspace_id`,`title`,`creator_user_id`) VALUES (?,?,?)", id, t, userID); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func trimRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
