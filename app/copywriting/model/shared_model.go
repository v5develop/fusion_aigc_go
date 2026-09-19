package model

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SharedModel 读取现有系统的表。只做 SELECT，不改这些表的结构和数据。
type SharedModel struct {
	conn sqlx.SqlConn
}

func NewSharedModel(conn sqlx.SqlConn) *SharedModel {
	return &SharedModel{conn: conn}
}

// WritingModel 当前写作模型：afv_system_config.ai_writing_model_id → afv_ai_model → afv_ai_provider。
type WritingModel struct {
	Id       int64  `db:"id"`
	Name     string `db:"name"`
	Code     string `db:"code"`
	Platform string `db:"platform"`
	ApiUrl   string `db:"api_url"`
	ApiKey   string `db:"api_key"`
	// AutoAppendV1Path provider 配置的是否自动补 /v1 路径
	AutoAppendV1Path int64 `db:"auto_append_v1_path"`
	TimeoutSeconds   int64 `db:"generation_timeout_seconds"`
	MaxConcurrency   int64 `db:"max_concurrency"`
}

const configKeyWritingModel = "ai_writing_model_id"

// ResolveWritingModel 取当前启用的写作模型；配置缺失或模型停用时报错。
func (m *SharedModel) ResolveWritingModel(ctx context.Context) (*WritingModel, error) {
	var w WritingModel
	q := "SELECT m.id, m.name, m.code, " +
		"COALESCE(p.platform, '') AS platform, COALESCE(p.api_url, '') AS api_url, COALESCE(p.api_key, '') AS api_key, " +
		"COALESCE(p.auto_append_v1_path, 0) AS auto_append_v1_path, COALESCE(m.generation_timeout_seconds, 0) AS generation_timeout_seconds, " +
		"COALESCE(m.max_concurrency, 0) AS max_concurrency " +
		"FROM afv_system_config c " +
		"JOIN afv_ai_model m ON m.id = c.config_value AND m.deleted = 0 AND m.status = 1 " +
		"LEFT JOIN afv_ai_provider p ON p.id = m.ai_provider_id AND p.deleted = 0 " +
		"WHERE c.config_key = ? AND c.deleted = 0 LIMIT 1"
	switch err := m.conn.QueryRowCtx(ctx, &w, q, configKeyWritingModel); err {
	case nil:
		return &w, nil
	case sqlx.ErrNotFound:
		return nil, fmt.Errorf("写作模型未配置或已停用（afv_system_config.%s）", configKeyWritingModel)
	default:
		return nil, err
	}
}

// Derivative 文案衍生出的内容（来自 afv_asset）。
type Derivative struct {
	Id         int64     `db:"id"`
	Type       string    `db:"type"`
	CoverUrl   string    `db:"cover_url"`
	ResultUrl  string    `db:"result_url"`
	CreateTime time.Time `db:"create_time"`
}

// ListDerivatives 生成历史：下游模块发任务时把 afv_creation_task.external_ref 写成文案 id，
// 这里用 afv_asset.source_task_id 关联回来取封面。assetTypes 为空表示全部。
//
// TODO 本版不交付。external_ref 的这种用法还需数据库负责人确认；
// 若不采用，改为新建文案→衍生内容的关系表。
func (m *SharedModel) ListDerivatives(ctx context.Context, copywritingID, userID int64, assetTypes []string, limit, offset int64) ([]*Derivative, int64, error) {
	conds := []string{"t.external_ref = ?", "t.deleted = 0", "a.deleted = 0"}
	args := []any{fmt.Sprint(copywritingID)}
	if userID > 0 {
		conds, args = append(conds, "t.user_id = ?"), append(args, userID)
	}
	if len(assetTypes) > 0 {
		conds = append(conds, "a.type IN ("+placeholders(len(assetTypes))+")")
		for _, t := range assetTypes {
			args = append(args, t)
		}
	}
	where := " FROM afv_creation_task t JOIN afv_asset a ON a.source_task_id = t.task_id WHERE " + strings.Join(conds, " AND ")

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*)"+where, args...); err != nil {
		return nil, 0, err
	}
	var list []*Derivative
	q := "SELECT a.id, a.type, COALESCE(a.cover_url, '') AS cover_url, COALESCE(a.result_url, '') AS result_url, a.create_time" +
		where + " ORDER BY a.id DESC LIMIT ? OFFSET ?"
	err := m.conn.QueryRowsCtx(ctx, &list, q, append(args, limit, offset)...)
	return list, total, err
}

// ModerationLog 一条文本审核记录，字段对齐 afv_moderation_log。
type ModerationLog struct {
	TaskId         string
	UserId         int64
	Provider       string
	Passed         int64
	Scene          string
	Label          string
	Reason         string
	RequestContent string
	RequestId      string
}

// InsertModerationLog 写审核留痕。
//
// TODO 默认关闭（配置 Moderation.LogToDb）。这是本服务唯一会写的现有表，
// 开启前需数据库负责人确认：provider 用什么值（现有只有 yidun）、task_id 的命名约定。
func (m *SharedModel) InsertModerationLog(ctx context.Context, l ModerationLog) error {
	q := "INSERT INTO afv_moderation_log (task_id, user_id, biz_type, provider, passed, scene, label, reason, request_content, request_id) " +
		"VALUES (?, ?, 'TEXT', ?, ?, ?, ?, ?, ?, ?)"
	_, err := m.conn.ExecCtx(ctx, q, l.TaskId, l.UserId, l.Provider, l.Passed,
		nullable(l.Scene), nullable(l.Label), nullable(l.Reason), l.RequestContent, nullable(l.RequestId))
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
