package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ AfvVideoCreationModel = (*customAfvVideoCreationModel)(nil)

// 类型
const (
	KindExploreShop = "exploreShop" // 探店视频（72 → 73/81）
	KindShortDrama  = "shortDrama"  // 带货短剧（87 → 88/89）
	KindVideoRemake = "videoRemake" // 爆款视频复刻（70 解析 → 71 复刻）

	// 视频复刻的两种来源
	SourceUpload = "upload" // 上传本地视频
	SourceLink   = "link"   // 粘贴热门视频链接
)

// 状态（两步生成）
const (
	VideoPlanning   int64 = 0 // 构思中（72 / 87）
	VideoPlanned    int64 = 1 // 待确认：剧本已出，等用户点「开始生成视频」
	VideoGenerating int64 = 2 // 生成中（73 / 88）
	VideoDone       int64 = 3
	VideoFailed     int64 = 4
)

type (
	// AfvVideoCreationModel is an interface to be customized, add more methods here,
	// and implement the added methods in customAfvVideoCreationModel.
	AfvVideoCreationModel interface {
		afvVideoCreationModel
		withSession(session sqlx.Session) AfvVideoCreationModel
		// FindOwned 未删除且属于该用户；userID 为 0 表示不校验归属（后台）。
		FindOwned(ctx context.Context, id, userID int64) (*AfvVideoCreation, error)
		FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvVideoCreation, error)
		List(ctx context.Context, f VideoListFilter) ([]*AfvVideoCreation, int64, error)
		// ListByStatus 轮询用：某个状态、更新时间早于 before 的记录（跳过已删除）
		ListByStatus(ctx context.Context, status int64, before time.Time, limit int64) ([]*AfvVideoCreation, error)
		// FinishPlan 构思完成：存剧本，转「待确认」
		FinishPlan(ctx context.Context, id int64, script, failReason string, status int64) (bool, error)
		// FinishParse 视频复刻专用：存剧情文本与原片时长，转「待确认」
		FinishParse(ctx context.Context, id int64, script string, duration int64) (bool, error)
		// StartGenerate 用户确认剧本后：记下出片任务号，转「生成中」
		StartGenerate(ctx context.Context, id int64, genTaskID string) (bool, error)
		// UpdateRemakeSettings 视频复刻专用：出片前把用户改过的剧情 / 档位 / 比例落库
		UpdateRemakeSettings(ctx context.Context, id int64, script, model, size string) error
		// FinishFetch 链接来源：转存完原片后写回地址、封面与解析任务号
		FinishFetch(ctx context.Context, id int64, videoURL, coverURL, planTaskID string) error
		// Replan 重新构思：换一个出剧本任务号，清掉旧剧本，转回「构思中」
		Replan(ctx context.Context, id int64, planTaskID string) (bool, error)
		// FinishGenerate 出片结束（成功或失败）
		FinishGenerate(ctx context.Context, id int64, status int64, videoURL, failReason string) (bool, error)
		SoftDelete(ctx context.Context, id int64) error
	}

	customAfvVideoCreationModel struct {
		*defaultAfvVideoCreationModel
	}

	VideoListFilter struct {
		UserID int64
		Kind   string
		Status int64 // -1 表示不限
		Limit  int64
		Offset int64
	}
)

// NewAfvVideoCreationModel returns a model for the database table.
func NewAfvVideoCreationModel(conn sqlx.SqlConn) AfvVideoCreationModel {
	return &customAfvVideoCreationModel{
		defaultAfvVideoCreationModel: newAfvVideoCreationModel(conn),
	}
}

func (m *customAfvVideoCreationModel) withSession(session sqlx.Session) AfvVideoCreationModel {
	return NewAfvVideoCreationModel(sqlx.NewSqlConnFromSession(session))
}

