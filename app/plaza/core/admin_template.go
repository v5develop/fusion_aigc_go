package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	maxTitleRunes = 255
	maxDescRunes  = 1000
	maxBatchIDs   = 200
	// trashDays 回收站保留天数，到期由清理任务物理删除。
	trashDays = 30
	sortStep  = 10
)

// 后台展示用的状态（在 state 之上叠加窗口与回收站）。
const (
	DisplayDraft     = "DRAFT"
	DisplayPublished = "PUBLISHED"
	DisplayOffline   = "OFFLINE"
	DisplayScheduled = "SCHEDULED" // 已发布但上线时间未到
	DisplayExpired   = "EXPIRED"   // 已发布但已过下线时间
	DisplayDeleted   = "DELETED"
)

// displayState 后台列表的状态标签。
func displayState(t *model.Template, now time.Time) string {
	if t.Deleted == 1 {
		return DisplayDeleted
	}
	switch t.State {
	case model.StateDraft:
		return DisplayDraft
	case model.StateOffline:
		return DisplayOffline
	}
	if t.OnlineAt.Valid && now.Before(t.OnlineAt.Time) {
		return DisplayScheduled
	}
	if t.OfflineAt.Valid && !now.Before(t.OfflineAt.Time) {
		return DisplayExpired
	}
	return DisplayPublished
}

// TemplateInput 新建 / 编辑模板的入参。
// 编辑是「结构化字段整体替换」：title 必填；description / category / tags / mediaType / videoUrl / posterUrl / channels
// 缺省视为清空或回默认（同 Java）；但以下字段「省略 = 不改」：payload（保留原载荷，不让老页面保存时误清）、
// slots（保留原入参 schema；显式 null / [] / items 为空 = 清空；只改入参用 PUT /templates/{id}/slots）、
// industryIds（[] = 清空）、agentSkillId（"" = 解绑）、needsProductImage / textOverlay / sortOrder。
// data_json 里的元数据键（agentSkillId / v1PlazaTemplateId）始终由服务端保持。
type TemplateInput struct {
	Title             string          `json:"title"`
	Description       string          `json:"description"`
	Category          string          `json:"category"`
	Tags              []string        `json:"tags"`
	Payload           json.RawMessage `json:"payload"`
	AgentSkillID      *string         `json:"agentSkillId"`
	MediaType         string          `json:"mediaType"`
	VideoURL          string          `json:"videoUrl"`
	PosterURL         string          `json:"posterUrl"`
	Channels          json.RawMessage `json:"channels"`
	Slots             json.RawMessage `json:"slots"`
	NeedsProductImage *bool           `json:"needsProductImage"`
	TextOverlay       *bool           `json:"textOverlay"`
	SortOrder         *int            `json:"sortOrder"`
	// MediaStatus 仅作兼容保留（READY | PENDING | PROCESSING，值被忽略）：视频的处理状态由服务端按「视频地址是否变化」决定，
	// 换了视频 / 新建视频模板即进入 PROCESSING，由后台任务抽封面、转码，完成后置 READY。
	MediaStatus string   `json:"mediaStatus"`
	IndustryIDs *[]int64 `json:"industryIds"`
	// UpdatedAt 乐观并发（编辑必填，缺 → 400 PARAM_MISSING）：带上读到的 updateTime，冲突时提示「他人已修改，刷新后再试」。
	UpdatedAt string `json:"updatedAt"`
	// Revision 读到的 revision（可选）：带了就按它判冲突（精确到每次写），不带退回比 updatedAt（秒级）。
	Revision *int `json:"revision"`

	slotsGiven bool // 请求体里带了 slots 键（含显式 null）
}

// validateTemplateInput 校验并规整（不碰库）。
func validateTemplateInput(in *TemplateInput) error {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || runeLen(in.Title) > maxTitleRunes {
		return errParam("标题必填，最多 255 个字")
	}
	in.Description = strings.TrimSpace(in.Description)
	if runeLen(in.Description) > maxDescRunes {
		return errParam("说明最多 1000 个字")
	}
	in.Category = strings.TrimSpace(in.Category)
	if runeLen(in.Category) > 64 {
		return errParam("分类最多 64 个字")
	}
	if in.MediaType == "" {
		in.MediaType = model.MediaImage
	}
	if in.MediaType != model.MediaImage && in.MediaType != model.MediaVideo {
		return errParam("mediaType 只能是 image 或 video")
	}
	in.VideoURL, in.PosterURL = strings.TrimSpace(in.VideoURL), strings.TrimSpace(in.PosterURL)
	if in.MediaType == model.MediaVideo && in.VideoURL == "" {
		return errParam("视频模板必须填写预览视频")
	}
	if in.MediaType == model.MediaImage { // image 时忽略 videoUrl
		in.VideoURL = ""
	}
	in.slotsGiven = len(in.Slots) > 0        // json.RawMessage：键缺省为 nil，显式 null 为 "null"
	schema, err := ParseSlotSchema(in.Slots) // 严格校验入参 schema（带字段路径的错误），并规整成 {schemaVersion:1,items:[…]}
	if err != nil {
		return err
	}
	in.Slots = schema.Marshal()
	if len(in.Payload) > 0 {
		obj, err := requireJSONObject(in.Payload)
		if err != nil {
			return err
		}
		if err := validatePayload(obj); err != nil {
			return err
		}
	}
	if err := validateMediaURL("videoUrl", in.VideoURL); err != nil {
		return err
	}
	if err := validateMediaURL("posterUrl", in.PosterURL); err != nil {
		return err
	}
	in.MediaStatus = strings.ToUpper(strings.TrimSpace(in.MediaStatus))
	if in.MediaStatus != "" && in.MediaStatus != model.MediaStatusReady && in.MediaStatus != model.MediaStatusPending && in.MediaStatus != model.MediaStatusProcessing {
		return errParam("mediaStatus 只能是 READY、PENDING 或 PROCESSING")
	}
	return validateTags(in.Tags)
}

const (
	maxMediaURLLen   = 1000
	maxPayloadImages = 20
	maxGuidanceRunes = 1000
	maxPromptRunes   = 8000
	maxTags          = 20
	maxTagRunes      = 32
)

