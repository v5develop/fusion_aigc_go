package model

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvDigitalHumanPortraitModel = (*customAfvDigitalHumanPortraitModel)(nil)

// AI定制形象状态
const (
	PortraitGenerating int64 = 0
	PortraitDone       int64 = 1
	PortraitFailed     int64 = 2
)

type (
	// AfvDigitalHumanPortraitModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvDigitalHumanPortraitModel.
	AfvDigitalHumanPortraitModel interface {
		afvDigitalHumanPortraitModel
		withSession(session sqlx.Session) AfvDigitalHumanPortraitModel
		FindOwned(ctx context.Context, id, userID int64) (*AfvDigitalHumanPortrait, error)
		FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvDigitalHumanPortrait, error)
		// Finish 生成中 → 完成/失败，返回是否由本次调用完成。imageURLs 为 JSON 数组。
		Finish(ctx context.Context, id, to int64, imageURLs, failReason string) (bool, error)
		ListGenerating(ctx context.Context, before time.Time, limit int64) ([]*AfvDigitalHumanPortrait, error)
	}

	customAfvDigitalHumanPortraitModel struct {
		*defaultAfvDigitalHumanPortraitModel
	}
)

// NewAfvDigitalHumanPortraitModel returns a model for the database table.
func NewAfvDigitalHumanPortraitModel(conn sqlx.SqlConn) AfvDigitalHumanPortraitModel {
	return &customAfvDigitalHumanPortraitModel{
		defaultAfvDigitalHumanPortraitModel: newAfvDigitalHumanPortraitModel(conn),
	}
}

func (m *customAfvDigitalHumanPortraitModel) withSession(session sqlx.Session) AfvDigitalHumanPortraitModel {
	return NewAfvDigitalHumanPortraitModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvDigitalHumanPortraitModel) FindOwned(ctx context.Context, id, userID int64) (*AfvDigitalHumanPortrait, error) {
	var resp AfvDigitalHumanPortrait
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvDigitalHumanPortraitRows, m.table)
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

func (m *customAfvDigitalHumanPortraitModel) FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvDigitalHumanPortrait, error) {
	return m.FindOneByUserIdRequestId(ctx, userID, sql.NullString{String: requestID, Valid: true})
}

func (m *customAfvDigitalHumanPortraitModel) Finish(ctx context.Context, id, to int64, imageURLs, failReason string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `image_urls` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, to, nullString(imageURLs), nullString(failReason), id, PortraitGenerating))
}

func (m *customAfvDigitalHumanPortraitModel) ListGenerating(ctx context.Context, before time.Time, limit int64) ([]*AfvDigitalHumanPortrait, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `status` = ? AND `deleted` = 0 AND `update_time` < ? ORDER BY `id` LIMIT ?", afvDigitalHumanPortraitRows, m.table)
	var list []*AfvDigitalHumanPortrait
	return list, m.conn.QueryRowsCtx(ctx, &list, q, PortraitGenerating, before, limit)
}
