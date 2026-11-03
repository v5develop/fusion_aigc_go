// Package model assetlib 五张旁表（deploy/sql/Go_V1.0.23__asset_ext.sql）的 bun 结构体，
// 外加 Java 素材表 afv_asset 的只读映射。
//
// 按设计方案 §9.5：新模块业务数据访问用 bun，不再用 goctl 生成 model。
// 这里只放结构体与常量；数据访问在 app/assetlib/rpc/internal/repo。
//
// JSON 列用 json.RawMessage 承接（type:json 让 bun 按 JSON 文本写入）；空值配 nullzero 写 NULL。
// 时间列 DATETIME(3)：插入时零值走库默认 CURRENT_TIMESTAMP(3)。
package model

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 表名。
const (
	TableAsset    = "afv_asset" // Java 表，只读（唯一的写入例外见 repo.AssetRepo.Rename）
	TableExt      = "afv_asset_ext"
	TableVersion  = "afv_asset_version"
	TableRelation = "afv_asset_relation"
	TableTag      = "afv_asset_tag"
	TableTagLink  = "afv_asset_tag_link"
)

// Kind 资产分类（afv_asset_ext.kind 的 ENUM）；空 = 未分类。
type Kind string

const (
	KindModel     Kind = "model"
	KindScene     Kind = "scene"
	KindMakeup    Kind = "makeup"
	KindHair      Kind = "hair"
	KindAccessory Kind = "accessory"
)

// Kinds 全部分类（固定顺序，分类条、后台统计都按它展开）。
var Kinds = []Kind{KindModel, KindScene, KindMakeup, KindHair, KindAccessory}

// VideoKinds 「AI生视频」大类下的二级分类键（与前端 VIDEO_KINDS 一一对应）。
// 只用于列表 kind_group=video 的筛选，不是可写入的 kind（afv_asset_ext.kind 的 ENUM 里没有它们）：
// 视频资产怎么归到这些二级分类还没定数据来源，定之前 kind_group=video 查不到任何资产。
var VideoKinds = []string{
	"viral_video", "video_remake", "promo_video", "digital_human", "digital_human_talk",
	"store_visit", "promo_drama", "motion_transfer", "canvas_video", "video_other",
}

// KindGroupKinds 大类 → 包含的 kind 键；未知大类返回 nil。
func KindGroupKinds(group string) []string {
	switch group {
	case "image":
		out := make([]string, len(Kinds))
		for i, k := range Kinds {
			out[i] = string(k)
		}
		return out
	case "video":
		return append([]string(nil), VideoKinds...)
	}
	return nil
}

// KindLabels 分类中文名（分类条展示用）。
var KindLabels = map[string]string{
	"all":                 "全部",
	string(KindModel):     "模特",
	string(KindScene):     "场景",
	string(KindMakeup):    "妆容",
	string(KindHair):      "发型",
	string(KindAccessory): "配饰",
	"uncategorized":       "未分类",
}

// Valid 是否是已知分类（不含空）。
func (k Kind) Valid() bool {
	for _, v := range Kinds {
		if v == k {
			return true
		}
	}
	return false
}

// 关系类型（afv_asset_relation.relation_type）。
const (
	RelReference     = "reference"      // 参考：双向展示在「相关资产」
	RelGeneratedFrom = "generated_from" // asset 由 related 生成：related 的详情里 asset 出现在「生成结果」，asset 的详情里 related 出现在「相关资产」
	RelVersionOf     = "version_of"     // asset 是 related 的另一版本（只存不展示，详情以 afv_asset_version 为准）
)

// ValidRelation 是否是已知关系类型。
func ValidRelation(t string) bool {
	return t == RelReference || t == RelGeneratedFrom || t == RelVersionOf
}

// 授权状态（afv_asset_ext.license）。
var Licenses = []string{"cleared", "internal", "unknown"}

// Java afv_asset.source_type（Java 实体注释：1-用户上传 2-AI生成）。Java 没有「官方」概念，
// 官方素材在公共模板表里，不进个人素材库；这里把 NULL / 其它值当作 official（系统写入）。
const (
	SourceTypeUpload    = 1
	SourceTypeGenerated = 2
)

// 来源（前端 Asset.source 与 List 的 source 筛选）。input = official + upload（前端 scope=input）。
const (
	SourceUpload    = "upload"
	SourceGenerated = "generated"
	SourceOfficial  = "official"
	SourceInput     = "input"
)

// SourceOf Java source_type → 前端 source。
func SourceOf(t sql.NullInt64) string {
	switch {
	case t.Valid && t.Int64 == SourceTypeUpload:
		return SourceUpload
	case t.Valid && t.Int64 == SourceTypeGenerated:
		return SourceGenerated
	}
	return SourceOfficial
}

