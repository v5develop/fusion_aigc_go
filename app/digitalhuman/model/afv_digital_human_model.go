package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvDigitalHumanModel = (*customAfvDigitalHumanModel)(nil)

// 数字人状态
const (
	DhStatusDubbing   int64 = 0 // 配音中（TTS）
	DhStatusRendering int64 = 1 // 视频生成中（执行面 85）
	DhStatusDone      int64 = 2
	DhStatusFailed    int64 = 3
)

// 形象来源
const (
	PortraitUpload = "UPLOAD"
	PortraitPublic = "PUBLIC"
	PortraitAI     = "AI"
)

type (
	// AfvDigitalHumanModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvDigitalHumanModel.
	AfvDigitalHumanModel interface {
		afvDigitalHumanModel
		withSession(session sqlx.Session) AfvDigitalHumanModel
		// FindOwned 未删除且属于该用户；userID 为 0 表示不校验归属（后台）。
		FindOwned(ctx context.Context, id, userID int64) (*AfvDigitalHuman, error)
		FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvDigitalHuman, error)
		List(ctx context.Context, f DhFilter) ([]*AfvDigitalHuman, int64, error)
		// StartRendering 配音中 → 视频生成中，记下音频与视频任务。
		StartRendering(ctx context.Context, id int64, audioURL string, duration int64, videoTaskID string) (bool, error)
		// Finish 从 from 状态结束为完成/失败，返回是否由本次调用完成。
		Finish(ctx context.Context, id, from, to int64, videoURL, failReason string) (bool, error)
		// ListByStatus 取某状态下 update_time 早于 before 的记录（轮询与卡单回收用）。
		ListByStatus(ctx context.Context, status int64, before time.Time, limit int64) ([]*AfvDigitalHuman, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvDigitalHumanModel struct {
		*defaultAfvDigitalHumanModel
	}

	DhFilter struct {
		UserID int64
		Status int64 // -1 表示不限
		Limit  int64
		Offset int64
	}
)

// NewAfvDigitalHumanModel returns a model for the database table.
func NewAfvDigitalHumanModel(conn sqlx.SqlConn) AfvDigitalHumanModel {
	return &customAfvDigitalHumanModel{
		defaultAfvDigitalHumanModel: newAfvDigitalHumanModel(conn),
	}
}

func (m *customAfvDigitalHumanModel) withSession(session sqlx.Session) AfvDigitalHumanModel {
	return NewAfvDigitalHumanModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvDigitalHumanModel) FindOwned(ctx context.Context, id, userID int64) (*AfvDigitalHuman, error) {
	var resp AfvDigitalHuman
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvDigitalHumanRows, m.table)
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

func (m *customAfvDigitalHumanModel) FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvDigitalHuman, error) {
	return m.FindOneByUserIdRequestId(ctx, userID, sql.NullString{String: requestID, Valid: true})
}

func (m *customAfvDigitalHumanModel) List(ctx context.Context, f DhFilter) ([]*AfvDigitalHuman, int64, error) {
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
	var list []*AfvDigitalHuman
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY `id` DESC LIMIT ? OFFSET ?", afvDigitalHumanRows, m.table, where)
	err := m.conn.QueryRowsCtx(ctx, &list, q, append(args, f.Limit, f.Offset)...)
	return list, total, err
}

func (m *customAfvDigitalHumanModel) StartRendering(ctx context.Context, id int64, audioURL string, duration int64, videoTaskID string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `audio_url` = ?, `duration` = ?, `video_task_id` = ? WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, DhStatusRendering, audioURL, duration, videoTaskID, id, DhStatusDubbing))
}

func (m *customAfvDigitalHumanModel) Finish(ctx context.Context, id, from, to int64, videoURL, failReason string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `video_url` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, to, nullString(videoURL), nullString(failReason), id, from))
}

func (m *customAfvDigitalHumanModel) ListByStatus(ctx context.Context, status int64, before time.Time, limit int64) ([]*AfvDigitalHuman, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `status` = ? AND `deleted` = 0 AND `update_time` < ? ORDER BY `id` LIMIT ?", afvDigitalHumanRows, m.table)
	var list []*AfvDigitalHuman
	return list, m.conn.QueryRowsCtx(ctx, &list, q, status, before, limit)
}

func (m *customAfvDigitalHumanModel) SoftDelete(ctx context.Context, id int64) error {
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
