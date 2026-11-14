package repo

import (
	"context"
	"database/sql"
	"encoding/json"

	"aigc-local-life/app/assetlib/model"

	"github.com/uptrace/bun"
)

// AssetRow afv_asset（只读）LEFT JOIN afv_asset_ext / 当前版本 的一行。e_* 为旁表列，没有旁表行时为 NULL。
type AssetRow struct {
	model.JavaAsset `bun:",extend"`

	ExtKind          sql.NullString `bun:"e_kind,scanonly"`
	ExtFavorite      sql.NullInt64  `bun:"e_favorite,scanonly"`
	ExtLicense       sql.NullString `bun:"e_license,scanonly"`
	ExtMetadata      []byte         `bun:"e_metadata,scanonly"`
	ExtProvenance    []byte         `bun:"e_provenance,scanonly"`
	CurrentVersionID sql.NullInt64  `bun:"e_current_version_id,scanonly"`
	CurrentImage     sql.NullString `bun:"v_image_url,scanonly"`
	IsVideo          bool           `bun:"is_video,scanonly"` // 按 videoCond 在 SQL 里算出的媒体形态
	// EffKind 有效分类 = ext.kind（用户改过）或推算的默认分类（model.EffectiveKindExpr）；NULL = 未分类
	EffKind sql.NullString `bun:"eff_kind,scanonly"`
}

// KindSource 分类来源：ext.kind 有值为 user，否则 auto（含推算不出的未分类）。
func (a *AssetRow) KindSource() string {
	if a.ExtKind.Valid && a.ExtKind.String != "" {
		return model.KindSourceUser
	}
	return model.KindSourceAuto
}

// SysTagList 系统标签（sys_tags JSON 数组；NULL / 非法为空）。
func (a *AssetRow) SysTagList() []string {
	if !a.SysTags.Valid || a.SysTags.String == "" {
		return []string{}
	}
	var out []string
	if json.Unmarshal([]byte(a.SysTags.String), &out) != nil || out == nil {
		return []string{}
	}
	return out
}

// Filter 列表 / 计数的筛选条件（空值不筛）。
type Filter struct {
	UserID       int64    // 必填：归属
	Kind         string   // 空=全部；uncategorized=有效分类为空；其余为 model.Kinds（按有效分类 = 用户分类 > 推算）
	Source       string   // 空 / input / generated / official / upload
	KindGroup    string   // 空 / image / marketing / video：按 model.KindGroupKinds 取有效分类 IN (...)
	Media        string   // 空 / image / video
	AspectRatio  string   // 空 / afv_asset.aspect_ratio 精确匹配
	EntryCodes   []string // 空=不限；a.entry_code IN (...)（素材库聚合分类节点的成员入口码）
	Tags         []string // 用户标签名，全部包含才算命中（AND）
	SysTags      []string // 系统标签，全部包含才算命中（AND）
	Untagged     bool     // 只要没挂本人任何标签的资产（口径同 TagRepo.UntaggedCount）
	FavoriteOnly bool
	Q            string
	// RelatedOf / ResultsOf 详情页的相关资产 / 生成结果（按 afv_asset_relation 取 id 集合）
	RelatedOf int64
	ResultsOf int64
}

// AssetRepo afv_asset 只读查询（+ 旁表 JOIN）。
type AssetRepo struct {
	db         bun.IDB
	videoTypes []string
}

// offerSQL Java 计费 offer 里已启用的入口（OfferService 只缓存 status=ENABLED 的 offer）。只读。
const offerSQL = "SELECT o.entry_code FROM afv_billing_offer AS o WHERE o.status = 'ENABLED' AND o.deleted = 0 AND "

// videoCond 视频判据，逐分支对齐 Java AssetService.resolveMediaType：
//  1. type 为 video / image 字面量直接判定（列是 unicode_ci，大小写不敏感）；
//  2. 已注册入口按 afv_billing_offer.category 判死（VIDEO 为视频，其它为图片）；
//  3. offer 查不到的：配置 VideoTypes（对应 Java 的 creaft 兼容入口 assetMediaType=VIDEO，Go 读不到那份配置）或名字含 video 判视频。
//
// type 为 NULL 时整个表达式为 NULL，调用方按图片处理。
func (r *AssetRepo) videoCond() (string, []any) {
	tail := "a.type LIKE '%video%'"
	var args []any
	if len(r.videoTypes) > 0 {
		tail = "(a.type LIKE '%video%' OR a.type IN (?))"
		args = append(args, bun.In(r.videoTypes))
	}
	return "(a.type = 'video' OR (a.type <> 'image' AND (" +
		"a.type IN (" + offerSQL + "o.category = 'VIDEO') OR " +
		"(a.type NOT IN (" + offerSQL + "o.category IS NOT NULL AND o.category <> '') AND " + tail + "))))", args
}

