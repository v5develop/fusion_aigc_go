package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvShopProfileModel = (*customAfvShopProfileModel)(nil)

type (
	// AfvShopProfileModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvShopProfileModel.
	AfvShopProfileModel interface {
		afvShopProfileModel
		withSession(session sqlx.Session) AfvShopProfileModel
		// FindOwned 未删除且属于该用户；userID 为 0 表示不校验归属（后台）。
		FindOwned(ctx context.Context, id, userID int64) (*AfvShopProfile, error)
		ListByUser(ctx context.Context, userID, limit, offset int64) ([]*AfvShopProfile, int64, error)
		CountByUser(ctx context.Context, userID int64) (int64, error)
		Save(ctx context.Context, data *AfvShopProfile) error
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvShopProfileModel struct {
		*defaultAfvShopProfileModel
	}
)

// NewAfvShopProfileModel returns a model for the database table.
func NewAfvShopProfileModel(conn sqlx.SqlConn) AfvShopProfileModel {
	return &customAfvShopProfileModel{
		defaultAfvShopProfileModel: newAfvShopProfileModel(conn),
	}
}

func (m *customAfvShopProfileModel) withSession(session sqlx.Session) AfvShopProfileModel {
	return NewAfvShopProfileModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvShopProfileModel) FindOwned(ctx context.Context, id, userID int64) (*AfvShopProfile, error) {
	var resp AfvShopProfile
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvShopProfileRows, m.table)
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

func (m *customAfvShopProfileModel) ListByUser(ctx context.Context, userID, limit, offset int64) ([]*AfvShopProfile, int64, error) {
	total, err := m.CountByUser(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	var list []*AfvShopProfile
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `user_id` = ? AND `deleted` = 0 ORDER BY `id` DESC LIMIT ? OFFSET ?", afvShopProfileRows, m.table)
	return list, total, m.conn.QueryRowsCtx(ctx, &list, q, userID, limit, offset)
}

func (m *customAfvShopProfileModel) CountByUser(ctx context.Context, userID int64) (int64, error) {
	var n int64
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE `user_id` = ? AND `deleted` = 0", m.table)
	return n, m.conn.QueryRowCtx(ctx, &n, q, userID)
}

// Save 更新可编辑的字段（不动 user_id、create_time）。
func (m *customAfvShopProfileModel) Save(ctx context.Context, data *AfvShopProfile) error {
	q := fmt.Sprintf("UPDATE %s SET `name` = ?, `open_date` = ?, `address` = ?, `products` = ?, "+
		"`selling_points` = ?, `self_title` = ?, `target_audience` = ? WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, data.Name, data.OpenDate, data.Address, data.Products,
		data.SellingPoints, data.SelfTitle, data.TargetAudience, data.Id)
	return err
}

func (m *customAfvShopProfileModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1 WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}