// validateMediaURL 空 = 不填；否则必须是 http(s) 地址（模板展示用，不接受 data: / javascript: 等）。
func validateMediaURL(field, v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxMediaURLLen || !validHTTPURL(v) {
		return errParam(field + " 必须是 http(s) 地址（最长 1000 字符）")
	}
	return nil
}

func validateTags(tags []string) error {
	if len(tags) > maxTags {
		return errParam(fmt.Sprintf("标签最多 %d 个", maxTags))
	}
	for _, t := range tags {
		if runeLen(strings.TrimSpace(t)) > maxTagRunes {
			return errParam(fmt.Sprintf("单个标签最多 %d 个字", maxTagRunes))
		}
	}
	return nil
}

// validatePayload 校验 payload 里编辑器管理的已知键（其余键原样放行）：result / image_urls 是 http(s) 地址数组，
// description / guidance / prompt 是字符串且有长度上限。键缺省不校验（载荷省略 = 保留原值由调用方处理）。
func validatePayload(obj json.RawMessage) error {
	for _, key := range []string{"result", "image_urls"} {
		v, ok := topValue(obj, key)
		if !ok || string(v) == "null" {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(v, &arr) != nil {
			return errParam("payload." + key + " 必须是地址数组")
		}
		if len(arr) > maxPayloadImages {
			return errParam(fmt.Sprintf("payload.%s 最多 %d 张", key, maxPayloadImages))
		}
		for i, a := range arr {
			var s string
			if json.Unmarshal(a, &s) != nil || !validHTTPURL(s) || len(s) > maxMediaURLLen {
				return errParam(fmt.Sprintf("payload.%s[%d] 必须是 http(s) 地址", key, i))
			}
		}
	}
	for key, limit := range map[string]int{"description": maxDescRunes, "guidance": maxGuidanceRunes, "prompt": maxPromptRunes} {
		v, ok := topValue(obj, key)
		if !ok || string(v) == "null" {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			return errParam("payload." + key + " 必须是文本")
		}
		if runeLen(s) > limit {
			return errParam(fmt.Sprintf("payload.%s 最多 %d 个字", key, limit))
		}
	}
	return nil
}

// resolveMediaStatus 新建时写入的 media_status：有预览视频的视频模板进入 PROCESSING（后台任务抽封面 / 转码，完成置 READY）；
// 图片模板、或视频模板暂时没填视频（草稿，发布检查本来就要求视频地址）为 READY。
func resolveMediaStatus(mediaType, videoURL string) string {
	if mediaType == model.MediaVideo && strings.TrimSpace(videoURL) != "" {
		return model.MediaStatusProcessing
	}
	return model.MediaStatusReady
}

func templateSnapshot(t model.Template, industryIDs []int64) map[string]any {
	m := map[string]any{
		"id": t.ID, "title": t.Title, "description": strPtr(t.Description), "category": strPtr(t.Category),
		"tags": readTags(t.TagsJSON), "payload": json.RawMessage(payloadForDisplay(t.DataJSON)),
		"agentSkillId": nilIfEmpty(skillID(t.DataJSON)), "mediaType": t.MediaType,
		"videoUrl": strPtr(t.VideoURL), "posterUrl": strPtr(t.PosterURL), "state": t.State,
		"onlineAt": timeStr(t.OnlineAt), "offlineAt": timeStr(t.OfflineAt),
		"channels": rawOrNil(t.ChannelsJSON), "slots": rawOrNil(t.SlotsJSON),
		"needsProductImage": t.NeedsProductImage == 1, "textOverlay": t.TextOverlay == 1,
		"sortOrder": t.SortOrder, "enabled": t.Enabled == 1, "deleted": t.Deleted == 1, "mediaStatus": t.MediaStatus,
	}
	if industryIDs != nil {
		m["industryIds"] = industryIDs
	}
	return m
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func rawOrNil(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return r
}

// checkSkill 技能绑定必须指向技能目录里真实存在的技能（只读 Java 表）：指向不存在的 id 不会报错，
// 只会让用户选中后静默退回默认技能。
func checkSkill(ctx context.Context, db bun.IDB, id string) error {
	if id == "" {
		return nil
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return errParam("agentSkillId 必须是数字 id（字符串）")
	}
	cnt, err := db.NewSelect().TableExpr("afv_compat_agent_skill").Where("agent_skill_id = ?", n).Where("deleted = 0").Count(ctx)
	if err != nil {
		return err
	}
	if cnt == 0 {
		return xerr.New(xerr.NotFound, "技能不存在（agentSkillId="+id+"）")
	}
	return nil
}

// loadChannelDict 渠道字典（保存时校验用）。
func loadChannelDict(ctx context.Context, db bun.IDB) (map[string]model.Channel, error) {
	var rows []model.Channel
	if err := db.NewSelect().Model(&rows).Where("ch.deleted = 0").Scan(ctx); err != nil {
		return nil, err
	}
	m := make(map[string]model.Channel, len(rows))
	for _, r := range rows {
		m[r.Code] = r
	}
	return m, nil
}

func industryIDsOf(ctx context.Context, db bun.IDB, templateID int64) ([]int64, error) {
	var ids []int64
	err := db.NewSelect().Model((*model.TemplateIndustry)(nil)).Column("industry_id").
		Where("template_id = ?", templateID).Order("industry_id ASC").Scan(ctx, &ids)
	if ids == nil {
		ids = []int64{}
	}
	return ids, err
}

// checkIndustryIDs industryIds 去重、全部存在且未删除。
func checkIndustryIDs(ctx context.Context, db bun.IDB, ids []int64) ([]int64, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return ids, nil
	}
	n, err := db.NewSelect().Model((*model.Industry)(nil)).Where("id IN (?)", bun.In(ids)).Where("deleted = 0").Count(ctx)
	if err != nil {
		return nil, err
	}
	if int(n) != len(ids) {
		return nil, xerr.NewReason(xerr.NotFound, ReasonIndustryNotFound, "有行业不存在或已删除")
	}
	return ids, nil
}

// maxIndustrySort 行业内当前最大 sort_order（无则 0）。
func maxIndustrySort(ctx context.Context, db bun.IDB, industryID int64) (int, error) {
	var m sql.NullInt64
	err := db.NewSelect().Model((*model.TemplateIndustry)(nil)).ColumnExpr("MAX(sort_order)").Where("industry_id = ?", industryID).Scan(ctx, &m)
	return int(m.Int64), err
}

func minIndustrySort(ctx context.Context, db bun.IDB, industryID int64) (int, error) {
	var m sql.NullInt64
	err := db.NewSelect().Model((*model.TemplateIndustry)(nil)).ColumnExpr("MIN(sort_order)").Where("industry_id = ?", industryID).Scan(ctx, &m)
	return int(m.Int64), err
}

// addIndustries 追加行业关系（已存在的跳过），新挂入的排该行业末尾。
func addIndustries(ctx context.Context, tx bun.IDB, templateID int64, ids []int64) error {
	for _, id := range ids {
		n, err := tx.NewSelect().Model((*model.TemplateIndustry)(nil)).Where("template_id = ?", templateID).Where("industry_id = ?", id).Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		max, err := maxIndustrySort(ctx, tx, id)
		if err != nil {
			return err
		}
		row := model.TemplateIndustry{TemplateID: templateID, IndustryID: id, SortOrder: max + sortStep}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// setIndustriesAll 整体替换模板的行业关系：保留仍在列表里的关系行（含 sort_order），去掉不在的，新挂入的排该行业末尾（最大值 + 10）。
func setIndustriesAll(ctx context.Context, tx bun.IDB, templateID int64, ids []int64) error {
	if _, err := tx.NewDelete().Model((*model.TemplateIndustry)(nil)).
		Where("template_id = ?", templateID).Where("industry_id NOT IN (?)", bun.In(append([]int64{0}, ids...))).Exec(ctx); err != nil {
		return err
	}
	return addIndustries(ctx, tx, templateID, ids)
}

// ---------------------------------------------------------------- 新建 / 编辑 / 复制

// AdminCreateTemplate 新建（默认 DRAFT）。
func (s *Service) AdminCreateTemplate(ctx context.Context, op Operator, in TemplateInput) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	if err := validateTemplateInput(&in); err != nil {
		return 0, err
	}
	var id int64
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		skill := ""
		if in.AgentSkillID != nil {
			skill = strings.TrimSpace(*in.AgentSkillID)
		}
		if err := checkSkill(ctx, tx, skill); err != nil {
			return err
		}
		dict, err := loadChannelDict(ctx, tx)
		if err != nil {
			return err
		}
		if err := validateChannels(in.Channels, dict); err != nil {
			return err
		}
		var inds []int64
		if in.IndustryIDs != nil {
			if inds, err = checkIndustryIDs(ctx, tx, *in.IndustryIDs); err != nil {
				return err
			}
		}
		data, err := composeData(in.Payload, skill, nil)
		if err != nil {
			return err
		}
		row := model.Template{
			Title: in.Title, DataJSON: data, TagsJSON: writeTags(in.Tags), MediaType: in.MediaType,
			State: model.StateDraft, MediaStatus: resolveMediaStatus(in.MediaType, in.VideoURL), NeedsProductImage: 1,
			ChannelsJSON: normalizeJSONArray(in.Channels), SlotsJSON: normalizeJSONArray(in.Slots),
		}
		if in.Description != "" {
			row.Description = sql.NullString{String: in.Description, Valid: true}
		}
		if in.Category != "" {
			row.Category = sql.NullString{String: in.Category, Valid: true}
		}
		if in.VideoURL != "" {
			row.VideoURL = sql.NullString{String: in.VideoURL, Valid: true}
		}
		if in.PosterURL != "" {
			row.PosterURL = sql.NullString{String: in.PosterURL, Valid: true}
		}
		if in.NeedsProductImage != nil {
			row.NeedsProductImage = b2i(*in.NeedsProductImage)
		}
		if in.TextOverlay != nil {
			row.TextOverlay = b2i(*in.TextOverlay)
		}
		if in.SortOrder != nil {
			row.SortOrder = *in.SortOrder
		}
		if op.ID > 0 {
			row.CreatedBy = sql.NullInt64{Int64: op.ID, Valid: true}
			row.UpdatedBy = row.CreatedBy
		}
		row.Enabled = 0 // 草稿对用户不可见
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		id = row.ID
		if err := addIndustries(ctx, tx, id, inds); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "TEMPLATE_CREATE", ResTemplate, fmt.Sprint(id), "", nil, templateSnapshot(row, inds))
	})
	if err != nil {
		return 0, err
	}
	s.Invalidate(ctx)
	return id, nil
}