// kindExpr 有效分类表达式及其参数（视频判据的配置类型）。
func (r *AssetRepo) kindExpr() (string, []any) {
	vc, args := r.videoCond()
	return model.EffectiveKindExpr(vc), args
}

// sourceCond 来源筛选（Java source_type：1 上传 2 生成；NULL / 其它 = official）。
func sourceCond(source string) string {
	switch source {
	case model.SourceUpload:
		return "a.source_type = 1"
	case model.SourceGenerated:
		return "a.source_type = 2"
	case model.SourceOfficial:
		return "(a.source_type IS NULL OR a.source_type NOT IN (1, 2))"
	case model.SourceInput:
		return "(a.source_type IS NULL OR a.source_type <> 2)"
	}
	return ""
}

// joinExt 旁表与当前版本。
func joinExt(q *bun.SelectQuery) *bun.SelectQuery {
	return q.Join("LEFT JOIN afv_asset_ext AS e ON e.asset_id = a.id").
		Join("LEFT JOIN afv_asset_version AS v ON v.id = e.current_version_id")
}

// owned 用户侧归属：本人、未删除、非草稿。
func owned(q *bun.SelectQuery, userID int64) *bun.SelectQuery {
	return q.Where("a.user_id = ?", userID).Where("a.deleted = 0").Where("(a.status IS NULL OR a.status <> 0)")
}

// apply 把 Filter 拼到查询上（不含 JOIN）。
func (r *AssetRepo) apply(q *bun.SelectQuery, f Filter) *bun.SelectQuery {
	q = owned(q, f.UserID)
	if f.Kind != "" || f.KindGroup != "" {
		ke, kargs := r.kindExpr()
		switch f.Kind {
		case "":
		case "uncategorized":
			q = q.Where(ke+" IS NULL", kargs...)
		default:
			q = q.Where(ke+" = ?", append(append([]any{}, kargs...), f.Kind)...)
		}
		if f.KindGroup != "" {
			q = q.Where(ke+" IN (?)", append(append([]any{}, kargs...), bun.In(model.KindGroupKinds(f.KindGroup)))...)
		}
	}
	if f.AspectRatio != "" {
		q = q.Where("a.aspect_ratio = ?", f.AspectRatio)
	}
	if len(f.EntryCodes) > 0 {
		q = q.Where("a.entry_code IN (?)", bun.In(f.EntryCodes))
	}
	if c := sourceCond(f.Source); c != "" {
		q = q.Where(c)
	}
	switch f.Media {
	case "video":
		c, args := r.videoCond()
		q = q.Where(c, args...)
	case "image":
		c, args := r.videoCond()
		q = q.Where("(a.type IS NULL OR NOT "+c+")", args...)
	}
	if f.FavoriteOnly {
		q = q.Where("e.favorite = 1")
	}
	for _, tag := range f.Tags {
		q = q.Where("EXISTS (SELECT 1 FROM afv_asset_tag_link AS l JOIN afv_asset_tag AS t ON t.id = l.tag_id "+
			"WHERE l.asset_id = a.id AND t.user_id = ? AND t.name = ?)", f.UserID, tag)
	}
	for _, tag := range f.SysTags {
		q = q.Where("JSON_CONTAINS(a.sys_tags, JSON_QUOTE(?))", tag)
	}
	if f.Untagged {
		q = q.Where(untaggedCond, f.UserID)
	}
	if f.Q != "" {
		like := likeArg(f.Q)
		q = q.Where("(a.name LIKE ? OR a.ai_prompt LIKE ?)", like, like)
	}
	if f.RelatedOf > 0 {
		// 相关资产：reference 双向 + 本资产 generated_from 的来源
		q = q.Where("a.id IN (SELECT related_asset_id FROM afv_asset_relation WHERE asset_id = ? AND relation_type IN ('reference', 'generated_from') "+
			"UNION SELECT asset_id FROM afv_asset_relation WHERE related_asset_id = ? AND relation_type = 'reference')", f.RelatedOf, f.RelatedOf)
	}
	if f.ResultsOf > 0 {
		// 生成结果：generated_from 指向本资产的资产
		q = q.Where("a.id IN (SELECT asset_id FROM afv_asset_relation WHERE related_asset_id = ? AND relation_type = 'generated_from')", f.ResultsOf)
	}
	return q
}

