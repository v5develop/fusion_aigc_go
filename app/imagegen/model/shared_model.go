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

// TemplateSlot 模板在模板素材投放位里的位置。
type TemplateSlot struct {
	BizType, Module, Slot string
}

// Template 一张模板：复用现有模板素材表，运营在现有后台「模板」里维护。
// 动作迁移的预置动作视频也走这里（type=video）。
type Template struct {
	Id        int64  `db:"id"`
	Title     string `db:"title"`
	Url       string `db:"url"`
	Type      string `db:"type"`
	SortOrder int64  `db:"sort_order"`
}

// FilterOption 模板筛选维度的一个选项（如海报类型：社媒 / 营销 …）。
type FilterOption struct {
	Code string `db:"option_code"`
	Name string `db:"option_name"`
}

const templateSelect = "SELECT a.id, a.title, a.url, a.type, p.sort_order " +
	"FROM afv_public_template_asset_placement p " +
	"JOIN afv_public_template_assets a ON a.id = p.asset_id AND a.deleted = 0 " +
	"WHERE p.biz_type = ? AND p.module_code = ? AND p.slot_code = ? AND p.status = 1 AND p.deleted = 0"

// ListTemplates 上架中的模板；optionCode 非空时只取打了该标签的（如海报类型）。
func (m *SharedModel) ListTemplates(ctx context.Context, s TemplateSlot, filterCode, optionCode string) ([]*Template, error) {
	q, args := templateSelect, []any{s.BizType, s.Module, s.Slot}
	if optionCode != "" {
		q += " AND EXISTS (SELECT 1 FROM afv_public_template_asset_filter af " +
			"JOIN afv_public_template_filter_option o ON o.id = af.option_id AND o.deleted = 0 " +
			"JOIN afv_public_template_filter f ON f.id = o.filter_id AND f.deleted = 0 " +
			"WHERE af.asset_id = a.id AND f.biz_type = ? AND f.filter_code = ? AND o.option_code = ?)"
		args = append(args, s.BizType, filterCode, optionCode)
	}
	var list []*Template
	return list, m.conn.QueryRowsCtx(ctx, &list, q+" ORDER BY p.sort_order, p.id", args...)
}

// FindTemplate 取一张上架中的模板；已下架或不在该投放位返回 ErrNotFound。
func (m *SharedModel) FindTemplate(ctx context.Context, s TemplateSlot, id int64) (*Template, error) {
	var t Template
	switch err := m.conn.QueryRowCtx(ctx, &t, templateSelect+" AND a.id = ? LIMIT 1", s.BizType, s.Module, s.Slot, id); err {
	case nil:
		return &t, nil
	case sqlx.ErrNotFound:
		return nil, ErrNotFound
	default:
		return nil, err
	}
}

// ListFilterOptions 某个筛选维度下启用中的选项（海报类型标签）。
func (m *SharedModel) ListFilterOptions(ctx context.Context, bizType, filterCode string) ([]*FilterOption, error) {
	q := "SELECT o.option_code, o.option_name FROM afv_public_template_filter f " +
		"JOIN afv_public_template_filter_option o ON o.filter_id = f.id AND o.status = 1 AND o.deleted = 0 " +
		"WHERE f.biz_type = ? AND f.filter_code = ? AND f.status = 1 AND f.deleted = 0 " +
		"ORDER BY o.sort_order, o.id"
	var list []*FilterOption
	return list, m.conn.QueryRowsCtx(ctx, &list, q, bizType, filterCode)
}