// normalizeJSONArray 空 / null → nil（写 NULL）。
func normalizeJSONArray(raw json.RawMessage) json.RawMessage {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" || t == "[]" {
		return nil
	}
	return raw
}

func lockTemplate(ctx context.Context, tx bun.IDB, id int64, includeDeleted bool) (*model.Template, error) {
	var t model.Template
	q := tx.NewSelect().Model(&t).Where("t.id = ?", id).For("UPDATE")
	if !includeDeleted {
		q = q.Where("t.deleted = 0")
	}
	if err := q.Scan(ctx); err != nil {
		if noRows(err) {
			return nil, xerr.NewReason(xerr.NotFound, ReasonTemplateNotFound, "模板不存在")
		}
		return nil, err
	}
	return &t, nil
}

// applyTemplateUpdate 定向列更新（不整行写回，避免默认值列连带覆盖）：只写 set 里的列与 updated_by，
// 不重算 enabled（编辑不改 state / 上下线窗口，enabled 由发布 / 下线 / 边界任务维护）。
// 乐观并发：带 `AND revision = ?` 并把 revision +1，影响 0 行 = 他人已改（返回 errRevisionStale，由调用方换成各自的冲突码）。
//
// keepUpdateTime=true（交接期 A 类写）时追加 `update_time = update_time`，抑制 ON UPDATE CURRENT_TIMESTAMP：并发判据只用 revision。
func applyTemplateUpdate(ctx context.Context, tx bun.IDB, id int64, revision int, set map[string]any, opID int64, keepUpdateTime bool) error {
	q := tx.NewUpdate().Model((*model.Template)(nil)).Where("id = ?", id).Where("revision = ?", revision).
		Set("revision = revision + 1")
	if keepUpdateTime {
		q = q.Set("update_time = update_time")
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q = q.Set("? = ?", bun.Ident(k), set[k])
	}
	if opID > 0 {
		q = q.Set("updated_by = ?", opID)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errRevisionStale
	}
	return nil
}

