package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
)

// 交接期（Plaza.AdminWriteEnabled=false，Java 仍是 afv_compat_plaza_template 的写者）可写的「A 类」接口：
// 只写 Go 独占的表 / 列，永不改 Java 也写的列（title / category / description / tags_json / data_json / sort_order / enabled / deleted / create_time，
// 以及 enabled 的派生 state / online_at / offline_at / deleted_at / deleted_by），并在模板行上冻结 update_time（见 keepUpdateTime）。
// 并发判据统一用 revision。清单与依据见 docs/api/api-plaza.md「交接期可写范围」。

// TemplateIndustriesInput PUT /templates/{id}/industries：给单个模板增删行业。
// assign / unassign 是增量（幂等，revision 可选）；industryIds 是整体替换（revision 必填；与 assign / unassign 互斥）。
type TemplateIndustriesInput struct {
	Assign      []int64  `json:"assign"`
	Unassign    []int64  `json:"unassign"`
	IndustryIDs *[]int64 `json:"industryIds"`
	Revision    *int     `json:"revision"`
}

// TemplateIndustriesResult 保存后的行业集合与新 revision（关系有变化时 +1；没变化不写、不记审计）。
type TemplateIndustriesResult struct {
	ID          int64   `json:"id"`
	IndustryIDs []int64 `json:"industryIds"`
	UpdatedAt   string  `json:"updatedAt"`
	Revision    int     `json:"revision"`
	Changed     bool    `json:"changed"`
}

func sameIDSet(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[int64]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

// AdminSetTemplateIndustries A 类写：只改 afv_plaza_template_industry（物理增删关系行，新挂入的排该行业末尾），模板行只 revision+1。
func (s *Service) AdminSetTemplateIndustries(ctx context.Context, op Operator, id int64, in TemplateIndustriesInput) (*TemplateIndustriesResult, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	replace := in.IndustryIDs != nil
	if replace && (len(in.Assign) > 0 || len(in.Unassign) > 0) {
		return nil, errParam("industryIds（整体替换）不能与 assign / unassign 同时传")
	}
	if !replace && len(in.Assign) == 0 && len(in.Unassign) == 0 {
		return nil, errParam("需要 assign / unassign 或 industryIds")
	}
	if replace && in.Revision == nil {
		return nil, xerr.New(xerr.ParamMissing, "整体替换行业需要 revision（带上读到的 revision）")
	}
	var out *TemplateIndustriesResult
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		rev := cur.Revision
		if in.Revision != nil {
			if rev, err = checkRevisionOnly(ctx, cur, in.Revision, ReasonConflict, "模板行业"); err != nil {
				return err
			}
		}
		before, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if replace {
			target, err := checkIndustryIDs(ctx, tx, *in.IndustryIDs)
			if err != nil {
				return err
			}
			if err := setIndustriesAll(ctx, tx, id, target); err != nil {
				return err
			}
		} else {
			add, err := checkIndustryIDs(ctx, tx, in.Assign)
			if err != nil {
				return err
			}
			if err := addIndustries(ctx, tx, id, add); err != nil {
				return err
			}
			if rm := dedupeIDs(in.Unassign); len(rm) > 0 {
				if _, err := tx.NewDelete().Model((*model.TemplateIndustry)(nil)).Where("template_id = ?", id).
					Where("industry_id IN (?)", bun.In(rm)).Exec(ctx); err != nil {
					return err
				}
			}
		}
		after, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		out = &TemplateIndustriesResult{ID: id, IndustryIDs: after, UpdatedAt: cur.UpdateTime.Format(timeLayout), Revision: cur.Revision}
		if sameIDSet(before, after) {
			return nil
		}
		if err := requireIndustryRelationPerm(ctx, op, cur, before, after); err != nil {
			return err // 回滚关系变更
		}
		if err := applyTemplateUpdate(ctx, tx, id, rev, nil, op.ID, s.keepUpdateTime()); err != nil {
			return staleConflict(ctx, err, id, rev, ReasonConflict, "模板行业")
		}
		upd, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out.Changed, out.UpdatedAt, out.Revision = true, upd.UpdateTime.Format(timeLayout), upd.Revision
		return s.audit(ctx, tx, op, "TEMPLATE_SET_INDUSTRIES", ResTemplate, fmt.Sprint(id), "",
			map[string]any{"industryIds": before}, map[string]any{"industryIds": after})
	})
	if err != nil {
		return nil, err
	}
	if out.Changed {
		s.Invalidate(ctx)
	}
	return out, nil
}

