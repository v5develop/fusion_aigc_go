package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvImageCreationModel = (*customAfvImageCreationModel)(nil)

// 类型
const (
	KindPoster         = "poster"
	KindLongImage      = "longImage"
	KindActionTransfer = "actionTransfer"
	KindVoiceover      = "voiceover" // 数字人口播
)

// 状态
const (
	StatusGenerating int64 = 0
	StatusDone       int64 = 1 // 含部分成功
	StatusFailed     int64 = 2
)

type (
	// AfvImageCreationModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvImageCreationModel.
	AfvImageCreationModel interface {
		afvImageCreationModel
		withSession(session sqlx.Session) AfvImageCreationModel
		// FindOwned 未删除且属于该用户；userID 为 0 表示不校验归属（后台）。
		FindOwned(ctx context.Context, id, userID int64) (*AfvImageCreation, error)
		FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvImageCreation, error)
		List(ctx context.Context, f ListFilter) ([]*AfvImageCreation, int64, error)
		// UpdateTasks 回写各张图的任务进展；status 为 StatusGenerating 时表示还没结束。
		UpdateTasks(ctx context.Context, id int64, tasks string, status int64, imageURLs, failReason string) (bool, error)
		ListGenerating(ctx context.Context, before time.Time, limit int64) ([]*AfvImageCreation, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvImageCreationModel struct {
		*defaultAfvImageCreationModel
	}

	ListFilter struct {
		UserID int64
		Kind   string
		Status int64 // -1 表示不限
		Limit  int64
		Offset int64
	}
)

// NewAfvImageCreationModel returns a model for the database table.
func NewAfvImageCreationModel(conn sqlx.SqlConn) AfvImageCreationModel {
	return &customAfvImageCreationModel{
		defaultAfvImageCreationModel: newAfvImageCreationModel(conn),
	}
}

func (m *customAfvImageCreationModel) withSession(session sqlx.Session) AfvImageCreationModel {
	return NewAfvImageCreationModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvImageCreationModel) FindOwned(ctx context.Context, id, userID int64) (*AfvImageCreation, error) {
	var resp AfvImageCreation
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvImageCreationRows, m.table)
	switch err := m.conn.QueryRowCtx(ctx, &resp, q, id); err {
	case nil:
		if userID != 0 && resp.UserId != userID {
			return nil, ErrNotFound
		}
		return &resp, nil
	case sqlx.ErrNotFound:
		return nil, ErrNotFound
	default:
		return nil, err
	}
}

func (m *customAfvImageCreationModel) FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvImageCreation, error) {
	return m.FindOneByUserIdRequestId(ctx, userID, sql.NullString{String: requestID, Valid: true})
}

func (m *customAfvImageCreationModel) List(ctx context.Context, f ListFilter) ([]*AfvImageCreation, int64, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if f.UserID > 0 {
		conds, args = append(conds, "`user_id` = ?"), append(args, f.UserID)
	}
	if f.Kind != "" {
		conds, args = append(conds, "`kind` = ?"), append(args, f.Kind)
	}
	if f.Status >= 0 {
		conds, args = append(conds, "`status` = ?"), append(args, f.Status)
	}
	where := " WHERE " + strings.Join(conds, " AND ")
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, fmt.Sprintf("SELECT COUNT(*) FROM %s%s", m.table, where), args...); err != nil {
		return nil, 0, err
	}
	var list []*AfvImageCreation
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY `id` DESC LIMIT ? OFFSET ?", afvImageCreationRows, m.table, where)
	err := m.conn.QueryRowsCtx(ctx, &list, q, append(args, f.Limit, f.Offset)...)
	return list, total, err
}

func (m *customAfvImageCreationModel) UpdateTasks(ctx context.Context, id int64, tasks string, status int64, imageURLs, failReason string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `tasks` = ?, `status` = ?, `image_urls` = ?, `fail_reason` = ? "+
		"WHERE `id` = ? AND `status` = ?", m.table)
	r, err := m.conn.ExecCtx(ctx, q, tasks, status, nullString(imageURLs), nullString(failReason), id, StatusGenerating)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

func (m *customAfvImageCreationModel) ListGenerating(ctx context.Context, before time.Time, limit int64) ([]*AfvImageCreation, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `status` = ? AND `update_time` < ? ORDER BY `id` LIMIT ?", afvImageCreationRows, m.table)
	var list []*AfvImageCreation
	return list, m.conn.QueryRowsCtx(ctx, &list, q, StatusGenerating, before, limit)
}

func (m *customAfvImageCreationModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1 WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