// errRevisionStale 条件更新 `WHERE id = ? AND revision = ?` 影响 0 行。
var errRevisionStale = errors.New("plaza: template revision stale")

// TemplateWriteResult 编辑接口的返回：新的 updatedAt 与 revision（下次保存带上）。
type TemplateWriteResult struct {
	ID        int64  `json:"id"`
	UpdatedAt string `json:"updatedAt"`
	Revision  int    `json:"revision"`
	// Warnings 不拦截保存的后台提示（如文字叠加模板的提示词缺「不要出现文字」约束）。
	Warnings []string `json:"warnings,omitempty"`
}

// checkConcurrency 编辑接口的乐观并发前置校验（行已 FOR UPDATE 锁住）。updatedAt 必填；
// 请求带了 revision 就只比 revision（精确，不受秒级精度影响），否则比 updatedAt（只传 updatedAt 的老前端）。
// 返回本次条件更新要用的期望 revision。拒收日志写明判据。
func checkConcurrency(ctx context.Context, cur *model.Template, updatedAt string, revision *int, reason, api string) (int, error) {
	if strings.TrimSpace(updatedAt) == "" {
		logx.WithContext(ctx).Infof("plaza: %s拒收 判据=updatedAt 缺失（编辑接口必填） 模板=%d", api, cur.ID)
		return 0, xerr.New(xerr.ParamMissing, "updatedAt 必填（带上读到的 updateTime）")
	}
	want, perr := parseAdminTime(updatedAt)
	if perr != nil {
		return 0, perr
	}
	if revision != nil {
		if *revision != cur.Revision {
			logx.WithContext(ctx).Infof("plaza: %s拒收 判据=revision 不一致 模板=%d 带来=%d 库里=%d", api, cur.ID, *revision, cur.Revision)
			return 0, xerr.NewReason(xerr.Conflict, reason, "他人已修改，刷新后再试")
		}
		return *revision, nil
	}
	if want.Valid && !sameSecond(cur.UpdateTime, want.Time) {
		logx.WithContext(ctx).Infof("plaza: %s拒收 判据=updatedAt 不一致 模板=%d 带来=%s 库里=%s",
			api, cur.ID, updatedAt, cur.UpdateTime.Format(timeLayout))
		return 0, xerr.NewReason(xerr.Conflict, reason, "他人已修改，刷新后再试")
	}
	return cur.Revision, nil
}

// staleConflict 条件更新影响 0 行时换成接口的冲突码。
func staleConflict(ctx context.Context, err error, id int64, rev int, reason, api string) error {
	if errors.Is(err, errRevisionStale) {
		logx.WithContext(ctx).Infof("plaza: %s拒收 判据=条件更新 WHERE id=%d AND revision=%d 影响 0 行（并发写入）", api, id, rev)
		return xerr.NewReason(xerr.Conflict, reason, "他人已修改，刷新后再试")
	}
	return err
}

// AdminUpdateTemplate 编辑（整体 PUT）。updatedAt 必填，revision 可选（带了优先按它判冲突）；返回新的 updatedAt / revision。
func (s *Service) AdminUpdateTemplate(ctx context.Context, op Operator, id int64, in TemplateInput) (*TemplateWriteResult, error) {
	if err := s.requireWrite(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.UpdatedAt) == "" {
		logx.WithContext(ctx).Infof("plaza: 模板编辑拒收 判据=updatedAt 缺失（编辑接口必填） 模板=%d", id)
		return nil, xerr.New(xerr.ParamMissing, "updatedAt 必填（带上读到的 updateTime）")
	}
	if err := validateTemplateInput(&in); err != nil {
		return nil, err
	}
	var out *TemplateWriteResult
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		rev, err := checkConcurrency(ctx, cur, in.UpdatedAt, in.Revision, ReasonTemplateConflict, "模板编辑")
		if err != nil {
			return err
		}
		beforeInds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		before := templateSnapshot(*cur, beforeInds)

		skill := skillID(cur.DataJSON)
		if in.AgentSkillID != nil {
			skill = strings.TrimSpace(*in.AgentSkillID)
			if err := checkSkill(ctx, tx, skill); err != nil {
				return err
			}
		}
		payload := in.Payload
		if len(payload) == 0 { // 省略 = 保留原载荷
			payload = payloadForDisplay(cur.DataJSON)
		}
		data, err := composeData(payload, skill, cur.DataJSON)
		if err != nil {
			return err
		}
		dict, err := loadChannelDict(ctx, tx)
		if err != nil {
			return err
		}
		if err := validateChannels(in.Channels, dict); err != nil {
			return err
		}
		set := map[string]any{
			"title": in.Title, "category": nullableStr(sql.NullString{String: in.Category, Valid: in.Category != ""}),
			"description": nullableStr(sql.NullString{String: in.Description, Valid: in.Description != ""}),
			"tags_json":   string(writeTags(in.Tags)), "data_json": string(data), "media_type": in.MediaType,
			"video_url":     nullableStr(sql.NullString{String: in.VideoURL, Valid: in.VideoURL != ""}),
			"poster_url":    nullableStr(sql.NullString{String: in.PosterURL, Valid: in.PosterURL != ""}),
			"channels_json": nullableJSON(normalizeJSONArray(in.Channels)),
		}
		if in.slotsGiven { // 省略 slots = 保留原入参 schema
			set["slots_json"] = nullableJSON(normalizeJSONArray(in.Slots))
		}
		if in.NeedsProductImage != nil {
			set["needs_product_image"] = b2i(*in.NeedsProductImage)
		}
		if in.TextOverlay != nil {
			set["text_overlay"] = b2i(*in.TextOverlay)
		}
		if in.SortOrder != nil {
			set["sort_order"] = *in.SortOrder
		}
		afterInds := beforeInds
		if in.IndustryIDs != nil {
			if afterInds, err = checkIndustryIDs(ctx, tx, *in.IndustryIDs); err != nil {
				return err
			}
		}
		for k, v := range mediaChangeSet(cur, in.MediaType, in.VideoURL, in.PosterURL) {
			set[k] = v // 换了视频 → PROCESSING 并清空流水线状态；视频没动且只补了封面的 FAILED 行 → 重新排队
		}
		if err := applyTemplateUpdate(ctx, tx, id, rev, set, op.ID, false); err != nil {
			return staleConflict(ctx, err, id, rev, ReasonTemplateConflict, "模板编辑")
		}
		if in.IndustryIDs != nil {
			if err := setIndustriesAll(ctx, tx, id, afterInds); err != nil {
				return err
			}
		}
		after, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = &TemplateWriteResult{ID: id, UpdatedAt: after.UpdateTime.Format(timeLayout), Revision: after.Revision,
			Warnings: overlayPromptWarnings(after.TextOverlay, after.DataJSON)}
		return s.audit(ctx, tx, op, "TEMPLATE_UPDATE", ResTemplate, fmt.Sprint(id), "", before, templateSnapshot(*after, afterInds))
	})
	if err != nil {
		return nil, err
	}
	s.Invalidate(ctx)
	return out, nil
}

