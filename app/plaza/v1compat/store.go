// Package v1compat creaft v1 兼容面的三个广场读接口（Go 复刻 Java，strangler 切流用）：
//
//	GET /api/v1/plaza-templates/list   ← Java CreaftV1SupportController#plaza      + CompatSupportService#plazaTemplates
//	GET /api/v1/plaza-templates/tags   ← Java CreaftV1SupportController#plazaTags
//	GET /api/v1/plaza-headlines        ← Java PlazaHeadlineController#list         + PlazaHeadlineService#listCurrent
//
// 口径：入参、响应字段、分页、排序、过滤、信封与 Java 逐字段同形（规范化 JSON 后相等），不改名、不删、不加；
// 响应由 pkg/v1wire 手写，不经 jsoncase 改写（X-Jsoncase: off）。与 Java 的差异只有 DESIGN 9.1 登记的「有意偏差」
// （见 docs/api/api-plaza.md「v1 兼容三接口」一节）。
//
// 数据只读：afv_compat_plaza_template、afv_plaza_headline、afv_agent_turn，本包没有任何写语句（单测断言）。
// 默认不注册（Plaza.V1Compat.Enabled=false），nginx 仍把三条路径指向 Java；切流步骤见文档。
//
// 设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 1.5-4、9.1、9.5、14.8。
package v1compat

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// TemplateRow afv_compat_plaza_template 的一行（v1 口径只用到这些列）。
type TemplateRow struct {
	ID          int64
	Title       sql.NullString
	Description sql.NullString
	TagsJSON    []byte // NULL → nil
	DataJSON    []byte // NULL → nil
}

// HeadlineRow afv_plaza_headline 的一行。
type HeadlineRow struct {
	ID              int64
	PlazaTemplateID sql.NullInt64
	StartDate       sql.NullString // yyyy-MM-dd
	EndDate         sql.NullString
	Why             sql.NullString
	SortOrder       sql.NullInt64
}

// UseCount 每个模板在 [from, mid) / [mid, to) 两个窗口的轮次数。
type UseCount struct {
	TemplateID   int64
	Uses         int64
	PreviousUses int64
}

// Source 只读数据源。
type Source interface {
	// EnabledTemplates 启用中的模板，按 sort_order ASC, update_time DESC, id DESC（CompatSupportService.java:216-220）。
	EnabledTemplates(ctx context.Context) ([]TemplateRow, error)
	// TemplatesByID 指定 id 中启用的模板（PlazaHeadlineService#loadTemplates(ids, true)）。
	TemplatesByID(ctx context.Context, ids []int64) ([]TemplateRow, error)
	// ActiveHeadlines today 落在 [start_date, end_date] 内、启用中的头条（PlazaHeadlineService.java:82-85）。
	ActiveHeadlines(ctx context.Context, today string) ([]HeadlineRow, error)
	// CountUses 复刻统计（PlazaHeadlineMapper#countTemplateUses），时间为 Asia/Shanghai 本地时间。
	CountUses(ctx context.Context, from, mid, to time.Time) ([]UseCount, error)
}

// SQLSource 基于 database/sql 的实现（与网关共用 DataSource 的连接池）。
type SQLSource struct{ DB *sql.DB }

const tplCols = "id, title, description, tags_json, data_json"

func (s SQLSource) EnabledTemplates(ctx context.Context) ([]TemplateRow, error) {
	return s.templates(ctx, "SELECT "+tplCols+" FROM afv_compat_plaza_template WHERE enabled = 1 AND deleted = 0 "+
		"ORDER BY sort_order ASC, update_time DESC, id DESC")
}

func (s SQLSource) TemplatesByID(ctx context.Context, ids []int64) ([]TemplateRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return s.templates(ctx, "SELECT "+tplCols+" FROM afv_compat_plaza_template WHERE id IN (?"+
		strings.Repeat(",?", len(ids)-1)+") AND enabled = 1 AND deleted = 0", args...)
}

func (s SQLSource) templates(ctx context.Context, q string, args ...any) ([]TemplateRow, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateRow
	for rows.Next() {
		var t TemplateRow
		if err := rows.Scan(&t.ID, &t.Title, &t.Description, &t.TagsJSON, &t.DataJSON); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s SQLSource) ActiveHeadlines(ctx context.Context, today string) ([]HeadlineRow, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id, plaza_template_id, DATE_FORMAT(start_date, '%Y-%m-%d'), "+
		"DATE_FORMAT(end_date, '%Y-%m-%d'), why, sort_order FROM afv_plaza_headline "+
		"WHERE enabled = 1 AND start_date <= ? AND end_date >= ? AND deleted = 0", today, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeadlineRow
	for rows.Next() {
		var h HeadlineRow
		if err := rows.Scan(&h.ID, &h.PlazaTemplateID, &h.StartDate, &h.EndDate, &h.Why, &h.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

const javaDateTime = "2006-01-02 15:04:05"

func (s SQLSource) CountUses(ctx context.Context, from, mid, to time.Time) ([]UseCount, error) {
	m := mid.Format(javaDateTime)
	rows, err := s.DB.QueryContext(ctx, "SELECT plaza_template_id, "+
		"SUM(CASE WHEN create_time >= ? THEN 1 ELSE 0 END) AS uses, "+
		"SUM(CASE WHEN create_time < ? THEN 1 ELSE 0 END) AS previous_uses "+
		"FROM afv_agent_turn WHERE plaza_template_id IS NOT NULL AND create_time >= ? AND create_time < ? "+
		"GROUP BY plaza_template_id", m, m, from.Format(javaDateTime), to.Format(javaDateTime))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UseCount
	for rows.Next() {
		var id sql.NullInt64
		var uses, prev sql.NullInt64
		if err := rows.Scan(&id, &uses, &prev); err != nil {
			return nil, err
		}
		if !id.Valid {
			continue
		}
		out = append(out, UseCount{TemplateID: id.Int64, Uses: uses.Int64, PreviousUses: prev.Int64})
	}
	return out, rows.Err()
}