// JavaAsset afv_asset 的**只读**映射（列以 Java 迁移 V1.0.1 / 59 / 90 / 93 / 179 / 181 为准）。
//
// 只读：本模块不 INSERT / DELETE 这张表，也不用 bun 的 Update(Model) 改它；
// 唯一的写入是 repo.AssetRepo.Rename 对 name 列的一条受控 UPDATE（pkg/asset 没有改名方法）。
// 未映射的 Java 列：project_id、description、properties、tags（Java 自己的标签 JSON，本模块不读写）、
// integration_delivery_id、integration_access_policy、update_time。
type JavaAsset struct {
	bun.BaseModel `bun:"table:afv_asset,alias:a"`

	ID           int64          `bun:"id,pk"`
	UserID       sql.NullInt64  `bun:"user_id"`
	WorkspaceID  int64          `bun:"workspace_id"`
	SourceTaskID sql.NullString `bun:"source_task_id"`
	Type         sql.NullString `bun:"type"`       // 通用类型 image / video，或创作入口编码（productSetImages、hitVideo …）
	EntryCode    string         `bun:"entry_code"` // 创作入口编码，聚合分类按它展开
	Name         string         `bun:"name"`       // 资产名称 = 前端 title
	CoverURL     sql.NullString `bun:"cover_url"`  // 封面（视频为封面帧）
	ResultURL    sql.NullString `bun:"result_url"` // 产物原图 / 视频 mp4
	WatermarkURL sql.NullString `bun:"watermark_url"`
	Width        int32          `bun:"width"`
	Height       int32          `bun:"height"`
	AspectRatio  string         `bun:"aspect_ratio"`
	SourceType   sql.NullInt64  `bun:"source_type"` // 1-用户上传 2-AI生成
	AIPrompt     sql.NullString `bun:"ai_prompt"`
	Status       sql.NullInt64  `bun:"status"` // 0-草稿 1-正常
	Deleted      int8           `bun:"deleted"`
	CreateTime   time.Time      `bun:"create_time"`
}

// Ext afv_asset_ext：资产扩展（旁表，主键 asset_id）。
type Ext struct {
	bun.BaseModel `bun:"table:afv_asset_ext,alias:e"`

	AssetID          int64           `bun:"asset_id,pk"`
	UserID           int64           `bun:"user_id,notnull"`
	Kind             sql.NullString  `bun:"kind"` // NULL = 未分类
	Favorite         int8            `bun:"favorite,notnull"`
	License          string          `bun:"license,notnull"`
	Metadata         json.RawMessage `bun:"metadata,type:json,nullzero"`
	Provenance       json.RawMessage `bun:"provenance,type:json,nullzero"`
	CurrentVersionID sql.NullInt64   `bun:"current_version_id"`
	CreatedAt        time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
	UpdatedAt        time.Time       `bun:"updated_at,notnull,default:current_timestamp(3)"`
}

// Version afv_asset_version：资产版本。
type Version struct {
	bun.BaseModel `bun:"table:afv_asset_version,alias:v"`

	ID        int64           `bun:"id,pk,autoincrement"`
	AssetID   int64           `bun:"asset_id,notnull"`
	VersionNo int32           `bun:"version_no,notnull"`
	ImageRid  sql.NullString  `bun:"image_rid"`
	ImageURL  string          `bun:"image_url,notnull"`
	Prompt    sql.NullString  `bun:"prompt"`
	Model     string          `bun:"model,notnull"`
	Params    json.RawMessage `bun:"params,type:json,nullzero"`
	CreatedAt time.Time       `bun:"created_at,notnull,default:current_timestamp(3)"`
}

// Relation afv_asset_relation：资产关系。
type Relation struct {
	bun.BaseModel `bun:"table:afv_asset_relation,alias:r"`

	ID             int64     `bun:"id,pk,autoincrement"`
	AssetID        int64     `bun:"asset_id,notnull"`
	RelatedAssetID int64     `bun:"related_asset_id,notnull"`
	RelationType   string    `bun:"relation_type,notnull"`
	CreatedAt      time.Time `bun:"created_at,notnull,default:current_timestamp(3)"`
}

// Tag afv_asset_tag：用户标签字典。
type Tag struct {
	bun.BaseModel `bun:"table:afv_asset_tag,alias:t"`

	ID        int64     `bun:"id,pk,autoincrement"`
	UserID    int64     `bun:"user_id,notnull"`
	Name      string    `bun:"name,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp(3)"`
}

// TagLink afv_asset_tag_link：资产 ↔ 标签。
type TagLink struct {
	bun.BaseModel `bun:"table:afv_asset_tag_link,alias:l"`

	AssetID   int64     `bun:"asset_id,pk"`
	TagID     int64     `bun:"tag_id,pk"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp(3)"`
}

// Metadata afv_asset_ext.metadata 的形状（前端 Asset.metadata）。
type Metadata struct {
	Industry string `json:"industry,omitempty"`
	Style    string `json:"style,omitempty"`
	Shot     string `json:"shot,omitempty"`
	Color    string `json:"color,omitempty"`
}

// Provenance afv_asset_ext.provenance 的形状（前端 Asset.provenance；params 原样 JSON）。
type Provenance struct {
	RunID      string          `json:"run_id,omitempty"`
	WorkflowID string          `json:"workflow_id,omitempty"`
	NodeID     string          `json:"node_id,omitempty"`
	Operator   string          `json:"operator,omitempty"`
	Model      string          `json:"model,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	CreatedAt  int64           `json:"created_at,omitempty"`
}