func sameSecond(a, b time.Time) bool { return a.Unix() == b.Unix() }

// ReasonConflict 只改入参接口的乐观并发冲突（4002）：模板在你打开之后被他人改过。
const ReasonConflict = "CONFLICT"

// TemplateSlotsInput PUT /templates/{id}/slots：只改入参 schema。
// Slots 为 {schemaVersion, items}（兼容旧键 slots / 裸数组）；null / [] / items 为空 = 清空。
// UpdatedAt 必填（缺 → 400 PARAM_MISSING），为读到的 updateTime；Revision 可选，带了优先按它判冲突；不一致 → 4002 reason=CONFLICT。
type TemplateSlotsInput struct {
	Slots     json.RawMessage `json:"slots"`
	UpdatedAt string          `json:"updatedAt"`
	Revision  *int            `json:"revision"`
}

// TemplateSlotsResult 保存后的入参 schema（规整后，无则 null）与新的 updateTime / revision（下次保存带上）。
type TemplateSlotsResult struct {
	ID        int64           `json:"id"`
	Slots     json.RawMessage `json:"slots"`
	UpdatedAt string          `json:"updatedAt"`
	Revision  int             `json:"revision"`
}

// AdminUpdateTemplateSlots 只改入参 schema（不动其它字段）。校验失败返回 *SlotsError（1003 · SLOTS_INVALID，data.field 为路径）。
// A 类写（slots_json 是 Go 独占列）：交接期也放行；交接期 revision 必填（update_time 被冻结，不能再当并发判据），且不改 update_time。
func (s *Service) AdminUpdateTemplateSlots(ctx context.Context, op Operator, id int64, in TemplateSlotsInput) (*TemplateSlotsResult, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	if s.handover() {
		if in.Revision == nil {
			logx.WithContext(ctx).Infof("plaza: 入参保存拒收 判据=交接期 revision 缺失（update_time 冻结，只按 revision 判冲突） 模板=%d", id)
			return nil, xerr.New(xerr.ParamMissing, "revision 必填（带上读到的 revision）")
		}
	} else if strings.TrimSpace(in.UpdatedAt) == "" {
		logx.WithContext(ctx).Infof("plaza: 入参保存拒收 判据=updatedAt 缺失（编辑接口必填） 模板=%d", id)
		return nil, xerr.New(xerr.ParamMissing, "updatedAt 必填（带上读到的 updateTime）")
	}
	schema, err := ParseSlotSchema(in.Slots)
	if err != nil {
		return nil, err
	}
	stored := schema.Marshal()
	var out *TemplateSlotsResult
	err = bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		var rev int
		if s.handover() {
			rev, err = checkRevisionOnly(ctx, cur, in.Revision, ReasonConflict, "入参保存")
		} else {
			rev, err = checkConcurrency(ctx, cur, in.UpdatedAt, in.Revision, ReasonConflict, "入参保存")
		}
		if err != nil {
			return err
		}
		inds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		before := templateSnapshot(*cur, inds)
		if err := applyTemplateUpdate(ctx, tx, id, rev, map[string]any{"slots_json": nullableJSON(stored)}, op.ID, s.keepUpdateTime()); err != nil {
			return staleConflict(ctx, err, id, rev, ReasonConflict, "入参保存")
		}
		after, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = &TemplateSlotsResult{ID: id, Slots: rawOrNilJSON(after.SlotsJSON), UpdatedAt: after.UpdateTime.Format(timeLayout), Revision: after.Revision}
		return s.audit(ctx, tx, op, "TEMPLATE_UPDATE_SLOTS", ResTemplate, fmt.Sprint(id), "", before, templateSnapshot(*after, inds))
	})
	if err != nil {
		return nil, err
	}
	s.Invalidate(ctx)
	return out, nil
}

// checkRevisionOnly 交接期 A 类写的并发前置校验：只比 revision（行已 FOR UPDATE 锁住）。
func checkRevisionOnly(ctx context.Context, cur *model.Template, revision *int, reason, api string) (int, error) {
	if revision == nil {
		return 0, xerr.New(xerr.ParamMissing, "revision 必填（带上读到的 revision）")
	}
	if *revision != cur.Revision {
		logx.WithContext(ctx).Infof("plaza: %s拒收 判据=revision 不一致 模板=%d 带来=%d 库里=%d", api, cur.ID, *revision, cur.Revision)
		return 0, xerr.NewReason(xerr.Conflict, reason, "他人已修改，刷新后再试")
	}
	return *revision, nil
}