// TemplateAttributesInput PUT /templates/{id}/attrs：只改 Go 独占的模板属性。字段缺省 = 不改；
// channels 显式 null / [] = 清空；mediaType 改 image 时 videoUrl 被清空，改 video 时必须有 videoUrl。revision 必填。
type TemplateAttributesInput struct {
	Revision          *int            `json:"revision"`
	UpdatedAt         string          `json:"updatedAt"` // 仅完整模式且未带 revision 时作判据；交接期 update_time 冻结，必须带 revision
	MediaType         *string         `json:"mediaType"`
	VideoURL          *string         `json:"videoUrl"`
	PosterURL         *string         `json:"posterUrl"`
	Channels          json.RawMessage `json:"channels"`
	NeedsProductImage *bool           `json:"needsProductImage"`
	TextOverlay       *bool           `json:"textOverlay"`
}

// AdminSetTemplateAttributes A 类写：media_type / video_url / poster_url / channels_json / needs_product_image / text_overlay（均为 Go_V1.0.33 新增列，Java 从不读写）。
func (s *Service) AdminSetTemplateAttributes(ctx context.Context, op Operator, id int64, in TemplateAttributesInput) (*TemplateWriteResult, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	if in.MediaType == nil && in.VideoURL == nil && in.PosterURL == nil && len(in.Channels) == 0 && in.NeedsProductImage == nil && in.TextOverlay == nil {
		return nil, errParam("没有要修改的属性")
	}
	if in.MediaType != nil && *in.MediaType != model.MediaImage && *in.MediaType != model.MediaVideo {
		return nil, errParam("mediaType 只能是 image 或 video")
	}
	var out *TemplateWriteResult
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		var rev int
		if !s.handover() && in.Revision == nil && strings.TrimSpace(in.UpdatedAt) != "" {
			rev, err = checkConcurrency(ctx, cur, in.UpdatedAt, nil, ReasonConflict, "模板属性")
		} else {
			rev, err = checkRevisionOnly(ctx, cur, in.Revision, ReasonConflict, "模板属性")
		}
		if err != nil {
			return err
		}
		inds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		before := templateSnapshot(*cur, inds)
		media, video, poster := cur.MediaType, cur.VideoURL.String, cur.PosterURL.String
		if in.MediaType != nil {
			media = *in.MediaType
		}
		if in.VideoURL != nil {
			video = strings.TrimSpace(*in.VideoURL)
		}
		if in.PosterURL != nil {
			poster = strings.TrimSpace(*in.PosterURL)
		}
		if media == model.MediaImage {
			video = ""
		} else if video == "" {
			return errParam("视频模板必须填写预览视频")
		}
		set := map[string]any{
			"media_type": media,
			"video_url":  nullableStr(sql.NullString{String: video, Valid: video != ""}),
			"poster_url": nullableStr(sql.NullString{String: poster, Valid: poster != ""}),
		}
		if len(in.Channels) > 0 {
			dict, err := loadChannelDict(ctx, tx)
			if err != nil {
				return err
			}
			if err := validateChannels(in.Channels, dict); err != nil {
				return err
			}
			set["channels_json"] = nullableJSON(normalizeJSONArray(in.Channels))
		}
		if in.NeedsProductImage != nil {
			set["needs_product_image"] = b2i(*in.NeedsProductImage)
		}
		if in.TextOverlay != nil {
			set["text_overlay"] = b2i(*in.TextOverlay)
		}
		for k, v := range mediaChangeSet(cur, media, video, poster) {
			set[k] = v // 同整体编辑：换了预览视频 → PROCESSING，由后台任务处理
		}
		if err := applyTemplateUpdate(ctx, tx, id, rev, set, op.ID, s.keepUpdateTime()); err != nil {
			return staleConflict(ctx, err, id, rev, ReasonConflict, "模板属性")
		}
		after, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = &TemplateWriteResult{ID: id, UpdatedAt: after.UpdateTime.Format(timeLayout), Revision: after.Revision,
			Warnings: overlayPromptWarnings(after.TextOverlay, after.DataJSON)}
		return s.audit(ctx, tx, op, "TEMPLATE_UPDATE_ATTRS", ResTemplate, fmt.Sprint(id), "", before, templateSnapshot(*after, inds))
	})
	if err != nil {
		return nil, err
	}
	s.Invalidate(ctx)
	return out, nil
}

