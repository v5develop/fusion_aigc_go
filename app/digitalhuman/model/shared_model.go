package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SharedModel 读取现有系统的表。只做 SELECT，不改这些表的结构和数据。
type SharedModel struct {
	conn sqlx.SqlConn
}

func NewSharedModel(conn sqlx.SqlConn) *SharedModel {
	return &SharedModel{conn: conn}
}

// TTSModel 当前配音模型：afv_system_config.ai_tts_model_id → afv_ai_model → afv_ai_provider。
type TTSModel struct {
	Id             int64  `db:"id"`
	Name           string `db:"name"`
	Code           string `db:"code"`
	Platform       string `db:"platform"`
	ApiUrl         string `db:"api_url"`
	ApiKey         string `db:"api_key"`
	TimeoutSeconds int64  `db:"generation_timeout_seconds"`
	MaxConcurrency int64  `db:"max_concurrency"`
}

const configKeyTTSModel = "ai_tts_model_id"

// ResolveTTSModel 取当前启用的配音模型；配置缺失或模型停用时报错。
func (m *SharedModel) ResolveTTSModel(ctx context.Context) (*TTSModel, error) {
	var t TTSModel
	q := "SELECT m.id, m.name, m.code, " +
		"COALESCE(p.platform, '') AS platform, COALESCE(p.api_url, '') AS api_url, COALESCE(p.api_key, '') AS api_key, " +
		"COALESCE(m.generation_timeout_seconds, 0) AS generation_timeout_seconds, COALESCE(m.max_concurrency, 0) AS max_concurrency " +
		"FROM afv_system_config c " +
		"JOIN afv_ai_model m ON m.id = c.config_value AND m.deleted = 0 AND m.status = 1 " +
		"LEFT JOIN afv_ai_provider p ON p.id = m.ai_provider_id AND p.deleted = 0 " +
		"WHERE c.config_key = ? AND c.deleted = 0 LIMIT 1"
	switch err := m.conn.QueryRowCtx(ctx, &t, q, configKeyTTSModel); err {
	case nil:
		return &t, nil
	case sqlx.ErrNotFound:
		return nil, fmt.Errorf("配音模型未配置或已停用（afv_system_config.%s）", configKeyTTSModel)
	default:
		return nil, err
	}
}

// BlockWords 敏感词：与 AI文案共用 afv_copywriting_option 的 BLOCKWORD 词库（code 即词本身）。
func (m *SharedModel) BlockWords(ctx context.Context) ([]string, error) {
	var words []string
	q := "SELECT `code` FROM afv_copywriting_option WHERE `option_type` = 'BLOCKWORD' AND `status` = 1 AND `deleted` = 0"
	return words, m.conn.QueryRowsCtx(ctx, &words, q)
}

// PublicModelSlot 公共数字人在模板素材投放位里的位置。
type PublicModelSlot struct {
	BizType, Module, Slot string
}

// PublicModel 公共数字人：复用现有模板素材（运营在现有后台「模板」里维护）。
//   - afv_public_template_assets：title=名字，url=形象图，extra.role=职业标签，extra.avatarUrl=头像（可空）
//   - afv_public_template_asset_placement：投放位（排序、上下架）
type PublicModel struct {
	Id        int64  `db:"id"`
	Name      string `db:"name"`
	ImageUrl  string `db:"image_url"`
	RoleLabel string `db:"role_label"`
	AvatarUrl string `db:"avatar_url"`
	SortOrder int64  `db:"sort_order"`
}

const publicModelSelect = "SELECT a.id, a.title AS name, a.url AS image_url, " +
	"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(a.extra, '$.role')), '') AS role_label, " +
	"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(a.extra, '$.avatarUrl')), '') AS avatar_url, p.sort_order " +
	"FROM afv_public_template_asset_placement p " +
	"JOIN afv_public_template_assets a ON a.id = p.asset_id AND a.deleted = 0 " +
	"WHERE p.biz_type = ? AND p.module_code = ? AND p.slot_code = ? AND p.status = 1 AND p.deleted = 0"

// ListPublicModels 上架中的公共数字人，按投放位排序。
func (m *SharedModel) ListPublicModels(ctx context.Context, s PublicModelSlot) ([]*PublicModel, error) {
	var list []*PublicModel
	q := publicModelSelect + " ORDER BY p.sort_order, p.id"
	return list, m.conn.QueryRowsCtx(ctx, &list, q, s.BizType, s.Module, s.Slot)
}

// FindPublicModel 取一个上架中的公共数字人；已下架或不在该投放位返回 ErrNotFound。
func (m *SharedModel) FindPublicModel(ctx context.Context, s PublicModelSlot, id int64) (*PublicModel, error) {
	var p PublicModel
	switch err := m.conn.QueryRowCtx(ctx, &p, publicModelSelect+" AND a.id = ? LIMIT 1", s.BizType, s.Module, s.Slot, id); err {
	case nil:
		return &p, nil
	case sqlx.ErrNotFound:
		return nil, ErrNotFound
	default:
		return nil, err
	}
}
