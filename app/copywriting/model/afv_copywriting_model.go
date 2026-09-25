package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvCopywritingModel = (*customAfvCopywritingModel)(nil)

type (
	// AfvCopywritingModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvCopywritingModel.
	AfvCopywritingModel interface {
		afvCopywritingModel
		withSession(session sqlx.Session) AfvCopywritingModel
		ListByUser(ctx context.Context, f ListFilter) ([]*AfvCopywriting, int64, error)
		// FindOwned 查未删除且属于该用户的文案；userID 为 0 表示不校验归属（后台）。
		FindOwned(ctx context.Context, id, userID int64) (*AfvCopywriting, error)
		// FinishGenerate 生成中 → 完成/失败，返回是否由本次调用完成。
		FinishGenerate(ctx context.Context, id int64, status int64, title, body string, wordCount int64, failReason string) (bool, error)
		UpdateContent(ctx context.Context, id int64, title, body string, wordCount int64) error
		SoftDelete(ctx context.Context, id int64) error
		// FindByRequestID 幂等：同一用户的同一 request_id 只生成一次。
		FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvCopywriting, error)
		// ListStuck 取卡在「生成中」且超过 before 未更新的记录（服务重启或崩溃留下的）。
		ListStuck(ctx context.Context, before time.Time, limit int64) ([]*AfvCopywriting, error)
	}

	customAfvCopywritingModel struct {
		*defaultAfvCopywritingModel
	}
)

// NewAfvCopywritingModel returns a model for the database table.
func NewAfvCopywritingModel(conn sqlx.SqlConn) AfvCopywritingModel {
	return &customAfvCopywritingModel{
		defaultAfvCopywritingModel: newAfvCopywritingModel(conn),
	}
}

func (m *customAfvCopywritingModel) withSession(session sqlx.Session) AfvCopywritingModel {
	return NewAfvCopywritingModel(sqlx.NewSqlConnFromSession(session))
}

// 状态取值
const (
	StatusGenerating int64 = 0
	StatusDone       int64 = 1
	StatusFailed     int64 = 2
)

// 来源取值
const (
	SourceAiWrite = "AI_WRITE"
	SourceImitate = "IMITATE"
	SourceManual  = "MANUAL"
)

type ListFilter struct {
	UserID      int64
	Keyword     string
	ContentType string
	Status      int64 // -1 表示不限
	Limit       int64
	Offset      int64
}

func (m *customAfvCopywritingModel) ListByUser(ctx context.Context, f ListFilter) ([]*AfvCopywriting, int64, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if f.UserID > 0 {
		conds, args = append(conds, "`user_id` = ?"), append(args, f.UserID)
	}
	if f.Status >= 0 {
		conds, args = append(conds, "`status` = ?"), append(args, f.Status)
	}
	if f.ContentType != "" {
		conds, args = append(conds, "`content_type` = ?"), append(args, f.ContentType)
	}
	if f.Keyword != "" {
		kw := "%" + f.Keyword + "%"
		conds, args = append(conds, "(`title` LIKE ? OR `body` LIKE ?)"), append(args, kw, kw)
	}
	where := " WHERE " + strings.Join(conds, " AND ")

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, fmt.Sprintf("SELECT COUNT(*) FROM %s%s", m.table, where), args...); err != nil {
		return nil, 0, err
	}
	var list []*AfvCopywriting
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY `id` DESC LIMIT ? OFFSET ?", afvCopywritingRows, m.table, where)
	err := m.conn.QueryRowsCtx(ctx, &list, q, append(args, f.Limit, f.Offset)...)
	return list, total, err
}

func (m *customAfvCopywritingModel) FindOwned(ctx context.Context, id, userID int64) (*AfvCopywriting, error) {
	var resp AfvCopywriting
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvCopywritingRows, m.table)
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

func (m *customAfvCopywritingModel) FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvCopywriting, error) {
	var resp AfvCopywriting
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `user_id` = ? AND `request_id` = ? LIMIT 1", afvCopywritingRows, m.table)
	switch err := m.conn.QueryRowCtx(ctx, &resp, q, userID, requestID); err {
	case nil:
		return &resp, nil
	case sqlx.ErrNotFound:
		return nil, ErrNotFound
	default:
		return nil, err
	}
}

func (m *customAfvCopywritingModel) ListStuck(ctx context.Context, before time.Time, limit int64) ([]*AfvCopywriting, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `status` = ? AND `update_time` < ? ORDER BY `id` LIMIT ?", afvCopywritingRows, m.table)
	var list []*AfvCopywriting
	return list, m.conn.QueryRowsCtx(ctx, &list, q, StatusGenerating, before, limit)
}

func (m *customAfvCopywritingModel) FinishGenerate(ctx context.Context, id int64, status int64, title, body string, wordCount int64, failReason string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `title` = ?, `body` = ?, `word_count` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?", m.table)
	r, err := m.conn.ExecCtx(ctx, q, status, title, body, wordCount, sql.NullString{String: failReason, Valid: failReason != ""}, id, StatusGenerating)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

func (m *customAfvCopywritingModel) UpdateContent(ctx context.Context, id int64, title, body string, wordCount int64) error {
	q := fmt.Sprintf("UPDATE %s SET `title` = ?, `body` = ?, `word_count` = ? WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, title, body, wordCount, id)
	return err
}

func (m *customAfvCopywritingModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1 WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}