// UnassignedSummary 「未分配行业的模板」待处理量（后台首页角标）。
type UnassignedSummary struct {
	// Total 未进回收站、没挂任何行业的模板数（用户端按行业浏览，它们一律看不到）。
	Total int `json:"total"`
	// Enabled 其中 enabled=1 的（Java 侧已上线、只差挂行业就对用户可见）：交接期最该先处理的一批。
	Enabled int `json:"enabled"`
	// AdminWriteEnabled false = 交接期（模板基础字段仍由 Java 后台维护，这里只能写行业 / 排序 / 入参 / 渠道 / 媒体类型 / 示例素材）。
	AdminWriteEnabled bool `json:"adminWriteEnabled"`
}

// AdminUnassignedSummary 只读计数。清单走 GET /templates?unassigned=true。
func (s *Service) AdminUnassignedSummary(ctx context.Context) (*UnassignedSummary, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	q := func() *bun.SelectQuery {
		return s.db.NewSelect().Model((*model.Template)(nil)).Where("t.deleted = 0").
			Where("NOT EXISTS (SELECT 1 FROM afv_plaza_template_industry r WHERE r.template_id = t.id)")
	}
	total, err := q().Count(ctx)
	if err != nil {
		return nil, err
	}
	enabled, err := q().Where("t.enabled = 1").Count(ctx)
	if err != nil {
		return nil, err
	}
	return &UnassignedSummary{Total: int(total), Enabled: int(enabled), AdminWriteEnabled: s.cfg.AdminWriteEnabled}, nil
}

// Capabilities 后台能力探测：当前写权属模式下各类操作是否可写，前端据此置灰按钮而不是被拒一次才知道。
type Capabilities struct {
	AdminWriteEnabled bool `json:"adminWriteEnabled"`
	Writable          struct {
		Status           bool `json:"status"`           // 发布 / 下线（含批量）
		Recycle          bool `json:"recycle"`          // 删除 / 恢复 / 彻底删除（含批量）
		FullEdit         bool `json:"fullEdit"`         // 新建 / 整体编辑 / 复制
		Slots            bool `json:"slots"`            // 入参 schema
		IndustryRelation bool `json:"industryRelation"` // 模板挂 / 摘行业（单个与批量）
		IndustryOrder    bool `json:"industryOrder"`    // 行业内排序 / 置顶
		Attrs            bool `json:"attrs"`            // 渠道 / 媒体类型等
		Samples          bool `json:"samples"`          // 示例素材
		Uploads          bool `json:"uploads"`          // 模板素材上传（presign / confirm）：装配了存储才有；与写权属无关
	} `json:"writable"`
}

// AdminCapabilities 不查库。A 类写只要库在就可写；B 类随 AdminWriteEnabled。
func (s *Service) AdminCapabilities() Capabilities {
	var c Capabilities
	c.AdminWriteEnabled = s.cfg.AdminWriteEnabled
	ok := s.db != nil
	b := ok && s.cfg.AdminWriteEnabled
	c.Writable.Status, c.Writable.Recycle, c.Writable.FullEdit = b, b, b
	c.Writable.Slots, c.Writable.IndustryRelation, c.Writable.IndustryOrder, c.Writable.Attrs, c.Writable.Samples = ok, ok, ok, ok, ok
	c.Writable.Uploads = s.uploader != nil
	return c
}
