package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvImageTextNoteModel = (*customAfvImageTextNoteModel)(nil)

// 笔记状态
const (
	StatusPlanning   int64 = 0 // 构思中（执行面 74）
	StatusPlanned    int64 = 1 // 待确认（规划页）
	StatusPlanFailed int64 = 2
	StatusGenerating int64 = 3 // 生成中（执行面 75）
	StatusDone       int64 = 4
	StatusGenFailed  int64 = 5
)

type (
	// AfvImageTextNoteModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvImageTextNoteModel.
	AfvImageTextNoteModel interface {
		afvImageTextNoteModel
		withSession(session sqlx.Session) AfvImageTextNoteModel
		// FindOwned 未删除且属于该用户；userID 为 0 表示不校验归属（后台）。
		FindOwned(ctx context.Context, id, userID int64) (*AfvImageTextNote, error)
		FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvImageTextNote, error)
		List(ctx context.Context, f ListFilter) ([]*AfvImageTextNote, int64, error)
		// FinishPlan 构思中 → 待确认（写入构思结果）/ 构思失败。
		FinishPlan(ctx context.Context, id int64, p *Plan, failReason string) (bool, error)
		// Replan 待确认/构思失败 → 构思中，换新的构思任务（可同时改套图数量）。
		Replan(ctx context.Context, id int64, taskID string, imageCount int64) (bool, error)
		// StartGenerate 待确认/生成失败 → 生成中，同时保存用户在规划页改过的文字。
		StartGenerate(ctx context.Context, id int64, title, content, tags string, count int64, taskID string) (bool, error)
		// FinishGenerate 生成中 → 已完成/生成失败。
		FinishGenerate(ctx context.Context, id, to int64, imageURLs, failReason string) (bool, error)
		// UpdateContent 编辑已完成的笔记。
		UpdateContent(ctx context.Context, id int64, title, content, tags, imageURLs string) (bool, error)
		ListByStatus(ctx context.Context, status int64, before time.Time, limit int64) ([]*AfvImageTextNote, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvImageTextNoteModel struct {
		*defaultAfvImageTextNoteModel
	}

	ListFilter struct {
		UserID int64
		Status int64 // -1 表示不限
		Limit  int64
		Offset int64
	}

	// Plan 构思结果（74 返回）。Script 为原始 JSON，Tags 为 JSON 数组。
	Plan struct {
		Script, Title, Content, Tags string
	}
)

// NewAfvImageTextNoteModel returns a model for the database table.
func NewAfvImageTextNoteModel(conn sqlx.SqlConn) AfvImageTextNoteModel {
	return &customAfvImageTextNoteModel{
		defaultAfvImageTextNoteModel: newAfvImageTextNoteModel(conn),
	}
}

func (m *customAfvImageTextNoteModel) withSession(session sqlx.Session) AfvImageTextNoteModel {
	return NewAfvImageTextNoteModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvImageTextNoteModel) FindOwned(ctx context.Context, id, userID int64) (*AfvImageTextNote, error) {
	var resp AfvImageTextNote
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvImageTextNoteRows, m.table)
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

func (m *customAfvImageTextNoteModel) FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvImageTextNote, error) {
	return m.FindOneByUserIdRequestId(ctx, userID, sql.NullString{String: requestID, Valid: true})
}

func (m *customAfvImageTextNoteModel) List(ctx context.Context, f ListFilter) ([]*AfvImageTextNote, int64, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if f.UserID > 0 {
		conds, args = append(conds, "`user_id` = ?"), append(args, f.UserID)
	}
	if f.Status >= 0 {
		conds, args = append(conds, "`status` = ?"), append(args, f.Status)
	}
	where := " WHERE " + strings.Join(conds, " AND ")
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, fmt.Sprintf("SELECT COUNT(*) FROM %s%s", m.table, where), args...); err != nil {
		return nil, 0, err
	}
	var list []*AfvImageTextNote
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY `id` DESC LIMIT ? OFFSET ?", afvImageTextNoteRows, m.table, where)
	err := m.conn.QueryRowsCtx(ctx, &list, q, append(args, f.Limit, f.Offset)...)
	return list, total, err
}

func (m *customAfvImageTextNoteModel) FinishPlan(ctx context.Context, id int64, p *Plan, failReason string) (bool, error) {
	if p == nil {
		q := fmt.Sprintf("UPDATE %s SET `status` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?", m.table)
		return affected(m.conn.ExecCtx(ctx, q, StatusPlanFailed, nullString(failReason), id, StatusPlanning))
	}
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `plan_script` = ?, `title` = ?, `content` = ?, `tags` = ?, `fail_reason` = NULL "+
		"WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, StatusPlanned, p.Script, p.Title, p.Content, p.Tags, id, StatusPlanning))
}

func (m *customAfvImageTextNoteModel) Replan(ctx context.Context, id int64, taskID string, imageCount int64) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `plan_task_id` = ?, `image_count` = ?, `plan_script` = NULL, `fail_reason` = NULL "+
		"WHERE `id` = ? AND `status` IN (?, ?) AND `deleted` = 0", m.table)
	return affected(m.conn.ExecCtx(ctx, q, StatusPlanning, taskID, imageCount, id, StatusPlanned, StatusPlanFailed))
}

func (m *customAfvImageTextNoteModel) StartGenerate(ctx context.Context, id int64, title, content, tags string, count int64, taskID string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `title` = ?, `content` = ?, `tags` = ?, `image_count` = ?, `gen_task_id` = ?, `fail_reason` = NULL "+
		"WHERE `id` = ? AND `status` IN (?, ?) AND `deleted` = 0", m.table)
	return affected(m.conn.ExecCtx(ctx, q, StatusGenerating, title, content, tags, count, taskID, id, StatusPlanned, StatusGenFailed))
}

func (m *customAfvImageTextNoteModel) FinishGenerate(ctx context.Context, id, to int64, imageURLs, failReason string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `image_urls` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, to, nullString(imageURLs), nullString(failReason), id, StatusGenerating))
}

func (m *customAfvImageTextNoteModel) UpdateContent(ctx context.Context, id int64, title, content, tags, imageURLs string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `title` = ?, `content` = ?, `tags` = ?, `image_urls` = ? WHERE `id` = ? AND `status` = ? AND `deleted` = 0", m.table)
	return affected(m.conn.ExecCtx(ctx, q, title, content, tags, imageURLs, id, StatusDone))
}

func (m *customAfvImageTextNoteModel) ListByStatus(ctx context.Context, status int64, before time.Time, limit int64) ([]*AfvImageTextNote, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `status` = ? AND `deleted` = 0 AND `update_time` < ? ORDER BY `id` LIMIT ?", afvImageTextNoteRows, m.table)
	var list []*AfvImageTextNote
	return list, m.conn.QueryRowsCtx(ctx, &list, q, status, before, limit)
}

func (m *customAfvImageTextNoteModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1 WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}

func affected(r sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