func (m *customAfvVideoCreationModel) FindOwned(ctx context.Context, id, userID int64) (*AfvVideoCreation, error) {
	var resp AfvVideoCreation
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `id` = ? AND `deleted` = 0 LIMIT 1", afvVideoCreationRows, m.table)
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

func (m *customAfvVideoCreationModel) FindByRequestID(ctx context.Context, userID int64, requestID string) (*AfvVideoCreation, error) {
	return m.FindOneByUserIdRequestId(ctx, userID, sql.NullString{String: requestID, Valid: true})
}

func (m *customAfvVideoCreationModel) List(ctx context.Context, f VideoListFilter) ([]*AfvVideoCreation, int64, error) {
	conds := []string{"`deleted` = 0"}
	var args []any
	if f.UserID > 0 {
		conds, args = append(conds, "`user_id` = ?"), append(args, f.UserID)
	}
	if f.Kind != "" {
		conds, args = append(conds, "`kind` = ?"), append(args, f.Kind)
	}
	if f.Status >= 0 {
		conds, args = append(conds, "`status` = ?"), append(args, f.Status)
	}
	where := " WHERE " + strings.Join(conds, " AND ")
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, fmt.Sprintf("SELECT COUNT(*) FROM %s%s", m.table, where), args...); err != nil {
		return nil, 0, err
	}
	var list []*AfvVideoCreation
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY `id` DESC LIMIT ? OFFSET ?", afvVideoCreationRows, m.table, where)
	err := m.conn.QueryRowsCtx(ctx, &list, q, append(args, f.Limit, f.Offset)...)
	return list, total, err
}

func (m *customAfvVideoCreationModel) ListByStatus(ctx context.Context, status int64, before time.Time, limit int64) ([]*AfvVideoCreation, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE `status` = ? AND `deleted` = 0 AND `update_time` < ? ORDER BY `id` LIMIT ?",
		afvVideoCreationRows, m.table)
	var list []*AfvVideoCreation
	return list, m.conn.QueryRowsCtx(ctx, &list, q, status, before, limit)
}

func (m *customAfvVideoCreationModel) FinishPlan(ctx context.Context, id int64, script, failReason string, status int64) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `plan_script` = ?, `status` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, nullString(script), status, nullString(failReason), id, VideoPlanning))
}

func (m *customAfvVideoCreationModel) FinishParse(ctx context.Context, id int64, script string, duration int64) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `plan_script` = ?, `duration` = ?, `status` = ?, `fail_reason` = NULL "+
		"WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, nullString(script), duration, VideoPlanned, id, VideoPlanning))
}

func (m *customAfvVideoCreationModel) StartGenerate(ctx context.Context, id int64, genTaskID string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `gen_task_id` = ?, `status` = ?, `fail_reason` = NULL WHERE `id` = ? AND `status` IN (?, ?)", m.table)
	return affected(m.conn.ExecCtx(ctx, q, nullString(genTaskID), VideoGenerating, id, VideoPlanned, VideoFailed))
}

func (m *customAfvVideoCreationModel) UpdateRemakeSettings(ctx context.Context, id int64, script, mdl, size string) error {
	q := fmt.Sprintf("UPDATE %s SET `plan_script` = ?, `model` = ?, `size` = ? WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, nullString(script), mdl, size, id)
	return err
}

func (m *customAfvVideoCreationModel) FinishFetch(ctx context.Context, id int64, videoURL, coverURL, planTaskID string) error {
	q := fmt.Sprintf("UPDATE %s SET `source_video_url` = ?, `cover_url` = COALESCE(NULLIF(?, ''), `cover_url`), "+
		"`plan_task_id` = ? WHERE `id` = ? AND `deleted` = 0", m.table)
	_, err := m.conn.ExecCtx(ctx, q, nullString(videoURL), coverURL, nullString(planTaskID), id)
	return err
}

func (m *customAfvVideoCreationModel) Replan(ctx context.Context, id int64, planTaskID string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `plan_task_id` = ?, `plan_script` = NULL, `status` = ?, `fail_reason` = NULL "+
		"WHERE `id` = ? AND `status` IN (?, ?)", m.table)
	return affected(m.conn.ExecCtx(ctx, q, nullString(planTaskID), VideoPlanning, id, VideoPlanned, VideoFailed))
}

func (m *customAfvVideoCreationModel) FinishGenerate(ctx context.Context, id int64, status int64, videoURL, failReason string) (bool, error) {
	q := fmt.Sprintf("UPDATE %s SET `status` = ?, `video_url` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?", m.table)
	return affected(m.conn.ExecCtx(ctx, q, status, nullString(videoURL), nullString(failReason), id, VideoGenerating))
}

func (m *customAfvVideoCreationModel) SoftDelete(ctx context.Context, id int64) error {
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
