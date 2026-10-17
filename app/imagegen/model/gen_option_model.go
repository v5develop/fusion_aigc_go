package model

import (
	"context"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// GenOption 本地生活生成选项（模型 / 比例 / 时长）的一行。编码只读，运营只能改启用、名称、排序与默认。
type GenOption struct {
	Id        int64  `db:"id"`
	Group     string `db:"option_group"`
	Code      string `db:"code"`
	Label     string `db:"label"`
	SortOrder int32  `db:"sort_order"`
	Enabled   bool   `db:"enabled"`
	IsDefault bool   `db:"is_default"`
}

// GenOptionModel 读写 afv_local_life_gen_option（本服务自有表，与 SharedModel 的只读约定分开）。
type GenOptionModel struct {
	conn sqlx.SqlConn
}

func NewGenOptionModel(conn sqlx.SqlConn) *GenOptionModel {
	return &GenOptionModel{conn: conn}
}

const genOptionCols = "id, option_group, code, label, sort_order, enabled, is_default"

// ListGenOptions 全部选项（含停用），按组、排序。
func (m *GenOptionModel) ListGenOptions(ctx context.Context) ([]*GenOption, error) {
	var list []*GenOption
	err := m.conn.QueryRowsCtx(ctx, &list,
		"SELECT "+genOptionCols+" FROM afv_local_life_gen_option ORDER BY option_group, sort_order, id")
	return list, err
}

// FindGenOption 按主键取一行；不存在返回 sqlx.ErrNotFound。
func (m *GenOptionModel) FindGenOption(ctx context.Context, id int64) (*GenOption, error) {
	var row GenOption
	err := m.conn.QueryRowCtx(ctx, &row, "SELECT "+genOptionCols+" FROM afv_local_life_gen_option WHERE id = ? LIMIT 1", id)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// SaveGenOption 写回一行；设为默认时先清掉同组的默认标记，两步在一个事务里完成。
func (m *GenOptionModel) SaveGenOption(ctx context.Context, row *GenOption) error {
	return m.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		if row.IsDefault {
			if _, err := s.ExecCtx(ctx,
				"UPDATE afv_local_life_gen_option SET is_default = 0 WHERE option_group = ? AND id <> ?",
				row.Group, row.Id); err != nil {
				return err
			}
		}
		_, err := s.ExecCtx(ctx,
			"UPDATE afv_local_life_gen_option SET label = ?, sort_order = ?, enabled = ?, is_default = ? WHERE id = ?",
			row.Label, row.SortOrder, row.Enabled, row.IsDefault, row.Id)
		return err
	})
}

// GenKind 生成类型的运营开关（目前只有模板必选）。没有行的类型回落 YAML。
type GenKind struct {
	KindCode         string `db:"kind_code"`
	TemplateRequired bool   `db:"template_required"`
}

// ListGenKinds 全部类型开关，按编码排序。
func (m *GenOptionModel) ListGenKinds(ctx context.Context) ([]*GenKind, error) {
	var list []*GenKind
	err := m.conn.QueryRowsCtx(ctx, &list,
		"SELECT kind_code, template_required FROM afv_local_life_gen_kind ORDER BY kind_code")
	return list, err
}

// SaveGenKind 写回一个类型的开关；行不存在时不新增（种子由迁移负责）。
func (m *GenOptionModel) SaveGenKind(ctx context.Context, row *GenKind) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE afv_local_life_gen_kind SET template_required = ? WHERE kind_code = ?",
		row.TemplateRequired, row.KindCode)
	return err
}
