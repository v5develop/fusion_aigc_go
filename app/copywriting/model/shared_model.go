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

// Derivative 文案衍生出的内容：这条文案被拿去生成过什么。
//
// Id 是**业务记录 id**（笔记 id / 生成记录 id / 视频 id / 数字人 id），前端拿它直接打开成品页。
// Size 是成品比例（如 9:16），用来判断横版竖版——各表都没存像素宽高，比例够用且更准。
type Derivative struct {
	Id         int64     `db:"id"`
	Kind       string    `db:"kind"`
	CoverUrl   string    `db:"cover_url"`
	ResultUrl  string    `db:"result_url"`
	IsVideo    int64     `db:"is_video"`
	Size       string    `db:"size"`
	CreateTime time.Time `db:"create_time"`
}

// 生成历史里的 kind 取值。和各模块的 kind 不完全一样：
// 图片类里「海报 / 长图」分开给，口播单列（产品要求算进生成历史）。
const (
	DerivativeNote         = "note"          // 图文笔记
	DerivativePoster       = "poster"        // AI海报
	DerivativeLongImage    = "long_image"    // AI长图
	DerivativeVoiceover    = "voiceover"     // 数字人口播
	DerivativeDigitalHuman = "digital_human" // AI数字人视频
	DerivativeExploreVideo = "explore_video" // 探店视频
	DerivativePromoDrama   = "promo_drama"   // 带货短剧
)

// derivativeSources 每种 kind 从哪张表捞、怎么取封面和成品。
//
// 四张业务表形状不同，所以各写一段 SELECT 再 UNION ALL —— 让数据库统一排序分页，
// 免得在内存里合并四个结果集还要自己分页。
//
// 只收**已完成**的记录：生成历史是拿来点开看成品的，生成中和失败的给出去没意义
// （各表的状态码还不一样，混在一起更容易出错）。
var derivativeSources = map[string]string{
	DerivativeNote: "SELECT `id`, '" + DerivativeNote + "' AS `kind`, " + firstImage + " AS `cover_url`, " +
		firstImage + " AS `result_url`, 0 AS `is_video`, '' AS `size`, `create_time`, `copywriting_id`, `user_id` " +
		"FROM `afv_image_text_note` WHERE `status` = 4 AND `deleted` = 0",

	DerivativePoster: imageCreationSource(DerivativePoster, "poster", 0),
	DerivativeLongImage: imageCreationSource(DerivativeLongImage, "longImage", 0),
	DerivativeVoiceover: imageCreationSource(DerivativeVoiceover, "voiceover", 1),

	DerivativeDigitalHuman: "SELECT `id`, '" + DerivativeDigitalHuman + "' AS `kind`, COALESCE(`image_url`, '') AS `cover_url`, " +
		"COALESCE(`video_url`, '') AS `result_url`, 1 AS `is_video`, '' AS `size`, `create_time`, `copywriting_id`, `user_id` " +
		"FROM `afv_digital_human` WHERE `status` = 2 AND `deleted` = 0",

	DerivativeExploreVideo: videoCreationSource(DerivativeExploreVideo, "exploreShop"),
	DerivativePromoDrama:   videoCreationSource(DerivativePromoDrama, "shortDrama"),
}

// firstImage 取 image_urls（JSON 数组）里的第一张。套图就用第一张当封面。
const firstImage = "COALESCE(JSON_UNQUOTE(JSON_EXTRACT(`image_urls`, '$[0]')), '')"

func imageCreationSource(kind, tableKind string, isVideo int) string {
	return fmt.Sprintf("SELECT `id`, '%s' AS `kind`, %s AS `cover_url`, %s AS `result_url`, %d AS `is_video`, "+
		"COALESCE(`size`, '') AS `size`, `create_time`, `copywriting_id`, `user_id` "+
		"FROM `afv_local_life_creation` WHERE `kind` = '%s' AND `status` = 1 AND `deleted` = 0",
		kind, firstImage, firstImage, isVideo, tableKind)
}

func videoCreationSource(kind, tableKind string) string {
	return fmt.Sprintf("SELECT `id`, '%s' AS `kind`, COALESCE(NULLIF(`cover_url`, ''), `char_url`, '') AS `cover_url`, "+
		"COALESCE(`video_url`, '') AS `result_url`, 1 AS `is_video`, COALESCE(`size`, '') AS `size`, "+
		"`create_time`, `copywriting_id`, `user_id` "+
		"FROM `afv_video_creation` WHERE `kind` = '%s' AND `status` = 3 AND `deleted` = 0", kind, tableKind)
}

// DerivativeKinds 全部 kind，按展示顺序。
func DerivativeKinds() []string {
	return []string{DerivativeNote, DerivativePoster, DerivativeLongImage, DerivativeVoiceover,
		DerivativeDigitalHuman, DerivativeExploreVideo, DerivativePromoDrama}
}

// ListDerivatives 生成历史：这条文案被拿去生成过哪些成品。kinds 为空表示全部。
func (m *SharedModel) ListDerivatives(ctx context.Context, copywritingID, userID int64, kinds []string, limit, offset int64) ([]*Derivative, int64, error) {
	if len(kinds) == 0 {
		kinds = DerivativeKinds()
	}
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if q, ok := derivativeSources[k]; ok {
			parts = append(parts, q)
		}
	}
	if len(parts) == 0 {
		return nil, 0, nil
	}
	union := "(" + strings.Join(parts, ") UNION ALL (") + ")"
	where := "SELECT * FROM (" + union + ") d WHERE d.`copywriting_id` = ?"
	args := []any{copywritingID}
	if userID > 0 {
		where += " AND d.`user_id` = ?"
		args = append(args, userID)
	}

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM ("+where+") t", args...); err != nil {
		return nil, 0, err
	}
	var list []*Derivative
	q := "SELECT d.`id`, d.`kind`, d.`cover_url`, d.`result_url`, d.`is_video`, d.`size`, d.`create_time` FROM (" +
		union + ") d WHERE d.`copywriting_id` = ?"
	if userID > 0 {
		q += " AND d.`user_id` = ?"
	}
	q += " ORDER BY d.`create_time` DESC, d.`id` DESC LIMIT ? OFFSET ?"
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
