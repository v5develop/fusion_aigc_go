package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvDigitalHumanVoiceModel = (*customAfvDigitalHumanVoiceModel)(nil)

type (
	// AfvDigitalHumanVoiceModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvDigitalHumanVoiceModel.
	AfvDigitalHumanVoiceModel interface {
		afvDigitalHumanVoiceModel
		withSession(session sqlx.Session) AfvDigitalHumanVoiceModel
		// List category 为空表示全部；onlyEnabled 供 App 端使用。
		List(ctx context.Context, category string, onlyEnabled bool) ([]*AfvDigitalHumanVoice, error)
		// FindAlive 未删除的音色（含停用，由调用方判断）。
		FindAlive(ctx context.Context, id int64) (*AfvDigitalHumanVoice, error)
		UpdatePreview(ctx context.Context, id int64, url string) error
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvDigitalHumanVoiceModel struct {
		*defaultAfvDigitalHumanVoiceModel
	}
)

// NewAfvDigitalHumanVoiceModel returns a model for the database table.
func NewAfvDigitalHumanVoiceModel(conn sqlx.SqlConn) AfvDigitalHumanVoiceModel {
	return &customAfvDigitalHumanVoiceModel{
		defaultAfvDigitalHumanVoiceModel: newAfvDigitalHumanVoiceModel(conn),
	}
}

func (m *customAfvDigitalHumanVoiceModel) withSession(session sqlx.Session) AfvDigitalHumanVoiceModel {
	return NewAfvDigitalHumanVoiceModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvDigitalHumanVoiceModel) List(ctx context.Context, category string, onlyEnabled bool) ([]*AfvDigitalHumanVoice, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if category != "" {
		conds, args = append(conds, "JSON_CONTAINS(`categories`, JSON_QUOTE(?))"), append(args, category)
	}
	if onlyEnabled {
		conds = append(conds, "`status` = 1")
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY `sort_order`, `id`",
		afvDigitalHumanVoiceRows, m.table, strings.Join(conds, " AND "))
	var list []*AfvDigitalHumanVoice
	return list, m.conn.QueryRowsCtx(ctx, &list, q, args...)
}

func (m *customAfvDigitalHumanVoiceModel) FindAlive(ctx context.Context, id int64) (*AfvDigitalHumanVoice, error) {
	var resp AfvDigitalHumanVoice
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvDigitalHumanVoiceRows, m.table)
	switch err := m.conn.QueryRowCtx(ctx, &resp, q, id); err {
	case nil:
		return &resp, nil
	case sqlx.ErrNotFound:
		return nil, ErrNotFound
	default:
		return nil, err
	}
}

func (m *customAfvDigitalHumanVoiceModel) UpdatePreview(ctx context.Context, id int64, url string) error {
	q := fmt.Sprintf("UPDATE %s SET `preview_url` = ? WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, url, id)
	return err
}

// SoftDelete 逻辑删除：deleted_id 置为行 ID，唯一键因此释放，同 voice 可重建。
func (m *customAfvDigitalHumanVoiceModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1, `deleted_id` = `id` WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}