func (r *AssetRepo) selectRows(rows *[]*AssetRow) *bun.SelectQuery {
	vc, vargs := r.videoCond()
	ke, kargs := r.kindExpr()
	return joinExt(r.db.NewSelect().Model(rows).
		ColumnExpr("a.id, a.user_id, a.workspace_id, a.source_task_id, a.type, a.entry_code, a.name, a.cover_url, a.result_url, "+
			"a.watermark_url, a.width, a.height, a.aspect_ratio, a.source_type, a.ai_prompt, a.sys_tags, a.status, a.deleted, a.create_time").
		ColumnExpr("e.kind AS e_kind, e.favorite AS e_favorite, e.license AS e_license, e.metadata AS e_metadata, "+
			"e.provenance AS e_provenance, e.current_version_id AS e_current_version_id, v.image_url AS v_image_url").
		ColumnExpr("COALESCE("+vc+", FALSE) AS is_video", vargs...).
		ColumnExpr(ke+" AS eff_kind", kargs...))
}

// List 分页列表（create_time 倒序）。total=0 时不查第二条。
func (r *AssetRepo) List(ctx context.Context, f Filter, limit, offset int64) ([]*AssetRow, int64, error) {
	cnt, err := r.apply(joinExt(r.db.NewSelect().TableExpr("afv_asset AS a")), f).Count(ctx)
	if err != nil || cnt == 0 {
		return nil, int64(cnt), err
	}
	var rows []*AssetRow
	err = r.apply(r.selectRows(&rows), f).OrderExpr("a.create_time DESC, a.id DESC").Limit(limit).Offset(offset).Scan(ctx)
	return rows, int64(cnt), err
}

