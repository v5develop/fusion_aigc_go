package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 选项类型
const (
	OptionContentType = "CONTENT_TYPE"
	OptionStyle       = "STYLE"
	OptionIndustry    = "INDUSTRY"
	OptionWordCount   = "WORD_COUNT"
	OptionPrompt      = "PROMPT"    // 提示词模板，value 存完整模板
	OptionBlockWord   = "BLOCKWORD" // 敏感词，code 即词本身
)

var _ AfvCopywritingOptionModel = (*customAfvCopywritingOptionModel)(nil)

type (
	// AfvCopywritingOptionModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvCopywritingOptionModel.
	AfvCopywritingOptionModel interface {
		afvCopywritingOptionModel
		withSession(session sqlx.Session) AfvCopywritingOptionModel
		// ListByType optionType 为空表示全部；onlyEnabled 供 App 端使用。
		ListByType(ctx context.Context, optionType string, onlyEnabled bool) ([]*AfvCopywritingOption, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvCopywritingOptionModel struct {
		*defaultAfvCopywritingOptionModel
	}
)

// NewAfvCopywritingOptionModel returns a model for the database table.
func NewAfvCopywritingOptionModel(conn sqlx.SqlConn) AfvCopywritingOptionModel {
	return &customAfvCopywritingOptionModel{
		defaultAfvCopywritingOptionModel: newAfvCopywritingOptionModel(conn),
	}
}

func (m *customAfvCopywritingOptionModel) withSession(session sqlx.Session) AfvCopywritingOptionModel {
	return NewAfvCopywritingOptionModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvCopywritingOptionModel) ListByType(ctx context.Context, optionType string, onlyEnabled bool) ([]*AfvCopywritingOption, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if optionType != "" {
		conds, args = append(conds, "`option_type` = ?"), append(args, optionType)
	}
	if onlyEnabled {
		conds = append(conds, "`status` = 1")
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY `option_type`, `sort_order`, `id`",
		afvCopywritingOptionRows, m.table, strings.Join(conds, " AND "))
	var list []*AfvCopywritingOption
	return list, m.conn.QueryRowsCtx(ctx, &list, q, args...)
}

// SoftDelete 逻辑删除：deleted_id 置为行 ID，唯一键因此释放，同 code 可重建（同现有表约定）。
func (m *customAfvCopywritingOptionModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1, `deleted_id` = `id` WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}