// AdminDuplicateTemplate 复制：标题加「（副本）」、DRAFT、行业 / 素材 / 技能 / 渠道 / 可替换位一并复制，排在末尾。
func (s *Service) AdminDuplicateTemplate(ctx context.Context, op Operator, id int64) (int64, error) {
	if err := s.requireWrite(); err != nil {
		return 0, err
	}
	var newID int64
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		src, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		row := *src
		row.ID = 0
		row.Title = truncateRunes(src.Title, maxTitleRunes-4) + "（副本）"
		row.State, row.Enabled = model.StateDraft, 0
		row.OnlineAt, row.OfflineAt = sql.NullTime{}, sql.NullTime{}
		row.DeletedAt, row.DeletedBy, row.Deleted = sql.NullTime{}, sql.NullInt64{}, 0
		var maxSort sql.NullInt64
		if err := tx.NewSelect().Model((*model.Template)(nil)).ColumnExpr("MAX(sort_order)").Where("deleted = 0").Scan(ctx, &maxSort); err != nil {
			return err
		}
		row.SortOrder = int(maxSort.Int64) + sortStep
		if op.ID > 0 {
			row.CreatedBy = sql.NullInt64{Int64: op.ID, Valid: true}
			row.UpdatedBy = row.CreatedBy
		}
		row.CreateTime, row.UpdateTime = time.Time{}, time.Time{}
		row.Revision = 0
		if _, err := tx.NewInsert().Model(&row).ExcludeColumn("online_at", "offline_at", "deleted_at", "deleted_by").Exec(ctx); err != nil {
			return err
		}
		newID = row.ID
		inds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := addIndustries(ctx, tx, newID, inds); err != nil {
			return err
		}
		if _, err := tx.NewRaw("INSERT INTO afv_plaza_sample_asset (scope_type, industry_id, template_id, kind, url, thumb_url, title, role, sort_order, enabled, created_by) "+
			"SELECT scope_type, industry_id, ?, kind, url, thumb_url, title, role, sort_order, enabled, ? FROM afv_plaza_sample_asset "+
			"WHERE scope_type = ? AND template_id = ? AND deleted = 0", newID, nullInt(op.ID), model.SampleScopeTemplate, id).Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "TEMPLATE_DUPLICATE", ResTemplate, fmt.Sprint(newID), "", map[string]any{"sourceId": id}, templateSnapshot(row, inds))
	})
	if err != nil {
		return 0, err
	}
	s.Invalidate(ctx)
	return newID, nil
}

func nullInt(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---------------------------------------------------------------- 生命周期

// PublishInput 发布 / 定时上线 / 下线的入参。onlineAt / offlineAt 空串 = 立即 / 不限。
type PublishInput struct {
	OnlineAt  string `json:"onlineAt"`
	OfflineAt string `json:"offlineAt"`
	// Force 越过「没有绑定行业」的检查（会发布，但用户端不可见，返回警告）。
	Force bool `json:"force"`
}

// PublishResult 发布结果。
type PublishResult struct {
	ID       int64    `json:"id"`
	State    string   `json:"state"`
	Warnings []string `json:"warnings"`
}

// checkPublishable 发布前检查：有标题、一张效果图、视频模板有预览视频且转码就绪，窗口先后合理。
// 绑定行业的检查另行处理（可 force）。
func checkPublishable(t *model.Template, onlineAt, offlineAt sql.NullTime) error {
	if strings.TrimSpace(t.Title) == "" {
		return errParam("发布需要标题")
	}
	if firstResult(t.DataJSON) == "" {
		return errParam("发布需要至少一张效果图")
	}
	if t.MediaType == model.MediaVideo {
		if !t.VideoURL.Valid || t.VideoURL.String == "" {
			return errParam("视频模板必须填写预览视频")
		}
		switch t.MediaStatus {
		case model.MediaStatusReady:
		case model.MediaStatusFailed:
			msg := "视频处理失败，请点「重新处理」或更换视频后再发布"
			if t.MediaError.Valid && t.MediaError.String != "" {
				msg = "视频处理失败（" + t.MediaError.String + "），请点「重新处理」或更换视频后再发布"
			}
			return errParam(msg)
		default:
			return errParam("视频还在处理中（抽封面 / 转码），处理完成后才能发布")
		}
	}
	if onlineAt.Valid && offlineAt.Valid && !offlineAt.Time.After(onlineAt.Time) {
		return errParam("下线时间必须晚于上线时间")
	}
	return nil
}

// AdminPublishTemplate 发布；带 onlineAt / offlineAt 即定时上线 / 下线。
func (s *Service) AdminPublishTemplate(ctx context.Context, op Operator, id int64, in PublishInput) (*PublishResult, error) {
	if err := s.requireWrite(); err != nil {
		return nil, err
	}
	return s.publish(ctx, op, id, in, "")
}

func (s *Service) publish(ctx context.Context, op Operator, id int64, in PublishInput, batchID string) (*PublishResult, error) {
	onAt, err := parseAdminTime(in.OnlineAt)
	if err != nil {
		return nil, err
	}
	offAt, err := parseAdminTime(in.OfflineAt)
	if err != nil {
		return nil, err
	}
	res := &PublishResult{ID: id, State: model.StatePublished, Warnings: []string{}}
	err = bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := checkPublishable(cur, onAt, offAt); err != nil {
			return err
		}
		inds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(inds) == 0 {
			if !in.Force {
				return errParam("发布前请先绑定行业（没有行业的模板用户端看不到）")
			}
			res.Warnings = append(res.Warnings, "没有绑定行业：已发布，但用户端暂不可见")
		}
		res.Warnings = append(res.Warnings, overlayPromptWarnings(cur.TextOverlay, cur.DataJSON)...)
		before := templateSnapshot(*cur, inds)
		now := s.now()
		enabled := recomputeEnabled(model.StatePublished, onAt, offAt, false, now)
		q := tx.NewUpdate().Model((*model.Template)(nil)).Where("id = ?", id).Set("revision = revision + 1").
			Set("state = ?", model.StatePublished).Set("enabled = ?", enabled)
		q = setNullableTime(q, "online_at", onAt)
		q = setNullableTime(q, "offline_at", offAt)
		if op.ID > 0 {
			q = q.Set("updated_by = ?", op.ID)
		}
		if _, err := q.Exec(ctx); err != nil {
			return err
		}
		after := templateSnapshot(*cur, inds)
		after["state"], after["enabled"] = model.StatePublished, enabled == 1
		after["onlineAt"], after["offlineAt"] = timeStr(onAt), timeStr(offAt)
		action := "TEMPLATE_PUBLISH"
		if onAt.Valid && onAt.Time.After(now) {
			action = "TEMPLATE_SCHEDULE"
		}
		return s.audit(ctx, tx, op, action, ResTemplate, fmt.Sprint(id), batchID, before, after)
	})
	if err != nil {
		return nil, err
	}
	s.Invalidate(ctx)
	return res, nil
}