// GetOwned 取本人的一条（含旁表）；不存在 / 不属于本人 / 已删除 / 草稿都返回 nil。
func (r *AssetRepo) GetOwned(ctx context.Context, id, userID int64) (*AssetRow, error) {
	var rows []*AssetRow
	err := owned(r.selectRows(&rows).Where("a.id = ?", id), userID).Limit(1).Scan(ctx)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// OwnedIDs ids 里属于本人（未删除、非草稿）的那些。
func (r *AssetRepo) OwnedIDs(ctx context.Context, userID int64, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []int64
	err := owned(r.db.NewSelect().TableExpr("afv_asset AS a").Column("a.id").Where("a.id IN (?)", bun.In(ids)), userID).Scan(ctx, &out)
	return out, err
}

// OwnedSource 本人资产的 id 与来源（删除前分流官方 / 可删）。
type OwnedSource struct {
	ID         int64         `bun:"id"`
	SourceType sql.NullInt64 `bun:"source_type"`
}

// OwnedSources ids 里属于本人（未删除、非草稿）的那些，带 source_type。
func (r *AssetRepo) OwnedSources(ctx context.Context, userID int64, ids []int64) ([]OwnedSource, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []OwnedSource
	err := owned(r.db.NewSelect().TableExpr("afv_asset AS a").Column("a.id", "a.source_type").Where("a.id IN (?)", bun.In(ids)), userID).
		Scan(ctx, &out)
	return out, err
}

// SoftDelete 逻辑删除本人的上传 / 生成资产：**afv_asset 的第二处受控写入**（先例 pkg/asset.SoftDeleteByTask）。
// 只置 deleted = 1（与 Java @TableLogic 同口径，update_time 由表的 ON UPDATE 刷新），条件里再带一遍
// 归属 + 来源（source_type IN (1, 2)），官方素材即使混进 ids 也不会被删。不碰存储对象、任务结果与其它列。
func (r *AssetRepo) SoftDelete(ctx context.Context, userID int64, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return affected(r.db.NewUpdate().Table(model.TableAsset).
		Set("deleted = 1").
		Where("id IN (?)", bun.In(ids)).Where("user_id = ?", userID).Where("deleted = 0").
		Where("source_type IN (?, ?)", model.SourceTypeUpload, model.SourceTypeGenerated).
		Exec(ctx))
}

// KindCount 分类计数的一行（按有效分类）；Kind 为 NULL 即未分类。
// 列别名用 eff_kind：MySQL 的 GROUP BY 先在 FROM 里找名字，叫 kind 会被解析成 e.kind。
type KindCount struct {
	Kind   sql.NullString `bun:"eff_kind"`
	Source string         `bun:"source"`
	N      int64          `bun:"n"`
}

// Categories 按有效分类 GROUP BY（NULL = 未分类），计数随 source / media（Filter 里其它条件也生效）。
func (r *AssetRepo) Categories(ctx context.Context, f Filter) ([]KindCount, error) {
	var out []KindCount
	ke, kargs := r.kindExpr()
	q := r.db.NewSelect().TableExpr("afv_asset AS a").
		Join("LEFT JOIN afv_asset_ext AS e ON e.asset_id = a.id").
		ColumnExpr(ke+" AS eff_kind", kargs...).ColumnExpr("COUNT(*) AS n")
	f.Kind = ""
	f.KindGroup = ""
	err := r.apply(q, f).GroupExpr("eff_kind").Scan(ctx, &out)
	return out, err
}

// SysTagCount 系统标签字典的一行。
type SysTagCount struct {
	Name string `bun:"name"`
	N    int64  `bun:"n"`
}

// SysTagCounts 本人资产（未删除、非草稿）里出现过的系统标签及资产数，按数量倒序、名字升序，最多 limit 个。
func (r *AssetRepo) SysTagCounts(ctx context.Context, userID int64, limit int64) ([]SysTagCount, error) {
	var out []SysTagCount
	q := r.db.NewSelect().TableExpr("afv_asset AS a").
		Join("JOIN JSON_TABLE(a.sys_tags, '$[*]' COLUMNS (tag VARCHAR(64) PATH '$')) AS jt").
		ColumnExpr("jt.tag AS name, COUNT(*) AS n")
	err := owned(q, userID).Where("jt.tag IS NOT NULL AND jt.tag <> ''").
		GroupExpr("jt.tag").OrderExpr("n DESC, name ASC").Limit(limit).Scan(ctx, &out)
	return out, err
}

// AdminKindStats 后台：按有效分类 × 来源计数。userID=0 为全部用户；只排除已删除。
func (r *AssetRepo) AdminKindStats(ctx context.Context, userID int64, media string) ([]KindCount, error) {
	var out []KindCount
	ke, kargs := r.kindExpr()
	q := r.db.NewSelect().TableExpr("afv_asset AS a").
		Join("LEFT JOIN afv_asset_ext AS e ON e.asset_id = a.id").
		ColumnExpr(ke+" AS eff_kind", kargs...).
		ColumnExpr("CASE a.source_type WHEN 1 THEN 'upload' WHEN 2 THEN 'generated' ELSE 'official' END AS source").
		ColumnExpr("COUNT(*) AS n").
		Where("a.deleted = 0")
	if userID > 0 {
		q = q.Where("a.user_id = ?", userID)
	}
	c, args := r.videoCond()
	switch media {
	case "video":
		q = q.Where(c, args...)
	case "image":
		q = q.Where("(a.type IS NULL OR NOT "+c+")", args...)
	}
	err := q.GroupExpr("eff_kind, source").Scan(ctx, &out)
	return out, err
}

// Rename 改标题：**本模块唯一一处写 Java 表 afv_asset 的地方**。
// pkg/asset 没有改名方法，Java 的 PUT /api/v1/asset 需要用户令牌、Go 侧调不到，所以这里做一条受控 UPDATE：
// 只改 name 一列（update_time 由 Java 表的 ON UPDATE 自动刷新），带归属条件，不碰 status / source_type 等其它列。
// 返回 false 表示没有命中本人的资产（标题未变时 MySQL 也返回 0 行，调用方应先确认归属再调）。
func (r *AssetRepo) Rename(ctx context.Context, id, userID int64, name string) (bool, error) {
	n, err := affected(r.db.NewUpdate().Table(model.TableAsset).
		Set("name = ?", name).
		Where("id = ?", id).Where("user_id = ?", userID).Where("deleted = 0").
		Exec(ctx))
	return n > 0, err
}

// CreatedMs 资产创建时间（毫秒）。
func (a *AssetRow) CreatedMs() int64 {
	if a.CreateTime.IsZero() {
		return 0
	}
	return a.CreateTime.UnixMilli()
}
