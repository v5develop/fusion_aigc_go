package model

import (
	"context"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SharedModel 读取现有系统的表。只做 SELECT。
type SharedModel struct {
	conn sqlx.SqlConn
}

func NewSharedModel(conn sqlx.SqlConn) *SharedModel {
	return &SharedModel{conn: conn}
}

// BlockWords 敏感词：与 AI文案共用 afv_copywriting_option 的 BLOCKWORD 词库（code 即词本身）。
func (m *SharedModel) BlockWords(ctx context.Context) ([]string, error) {
	var words []string
	q := "SELECT `code` FROM afv_copywriting_option WHERE `option_type` = 'BLOCKWORD' AND `status` = 1 AND `deleted` = 0"
	return words, m.conn.QueryRowsCtx(ctx, &words, q)
}