// setNullableTime 把 DATETIME 列写成「北京时间的某一刻」或 NULL：用 FROM_UNIXTIME 交给库按会话时区换算
// （bun 的方言按 UTC 内联 time.Time，与 DSN 的 +08:00 会话时区不一致，直接传 time.Time 会差 8 小时）。
func setNullableTime(q *bun.UpdateQuery, col string, t sql.NullTime) *bun.UpdateQuery {
	if !t.Valid {
		return q.Set("? = NULL", bun.Ident(col))
	}
	return q.Set("? = FROM_UNIXTIME(? / 1000)", bun.Ident(col), t.Time.UnixMilli())
}

// AdminOfflineTemplate 下线。
func (s *Service) AdminOfflineTemplate(ctx context.Context, op Operator, id int64) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	return s.offline(ctx, op, id, "")
}

func (s *Service) offline(ctx context.Context, op Operator, id int64, batchID string) error {
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		inds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		before := templateSnapshot(*cur, inds)
		q := tx.NewUpdate().Model((*model.Template)(nil)).Where("id = ?", id).Set("revision = revision + 1").
			Set("state = ?", model.StateOffline).Set("enabled = 0")
		if op.ID > 0 {
			q = q.Set("updated_by = ?", op.ID)
		}
		if _, err := q.Exec(ctx); err != nil {
			return err
		}
		after := templateSnapshot(*cur, inds)
		after["state"], after["enabled"] = model.StateOffline, false
		return s.audit(ctx, tx, op, "TEMPLATE_OFFLINE", ResTemplate, fmt.Sprint(id), batchID, before, after)
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}

// AdminDeleteTemplate 软删除（进回收站）：deleted=1、deleted_at、deleted_by，状态不变；enabled 置 0（Java 老读者立刻看不到）。
func (s *Service) AdminDeleteTemplate(ctx context.Context, op Operator, id int64) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	return s.softDelete(ctx, op, id, "")
}

func (s *Service) softDelete(ctx context.Context, op Operator, id int64, batchID string) error {
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		inds, _ := industryIDsOf(ctx, tx, id)
		before := templateSnapshot(*cur, inds)
		q := tx.NewUpdate().Model((*model.Template)(nil)).Where("id = ?", id).Set("revision = revision + 1").
			Set("deleted = 1").Set("enabled = 0").Set("deleted_at = NOW()")
		if op.ID > 0 {
			q = q.Set("deleted_by = ?", op.ID)
		}
		if _, err := q.Exec(ctx); err != nil {
			return err
		}
		after := templateSnapshot(*cur, inds)
		after["deleted"], after["enabled"] = true, false
		return s.audit(ctx, tx, op, "TEMPLATE_DELETE", ResTemplate, fmt.Sprint(id), batchID, before, after)
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}

// AdminRestoreTemplate 从回收站恢复：状态置为 OFFLINE，不会自动重新上线。
func (s *Service) AdminRestoreTemplate(ctx context.Context, op Operator, id int64) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	return s.restore(ctx, op, id, "")
}

func (s *Service) restore(ctx context.Context, op Operator, id int64, batchID string) error {
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if cur.Deleted == 0 {
			return errParam("模板不在回收站里")
		}
		inds, _ := industryIDsOf(ctx, tx, id)
		before := templateSnapshot(*cur, inds)
		q := tx.NewUpdate().Model((*model.Template)(nil)).Where("id = ?", id).Set("revision = revision + 1").
			Set("deleted = 0").Set("state = ?", model.StateOffline).Set("enabled = 0").
			Set("deleted_at = NULL").Set("deleted_by = NULL")
		if op.ID > 0 {
			q = q.Set("updated_by = ?", op.ID)
		}
		if _, err := q.Exec(ctx); err != nil {
			return err
		}
		after := templateSnapshot(*cur, inds)
		after["deleted"], after["state"], after["enabled"] = false, model.StateOffline, false
		return s.audit(ctx, tx, op, "TEMPLATE_RESTORE", ResTemplate, fmt.Sprint(id), batchID, before, after)
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}

// HeadlineCleaner 彻底删除模板时清掉它的头条规则的调用点（头条域由另一个模块负责，这里只留钩子；nil = 不做）。
type HeadlineCleaner func(ctx context.Context, tx bun.IDB, templateID int64) error

// SetHeadlineCleaner 注入头条清理钩子。
func (s *Service) SetHeadlineCleaner(h HeadlineCleaner) { s.headlineCleaner = h }

// HeadlineBriefFunc 后台模板列表行的头条摘要（一页一次；返回的 map 缺某个 id = 该行 headlineBrief 为 null）。
type HeadlineBriefFunc func(ctx context.Context, ids []int64) (map[int64]any, error)

// HeadlineStatusFunc 后台模板详情的头条联动（各行业榜名次 / 来源 / 抑制）；返回 nil = null。
type HeadlineStatusFunc func(ctx context.Context, id int64) (any, error)

// SetHeadlineBrief / SetHeadlineStatus 注入头条域的读取（nil = 字段恒为 null）。头条读取失败只打日志，不影响列表 / 详情。
func (s *Service) SetHeadlineBrief(f HeadlineBriefFunc)   { s.headlineBrief = f }
func (s *Service) SetHeadlineStatus(f HeadlineStatusFunc) { s.headlineStatus = f }

// AdminPurgeTemplate 彻底删除：只能删回收站里的（需 plaza:delete，路由层判权）；物理删除模板行、关系行、模板级示例素材与其头条规则。
func (s *Service) AdminPurgeTemplate(ctx context.Context, op Operator, id int64) error {
	if err := s.requireWrite(); err != nil {
		return err
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		return s.purgeIn(ctx, tx, op, id, "")
	})
}

