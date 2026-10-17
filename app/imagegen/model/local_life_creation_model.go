package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ LocalLifeCreationModel = (*customLocalLifeCreationModel)(nil)

// 类型
const (
	KindPoster         = "poster"
	KindLongImage      = "longImage"
	KindActionTransfer = "actionTransfer"
	KindVoiceover      = "voiceover" // 数字人口播
)

// 状态
const (
	StatusGenerating int64 = 0
	StatusDone       int64 = 1 // 含部分成功
	StatusFailed     int64 = 2
)

type (
	// LocalLifeCreationModel is an interface to be customized, add more methods here,
	// and implement the added methods in customLocalLifeCreationModel.
	LocalLifeCreationModel interface {
		localLifeCreationModel
		withSession(session sqlx.Session) LocalLifeCreationModel
		// FindOwned 未删除且属于该用户；userID 为 0 表示不校验归属（后台）。
		FindOwned(ctx context.Context, id, userID int64) (*LocalLifeCreation, error)
		FindByRequestID(ctx context.Context, userID int64, requestID string) (*LocalLifeCreation, error)
		List(ctx context.Context, f ListFilter) ([]*LocalLifeCreation, int64, error)
		// UpdateTasks 回写各张图的任务进展；status 为 StatusGenerating 时表示还没结束。
		UpdateTasks(ctx context.Context, id int64, tasks string, status int64, imageURLs, failReason string) (bool, error)
		ListGenerating(ctx context.Context, before time.Time, limit int64) ([]*LocalLifeCreation, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customLocalLifeCreationModel struct {
		*defaultLocalLifeCreationModel
	}

	ListFilter struct {
		UserID int64
		Kind   string
		Media  string // image / video，空=不限
		Status int64  // -1 表示不限
		Limit  int64
		Offset int64
	}
)

// 媒体类型。图片表里既有图片（海报、长图）也有视频（动作迁移、数字人口播），按 kind 区分。
const (
	MediaImage = "image"
	MediaVideo = "video"
)

// MediaOf 图片表记录的成品媒体类型：动作迁移与数字人口播的成品是视频，其余是图片。
func MediaOf(kind string) string {
	if kind == KindActionTransfer || kind == KindVoiceover {
		return MediaVideo
	}
	return MediaImage
}

// MediaKinds 某媒体类型包含的 kind；media 为空返回 nil（不限）。
func MediaKinds(media string) []string {
	switch media {
	case MediaImage:
		return []string{KindPoster, KindLongImage}
	case MediaVideo:
		return []string{KindActionTransfer, KindVoiceover}
	}
	return nil
}

// NewLocalLifeCreationModel returns a model for the database table.
func NewLocalLifeCreationModel(conn sqlx.SqlConn) LocalLifeCreationModel {
	return &customLocalLifeCreationModel{
		defaultLocalLifeCreationModel: newLocalLifeCreationModel(conn),
	}
}

func (m *customLocalLifeCreationModel) withSession(session sqlx.Session) LocalLifeCreationModel {
	return NewLocalLifeCreationModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customLocalLifeCreationModel) FindOwned(ctx context.Context, id, userID int64) (*LocalLifeCreation, error) {
	var resp LocalLifeCreation
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", localLifeCreationRows, m.table)
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

func (m *customLocalLifeCreationModel) FindByRequestID(ctx context.Context, userID int64, requestID string) (*LocalLifeCreation, error) {
	return m.FindOneByUserIdRequestId(ctx, userID, sql.NullString{String: requestID, Valid: true})
}

func (m *customLocalLifeCreationModel) List(ctx context.Context, f ListFilter) ([]*LocalLifeCreation, int64, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if f.UserID > 0 {
		conds, args = append(conds, "`user_id` = ?"), append(args, f.UserID)
	}
	if f.Kind != "" {
		conds, args = append(conds, "`kind` = ?"), append(args, f.Kind)
	}
	if kinds := MediaKinds(f.Media); len(kinds) > 0 {
		conds = append(conds, "`kind` IN ("+strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")+")")
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	if f.Status >= 0 {
		conds, args = append(conds, "`status` = ?"), append(args, f.Status)
	}
	where := " WHERE " + strings.Join(conds, " AND ")
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, fmt.Sprintf("SELECT COUNT(*) FROM %s%s", m.table, where), args...); err != nil {
		return nil, 0, err
	}
	var list []*LocalLifeCreation
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY `id` DESC LIMIT ? OFFSET ?", localLifeCreationRows, m.table, where)
	err := m.conn.QueryRowsCtx(ctx, &list, q, append(args, f.Limit, f.Offset)...)
	return list, total, err
}

func (m *customLocalLifeCreationModel) UpdateTasks(ctx context.Context, id int64, tasks string, status int64, imageURLs, failReason string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `tasks` = ?, `status` = ?, `image_urls` = ?, `fail_reason` = ? "+
		"WHERE `id` = ? AND `status` = ?", m.table)
	r, err := m.conn.ExecCtx(ctx, q, tasks, status, nullString(imageURLs), nullString(failReason), id, StatusGenerating)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

func (m *customLocalLifeCreationModel) ListGenerating(ctx context.Context, before time.Time, limit int64) ([]*LocalLifeCreation, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `status` = ? AND `deleted` = 0 AND `update_time` < ? ORDER BY `id` LIMIT ?", localLifeCreationRows, m.table)
	var list []*LocalLifeCreation
	return list, m.conn.QueryRowsCtx(ctx, &list, q, StatusGenerating, before, limit)
}

func (m *customLocalLifeCreationModel) SoftDelete(ctx context.Context, id int64) error {
	q := fmt.Sprintf("UPDATE %s SET `deleted` = 1 WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, id)
	return err
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
