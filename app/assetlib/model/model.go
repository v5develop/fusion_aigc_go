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

	"aigc-local-life/pkg/asset"

	"github.com/uptrace/bun"
)

// 表名。
const (
	TableAsset    = "afv_asset" // Java 表，只读（受控写入例外：repo.AssetRepo.Rename / SoftDelete）
	TableExt      = "afv_asset_ext"
	TableVersion  = "afv_asset_version"
	TableRelation = "afv_asset_relation"
	TableTag      = "afv_asset_tag"
	TableTagLink  = "afv_asset_tag_link"
)

// Kind 资产分类（afv_asset_ext.kind 的 ENUM）；空 = 未分类。
// 取值与 pkg/asset.Usage（进库时的默认用途）是同一组，中文名也取自 pkg/asset（Go 唯一一份口径）。
type Kind string

const (
	KindModel     Kind = Kind(asset.UsageModel)
	KindScene     Kind = Kind(asset.UsageScene)
	KindMakeup    Kind = Kind(asset.UsageMakeup)
	KindHair      Kind = Kind(asset.UsageHair)
	KindAccessory Kind = Kind(asset.UsageAccessory)
)

// Kinds 全部分类（固定顺序，分类条、后台统计都按它展开）= pkg/asset.Usages。
var Kinds = func() []Kind {
	out := make([]Kind, len(asset.Usages))
	for i, u := range asset.Usages {
		out[i] = Kind(u)
	}
	return out
}()

// 分类大类（kind_group）：image = 商拍类（沿用旧键，与 Java /asset/categories 的「创作素材」组一致）、
// marketing = 营销素材类、video = 视频类。一个 kind 只属于一个大类。
var kindGroups = map[string][]asset.Usage{
	"image":     {asset.UsageModel, asset.UsageScene, asset.UsageMakeup, asset.UsageHair, asset.UsageAccessory},
	"marketing": {asset.UsageProduct, asset.UsageStorefront, asset.UsageDish, asset.UsageLogo, asset.UsagePoster, asset.UsageReference, asset.UsageOther},
	"video":     {asset.UsageVideoFinal, asset.UsageVideoSegment},
}

// KindGroups 大类键（固定顺序）。
var KindGroups = []string{"image", "marketing", "video"}

// KindGroupLabels 大类中文名。
var KindGroupLabels = map[string]string{"image": "商拍素材", "marketing": "营销素材", "video": "视频素材"}

// KindGroupKinds 大类 → 包含的 kind 键；未知大类返回 nil。
func KindGroupKinds(group string) []string {
	us, ok := kindGroups[group]
	if !ok {
		return nil
	}
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = string(u)
	}
	return out
}

// KindLabels 分类中文名（分类条展示用）：全部 / 各 kind（取 pkg/asset 用途中文名）/ 未分类。
var KindLabels = func() map[string]string {
	m := map[string]string{"all": "全部", "uncategorized": "未分类"}
	for _, u := range asset.Usages {
		m[string(u)] = u.Label()
	}
	return m
}()

// 分类来源（Asset.kind_source）：user = 用户在资产库改过（afv_asset_ext.kind 有值）；auto = 系统按用途标签 / 入口 / 类型推算。
const (
	KindSourceUser = "user"
	KindSourceAuto = "auto"
)

// Valid 是否是已知分类（不含空）。
func (k Kind) Valid() bool { return asset.Usage(k).Valid() }

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
// 只读为主：不用 bun 的 Update(Model) 改它；受控写入只有 repo.AssetRepo.Rename（name 列）、SoftDelete（deleted 列）
// 与 repo.AssetRepo.InsertUpload（「存为资产」新增一行用户上传，见 repo/upload.go）。
// 未映射的 Java 列：project_id、description、properties、tags（Java 的标签 JSON，未启用；用户标签以本模块 afv_asset_tag 为唯一来源）、
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
	SysTags      sql.NullString `bun:"sys_tags"` // 系统标签 JSON 数组（Java V1.0.236；pkg/asset 与 Java 入库时写，用户不可改）
	Status       sql.NullInt64  `bun:"status"`   // 0-草稿 1-正常
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
