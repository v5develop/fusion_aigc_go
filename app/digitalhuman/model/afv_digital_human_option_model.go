package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 选项类型
const (
	OptionAge           = "AGE"
	OptionSkinTone      = "SKIN_TONE"
	OptionRole          = "ROLE"
	OptionVoiceCategory = "VOICE_CATEGORY"
	OptionPrompt        = "PROMPT" // 提示词模板，value 存完整模板
)

var _ AfvDigitalHumanOptionModel = (*customAfvDigitalHumanOptionModel)(nil)

type (
	// AfvDigitalHumanOptionModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvDigitalHumanOptionModel.
	AfvDigitalHumanOptionModel interface {
		afvDigitalHumanOptionModel
		withSession(session sqlx.Session) AfvDigitalHumanOptionModel
		// ListByType optionType 为空表示全部；onlyEnabled 供 App 端使用。
		ListByType(ctx context.Context, optionType string, onlyEnabled bool) ([]*AfvDigitalHumanOption, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvDigitalHumanOptionModel struct {
		*defaultAfvDigitalHumanOptionModel
	}
)

// NewAfvDigitalHumanOptionModel returns a model for the database table.
func NewAfvDigitalHumanOptionModel(conn sqlx.SqlConn) AfvDigitalHumanOptionModel {
	return &customAfvDigitalHumanOptionModel{
		defaultAfvDigitalHumanOptionModel: newAfvDigitalHumanOptionModel(conn),
	}
}

func (m *customAfvDigitalHumanOptionModel) withSession(session sqlx.Session) AfvDigitalHumanOptionModel {
	return NewAfvDigitalHumanOptionModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvDigitalHumanOptionModel) ListByType(ctx context.Context, optionType string, onlyEnabled bool) ([]*AfvDigitalHumanOption, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if optionType != "" {
		conds, args = append(conds, "`option_type` = ?"), append(args, optionType)
	}
	if onlyEnabled {
		conds = append(conds, "`status` = 1")
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY `option_type`, `sort_order`, `id`",
		afvDigitalHumanOptionRows, m.table, strings.Join(conds, " AND "))
	var list []*AfvDigitalHumanOption
	return list, m.conn.QueryRowsCtx(ctx, &list, q, args...)
}

// SoftDelete 逻辑删除：deleted_id 置为行 ID，唯一键因此释放，同 code 可重建（同现有表约定）。
func (m *customAfvDigitalHumanOptionModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1, `deleted_id` = `id` WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}
