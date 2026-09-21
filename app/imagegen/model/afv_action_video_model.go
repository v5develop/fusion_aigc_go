package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvActionVideoModel = (*customAfvActionVideoModel)(nil)

type (
	// AfvActionVideoModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvActionVideoModel.
	AfvActionVideoModel interface {
		afvActionVideoModel
		withSession(session sqlx.Session) AfvActionVideoModel
		ListByUser(ctx context.Context, userID int64) ([]*AfvActionVideo, error)
		FindOwned(ctx context.Context, id, userID int64) (*AfvActionVideo, error)
		// CountByUser 用来给「我的动作N」编号
		CountByUser(ctx context.Context, userID int64) (int64, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvActionVideoModel struct {
		*defaultAfvActionVideoModel
	}
)

// NewAfvActionVideoModel returns a model for the database table.
func NewAfvActionVideoModel(conn sqlx.SqlConn) AfvActionVideoModel {
	return &customAfvActionVideoModel{
		defaultAfvActionVideoModel: newAfvActionVideoModel(conn),
	}
}

func (m *customAfvActionVideoModel) withSession(session sqlx.Session) AfvActionVideoModel {
	return NewAfvActionVideoModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvActionVideoModel) ListByUser(ctx context.Context, userID int64) ([]*AfvActionVideo, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `user_id` = ? AND `deleted` = 0 ORDER BY `id` DESC", afvActionVideoRows, m.table)
	var list []*AfvActionVideo
	return list, m.conn.QueryRowsCtx(ctx, &list, q, userID)
}

func (m *customAfvActionVideoModel) FindOwned(ctx context.Context, id, userID int64) (*AfvActionVideo, error) {
	var v AfvActionVideo
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvActionVideoRows, m.table)
	switch err := m.conn.QueryRowCtx(ctx, &v, q, id); err {
	case nil:
		if userID != 0 && v.UserId != userID {
			return nil, ErrNotFound
		}
		return &v, nil
	case sqlx.ErrNotFound:
		return nil, ErrNotFound
	default:
		return nil, err
	}
}

func (m *customAfvActionVideoModel) CountByUser(ctx context.Context, userID int64) (int64, error) {
	var n int64
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE `user_id` = ?", m.table)
	return n, m.conn.QueryRowCtx(ctx, &n, q, userID)
}

func (m *customAfvActionVideoModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1 WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}