func (s *Service) purgeIn(ctx context.Context, tx bun.IDB, op Operator, id int64, batchID string) error {
	cur, err := lockTemplate(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if cur.Deleted == 0 {
		return errParam("只能彻底删除回收站里的模板")
	}
	inds, _ := industryIDsOf(ctx, tx, id)
	before := templateSnapshot(*cur, inds)
	if _, err := tx.NewDelete().Model((*model.TemplateIndustry)(nil)).Where("template_id = ?", id).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.NewDelete().Model((*model.SampleAsset)(nil)).Where("scope_type = ?", model.SampleScopeTemplate).Where("template_id = ?", id).Exec(ctx); err != nil {
		return err
	}
	if s.headlineCleaner != nil {
		if err := s.headlineCleaner(ctx, tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.NewDelete().Model((*model.Template)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return err
	}
	return s.audit(ctx, tx, op, "TEMPLATE_PURGE", ResTemplate, fmt.Sprint(id), batchID, before, nil)
}

// ---------------------------------------------------------------- 批量

// BatchInput 批量操作：ids ≤200；assign / unassign / replaceIndustries 要 industryIds；publish 可带 onlineAt / offlineAt；
// setOrder = 在 industryIds[0] 这个行业里置顶（该行业最小值 − 10）。
type BatchInput struct {
	Action      string  `json:"action"`
	IDs         []int64 `json:"ids"`
	IndustryIDs []int64 `json:"industryIds"`
	OnlineAt    string  `json:"onlineAt"`
	OfflineAt   string  `json:"offlineAt"`
	Force       bool    `json:"force"`
}

// BatchFailure 批量里失败的一条。
type BatchFailure struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// BatchResult 逐条处理的汇总。
type BatchResult struct {
	BatchID string         `json:"batchId"`
	OK      []int64        `json:"ok"`
	Failed  []BatchFailure `json:"failed"`
}

// BatchActions 允许的批量动作（路由层按动作拆路由判权：publish/offline → plaza:publish；delete/restore → plaza:delete）。
var BatchActions = map[string]bool{"publish": true, "offline": true, "delete": true, "restore": true, "assign": true, "unassign": true, "replaceIndustries": true, "setOrder": true}

// AdminBatch 逐条处理、部分失败返回原因；同一次提交共用 batch_id，每条各写一条审计。
// 行业类动作（assign / unassign / replaceIndustries / setOrder）只写行业关系表，属 A 类写，交接期也放行；
// publish / offline / delete / restore 改 enabled / state / deleted（Java 也写），交接期仍拒。
func (s *Service) AdminBatch(ctx context.Context, op Operator, in BatchInput) (*BatchResult, error) {
	gate := s.requireWrite
	if isIndustryBatchAction(in.Action) {
		gate = s.requireDB
	}
	if err := gate(); err != nil {
		return nil, err
	}
	if !BatchActions[in.Action] {
		return nil, errParam("不支持的批量动作：" + in.Action)
	}
	ids := dedupeIDs(in.IDs)
	if len(ids) == 0 || len(ids) > maxBatchIDs {
		return nil, errParam(fmt.Sprintf("ids 需要 1~%d 个", maxBatchIDs))
	}
	needInd := in.Action == "assign" || in.Action == "unassign" || in.Action == "replaceIndustries" || in.Action == "setOrder"
	if needInd && len(dedupeIDs(in.IndustryIDs)) == 0 && in.Action != "replaceIndustries" {
		return nil, errParam("需要 industryIds")
	}
	if in.Action == "setOrder" && len(dedupeIDs(in.IndustryIDs)) != 1 {
		return nil, errParam("置顶需要指定且仅指定一个行业")
	}
	res := &BatchResult{BatchID: newBatchID(), OK: []int64{}, Failed: []BatchFailure{}}
	for _, id := range ids {
		var err error
		switch in.Action {
		case "publish":
			_, err = s.publish(ctx, op, id, PublishInput{OnlineAt: in.OnlineAt, OfflineAt: in.OfflineAt, Force: in.Force}, res.BatchID)
		case "offline":
			err = s.offline(ctx, op, id, res.BatchID)
		case "delete":
			err = s.softDelete(ctx, op, id, res.BatchID)
		case "restore":
			err = s.restore(ctx, op, id, res.BatchID)
		default:
			err = s.batchIndustries(ctx, op, id, in, res.BatchID)
		}
		if err != nil {
			res.Failed = append(res.Failed, BatchFailure{ID: id, Reason: failReason(err)})
			continue
		}
		res.OK = append(res.OK, id)
	}
	return res, nil
}

func isIndustryBatchAction(a string) bool {
	return a == "assign" || a == "unassign" || a == "replaceIndustries" || a == "setOrder"
}

func failReason(err error) string {
	if _, msg, ok := xerr.Parse(err); ok {
		return msg
	}
	return "处理失败"
}

func (s *Service) batchIndustries(ctx context.Context, op Operator, id int64, in BatchInput, batchID string) error {
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		beforeInds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		inds, err := checkIndustryIDs(ctx, tx, in.IndustryIDs)
		if err != nil {
			return err
		}
		switch in.Action {
		case "assign":
			err = addIndustries(ctx, tx, id, inds)
		case "unassign":
			if len(inds) > 0 {
				_, err = tx.NewDelete().Model((*model.TemplateIndustry)(nil)).Where("template_id = ?", id).Where("industry_id IN (?)", bun.In(inds)).Exec(ctx)
			}
		case "replaceIndustries":
			err = setIndustriesAll(ctx, tx, id, inds)
		case "setOrder":
			indID := inds[0]
			var minS int
			if minS, err = minIndustrySort(ctx, tx, indID); err != nil {
				return err
			}
			if err = addIndustries(ctx, tx, id, []int64{indID}); err != nil { // 没挂过就先挂上
				return err
			}
			_, err = tx.NewUpdate().Model((*model.TemplateIndustry)(nil)).Set("sort_order = ?", minS-sortStep).
				Where("template_id = ?", id).Where("industry_id = ?", indID).Exec(ctx)
		}
		if err != nil {
			return err
		}
		afterInds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := requireIndustryRelationPerm(ctx, op, cur, beforeInds, afterInds); err != nil {
			return err // 回滚本条的关系变更
		}
		return s.audit(ctx, tx, op, "BATCH_"+strings.ToUpper(in.Action), ResTemplate, fmt.Sprint(id), batchID,
			templateSnapshot(*cur, beforeInds), templateSnapshot(*cur, afterInds))
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}
