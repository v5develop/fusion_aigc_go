package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// TemplateFilter 后台模板列表的筛选 / 排序 / 分页。
type TemplateFilter struct {
	Keyword         string
	IndustryID      int64
	IncludeChildren bool
	MediaType       string
	State           string // DRAFT | PUBLISHED | OFFLINE | SCHEDULED | EXPIRED | DELETED
	CreatedBy       int64
	OnlineFrom      string
	OnlineTo        string
	OfflineFrom     string
	OfflineTo       string
	Unassigned      bool
	Sort            string // sortOrder | updatedAt | createdAt | uses7d | industryOrder
	Order           string // asc | desc
	Page, Size      int
}

// AdminTemplateRow 后台列表行 / 回收站行。
type AdminTemplateRow struct {
	ID                int64         `json:"id"`
	Title             string        `json:"title"`
	MediaType         string        `json:"mediaType"`
	CoverURL          string        `json:"coverUrl"`
	State             string        `json:"state"`
	DisplayState      string        `json:"displayState"`
	OnlineAt          *string       `json:"onlineAt"`
	OfflineAt         *string       `json:"offlineAt"`
	Industries        []IndustryRef `json:"industries"`
	Uses7d            int64         `json:"uses7d"`
	CreatedBy         *int64        `json:"createdBy"`
	CreateTime        string        `json:"createTime"`
	UpdateTime        string        `json:"updateTime"`
	Revision          int           `json:"revision"` // 乐观并发版本号：编辑时与 updateTime 一起带回（revision 优先）
	SortOrder         int           `json:"sortOrder"`
	IndustrySortOrder *int          `json:"industrySortOrder,omitempty"`
	Enabled           bool          `json:"enabled"`
	Deleted           bool          `json:"deleted"`
	DeletedAt         *string       `json:"deletedAt,omitempty"`
	PurgeAt           *string       `json:"purgeAt,omitempty"`
	NeedsProductImage bool          `json:"needsProductImage"`
	// MediaStatus 视频处理状态 PROCESSING | READY | FAILED（图片模板恒 READY）；FAILED 时 MediaError 是给运营看的原因，
	// MediaNote 是处理结果备注（如「未转码」）。MediaAttempts 为已尝试次数（满 3 次转 FAILED）。可经 POST /templates/:id/reprocess-media 重新处理。
	MediaStatus   string  `json:"mediaStatus"`
	MediaError    *string `json:"mediaError"`
	MediaNote     *string `json:"mediaNote"`
	MediaAttempts int     `json:"mediaAttempts"`
	// HeadlineBrief 头条摘要（是否在榜、各范围名次、生效规则数、是否被抑制；头条域经 SetHeadlineBrief 提供，未装配为 null）。
	HeadlineBrief any `json:"headlineBrief"`
}

// AdminTemplatePage 后台列表分页。
type AdminTemplatePage struct {
	Total           int                `json:"total"`
	Page            int                `json:"page"`
	Size            int                `json:"size"`
	UnassignedCount int                `json:"unassignedCount"`
	Items           []AdminTemplateRow `json:"items"`
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// applyStateFilter 状态筛选（回收站与状态正交；默认列表不含回收站）。
func applyStateFilter(q *bun.SelectQuery, state string) (*bun.SelectQuery, error) {
	switch strings.ToUpper(state) {
	case "":
		return q.Where("t.deleted = 0"), nil
	case DisplayDeleted:
		return q.Where("t.deleted = 1"), nil
	case DisplayDraft:
		return q.Where("t.deleted = 0").Where("t.state = ?", model.StateDraft), nil
	case DisplayOffline:
		return q.Where("t.deleted = 0").Where("t.state = ?", model.StateOffline), nil
	case DisplayScheduled:
		return q.Where("t.deleted = 0").Where("t.state = ?", model.StatePublished).Where("t.online_at > NOW()"), nil
	case DisplayExpired:
		return q.Where("t.deleted = 0").Where("t.state = ?", model.StatePublished).Where("t.offline_at <= NOW()"), nil
	case DisplayPublished:
		return q.Where("t.deleted = 0").Where("t.state = ?", model.StatePublished).
			Where("(t.online_at IS NULL OR t.online_at <= NOW())").Where("(t.offline_at IS NULL OR t.offline_at > NOW())"), nil
	}
	return nil, errParam("state 不合法：" + state)
}

func (s *Service) industryIDsWithChildren(ctx context.Context, id int64, include bool) ([]int64, error) {
	ids := []int64{id}
	if include {
		var kids []int64
		if err := s.db.NewSelect().Model((*model.Industry)(nil)).Column("id").Where("parent_id = ?", id).Where("deleted = 0").Scan(ctx, &kids); err != nil {
			return nil, err
		}
		ids = append(ids, kids...)
	}
	return ids, nil
}

// AdminListTemplates 后台全量模板列表（所有行业 / 状态 / 媒体类型，默认不含回收站）。
func (s *Service) AdminListTemplates(ctx context.Context, f TemplateFilter) (*AdminTemplatePage, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	page, size := normPage(f.Page, f.Size, 20)
	if f.MediaType != "" && f.MediaType != model.MediaImage && f.MediaType != model.MediaVideo {
		return nil, errParam("mediaType 只能是 image 或 video")
	}
	sortKey := f.Sort
	if sortKey == "" {
		sortKey = "sortOrder"
	}
	desc := !strings.EqualFold(f.Order, "asc")
	if sortKey == "industryOrder" && f.IndustryID == 0 {
		return nil, errParam("行业内顺序需要先选择行业")
	}
	build := func() (*bun.SelectQuery, error) {
		var rows []model.Template
		q := s.db.NewSelect().Model(&rows)
		q, err := applyStateFilter(q, f.State)
		if err != nil {
			return nil, err
		}
		if kw := strings.TrimSpace(f.Keyword); kw != "" {
			like := "%" + likeEscape(kw) + "%"
			q = q.WhereGroup(" AND ", func(g *bun.SelectQuery) *bun.SelectQuery {
				return g.Where("t.title LIKE ?", like).WhereOr("t.description LIKE ?", like).WhereOr("CAST(t.tags_json AS CHAR) LIKE ?", like)
			})
		}
		if f.MediaType != "" {
			q = q.Where("t.media_type = ?", f.MediaType)
		}
		if f.CreatedBy > 0 {
			q = q.Where("t.created_by = ?", f.CreatedBy)
		}
		if f.IndustryID > 0 {
			ids, err := s.industryIDsWithChildren(ctx, f.IndustryID, f.IncludeChildren)
			if err != nil {
				return nil, err
			}
			q = q.Where("t.id IN (?)", s.db.NewSelect().Model((*model.TemplateIndustry)(nil)).Column("template_id").Where("industry_id IN (?)", bun.In(ids)))
		}
		if f.Unassigned {
			q = q.Where("NOT EXISTS (SELECT 1 FROM afv_plaza_template_industry r WHERE r.template_id = t.id)")
		}
		for _, tf := range []struct{ col, op, v string }{
			{"t.online_at", ">=", f.OnlineFrom}, {"t.online_at", "<", f.OnlineTo},
			{"t.offline_at", ">=", f.OfflineFrom}, {"t.offline_at", "<", f.OfflineTo},
		} {
			if tf.v == "" {
				continue
			}
			tm, err := parseAdminTime(tf.v)
			if err != nil {
				return nil, err
			}
			q = q.Where(tf.col+" "+tf.op+" FROM_UNIXTIME(? / 1000)", tm.Time.UnixMilli())
		}
		return q, nil
	}
	var rows []model.Template
	q, err := build()
	if err != nil {
		return nil, err
	}
	q = q.Model(&rows)
	inMemory := sortKey == "uses7d" || sortKey == "industryOrder"
	var total int
	if inMemory {
		if err := q.Scan(ctx); err != nil {
			return nil, err
		}
		total = len(rows)
	} else {
		col := map[string]string{"sortOrder": "t.sort_order", "updatedAt": "t.update_time", "createdAt": "t.create_time"}[sortKey]
		if col == "" {
			return nil, errParam("sort 不合法：" + sortKey)
		}
		dir := "ASC"
		if desc {
			dir = "DESC"
		}
		n, err := q.OrderExpr(col + " " + dir).OrderExpr("t.id DESC").Limit(int64(size)).Offset(int64((page - 1) * size)).ScanAndCount(ctx)
		if err != nil {
			return nil, err
		}
		total = int(n)
	}
	items, err := s.adminRows(ctx, rows, f.IndustryID)
	if err != nil {
		return nil, err
	}
	if inMemory {
		sortAdminRows(items, sortKey, desc)
		from, to := (page-1)*size, page*size
		if from > len(items) {
			from = len(items)
		}
		if to > len(items) {
			to = len(items)
		}
		items = items[from:to]
	}
	unassigned, err := s.db.NewSelect().Model((*model.Template)(nil)).Where("deleted = 0").
		Where("NOT EXISTS (SELECT 1 FROM afv_plaza_template_industry r WHERE r.template_id = t.id)").Count(ctx)
	if err != nil {
		return nil, err
	}
	return &AdminTemplatePage{Total: total, Page: page, Size: size, UnassignedCount: int(unassigned), Items: items}, nil
}

func sortAdminRows(items []AdminTemplateRow, key string, desc bool) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		var less, eq bool
		switch key {
		case "uses7d":
			less, eq = a.Uses7d < b.Uses7d, a.Uses7d == b.Uses7d
		default: // industryOrder：行业内顺序（与用户端同序），降序 = 倒过来
			ai, bi := intOr(a.IndustrySortOrder), intOr(b.IndustrySortOrder)
			less, eq = ai < bi, ai == bi
		}
		if eq {
			return a.ID > b.ID
		}
		if desc && key != "industryOrder" {
			return !less
		}
		if desc && key == "industryOrder" {
			return !less
		}
		return less
	})
}

func intOr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// adminRows 把模板行装配成列表行：批量取行业关系与近 7 天做同款次数。
func (s *Service) adminRows(ctx context.Context, rows []model.Template, industryID int64) ([]AdminTemplateRow, error) {
	out := make([]AdminTemplateRow, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	var rels []model.TemplateIndustry
	if err := s.db.NewSelect().Model(&rels).Where("template_id IN (?)", bun.In(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	var inds []model.Industry
	if err := s.db.NewSelect().Model(&inds).Where("ti.deleted = 0").Scan(ctx); err != nil {
		return nil, err
	}
	byID := map[int64]model.Industry{}
	for _, in := range inds {
		byID[in.ID] = in
	}
	byTpl := map[int64][]model.TemplateIndustry{}
	for _, r := range rels {
		byTpl[r.TemplateID] = append(byTpl[r.TemplateID], r)
	}
	var usage map[int64]int64
	if s.rep != nil {
		usage, _ = s.rep.Uses7d(ctx)
	}
	now := s.now()
	for _, t := range rows {
		t := t
		row := AdminTemplateRow{
			ID: t.ID, Title: t.Title, MediaType: t.MediaType, State: t.State, DisplayState: displayState(&t, now),
			OnlineAt: timeStr(t.OnlineAt), OfflineAt: timeStr(t.OfflineAt), Industries: []IndustryRef{},
			Uses7d: usage[t.ID], CreateTime: t.CreateTime.Format(timeLayout), UpdateTime: t.UpdateTime.Format(timeLayout),
			Revision: t.Revision, SortOrder: t.SortOrder, Enabled: t.Enabled == 1, Deleted: t.Deleted == 1, NeedsProductImage: t.NeedsProductImage == 1,
			CoverURL:    adminCover(&t),
			MediaStatus: t.MediaStatus, MediaError: strPtr(t.MediaError), MediaNote: strPtr(t.MediaNote), MediaAttempts: t.MediaAttempts,
		}
		if t.CreatedBy.Valid {
			v := t.CreatedBy.Int64
			row.CreatedBy = &v
		}
		if t.Deleted == 1 {
			row.DeletedAt = timeStr(t.DeletedAt)
			if t.DeletedAt.Valid {
				p := t.DeletedAt.Time.Add(trashDays * 24 * time.Hour).In(cst).Format(timeLayout)
				row.PurgeAt = &p
			}
		}
		for _, r := range byTpl[t.ID] {
			in, ok := byID[r.IndustryID]
			if !ok {
				continue
			}
			ref := IndustryRef{ID: in.ID, Code: in.Code, Name: in.Name}
			if in.ParentID.Valid {
				ref.ParentCode = byID[in.ParentID.Int64].Code
			}
			row.Industries = append(row.Industries, ref)
			if industryID > 0 && in.ID == industryID {
				so := r.SortOrder
				row.IndustrySortOrder = &so
			}
		}
		out = append(out, row)
	}
	if s.headlineBrief != nil {
		briefs, err := s.headlineBrief(ctx, ids)
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza: 读头条摘要失败，本页 headlineBrief 置 null: %v", err)
		}
		for i := range out {
			if b, ok := briefs[out[i].ID]; ok && b != nil {
				out[i].HeadlineBrief = b
			}
		}
	}
	return out, nil
}

func adminCover(t *model.Template) string {
	if t.MediaType == model.MediaVideo && t.PosterURL.Valid && t.PosterURL.String != "" {
		return t.PosterURL.String
	}
	return firstResult(t.DataJSON)
}

// AdminTrash 回收站列表（默认保留 30 天，到期物理删除）。
func (s *Service) AdminTrash(ctx context.Context, page, size int) (*AdminTemplatePage, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	page, size = normPage(page, size, 20)
	var rows []model.Template
	n, err := s.db.NewSelect().Model(&rows).Where("t.deleted = 1").OrderExpr("t.deleted_at DESC").OrderExpr("t.id DESC").
		Limit(int64(size)).Offset(int64((page - 1) * size)).ScanAndCount(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.adminRows(ctx, rows, 0)
	if err != nil {
		return nil, err
	}
	return &AdminTemplatePage{Total: int(n), Page: page, Size: size, Items: items}, nil
}

// IndustrySlot 模板在某行业里的关系行（含行业内排序位）。
type IndustrySlot struct {
	IndustryRef
	SortOrder int `json:"sortOrder"`
}

// AdminTemplateDetail 后台详情：全部字段 + 编辑用的载荷拆解 + 行业关系。
type AdminTemplateDetail struct {
	AdminTemplateRow
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Tags        []string        `json:"tags"`
	Payload     json.RawMessage `json:"payload"`
	// PayloadKnown 已知键（表单字段）；PayloadExtra 其余键的原 JSON（高级模式，保存时原样保留）。
	PayloadKnown   json.RawMessage `json:"payloadKnown"`
	PayloadExtra   json.RawMessage `json:"payloadExtra"`
	AgentSkillID   *string         `json:"agentSkillId"`
	VideoURL       *string         `json:"videoUrl"`
	PosterURL      *string         `json:"posterUrl"`
	Channels       json.RawMessage `json:"channels"`
	Slots          json.RawMessage `json:"slots"`
	TextOverlay    bool            `json:"textOverlay"`
	IndustryIDs    []int64         `json:"industryIds"`
	IndustrySlots  []IndustrySlot  `json:"industrySlots"`
	TargetChannels []ChannelSpec   `json:"targetChannels"`
	// HeadlineStatus 头条联动（各行业榜上的名次 / 来源 / 抑制状态、适用规则）：头条域经 SetHeadlineStatus 提供；
	// 未装配 / 模板不在目录里（草稿、回收站）/ 读取失败为 null。
	HeadlineStatus any `json:"headlineStatus"`
}

var knownPayloadKeys = map[string]bool{"result": true, "image_urls": true, "description": true, "guidance": true, "prompt": true}

// AdminGetTemplate 后台详情（含回收站里的）。
func (s *Service) AdminGetTemplate(ctx context.Context, id int64) (*AdminTemplateDetail, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	var t model.Template
	if err := s.db.NewSelect().Model(&t).Where("t.id = ?", id).Scan(ctx); err != nil {
		if noRows(err) {
			return nil, xerr.NewReason(xerr.NotFound, ReasonTemplateNotFound, "模板不存在")
		}
		return nil, err
	}
	rows, err := s.adminRows(ctx, []model.Template{t}, 0)
	if err != nil {
		return nil, err
	}
	d := &AdminTemplateDetail{AdminTemplateRow: rows[0], Description: t.Description.String, Category: t.Category.String,
		Tags: readTags(t.TagsJSON), VideoURL: strPtr(t.VideoURL), PosterURL: strPtr(t.PosterURL),
		Channels: rawOrNilJSON(t.ChannelsJSON), Slots: adminSlots(t.ID, t.SlotsJSON), TextOverlay: t.TextOverlay == 1,
		IndustryIDs: []int64{}, IndustrySlots: []IndustrySlot{}, TargetChannels: []ChannelSpec{}}
	d.Payload = payloadForDisplay(t.DataJSON)
	known, extra := splitPayload(d.Payload)
	d.PayloadKnown, d.PayloadExtra = known, extra
	if id := skillID(t.DataJSON); id != "" {
		d.AgentSkillID = &id
	}
	var rels []model.TemplateIndustry
	if err := s.db.NewSelect().Model(&rels).Where("template_id = ?", id).Order("industry_id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	for _, r := range rels {
		d.IndustryIDs = append(d.IndustryIDs, r.IndustryID)
		for _, ref := range d.Industries {
			if ref.ID == r.IndustryID {
				d.IndustrySlots = append(d.IndustrySlots, IndustrySlot{IndustryRef: ref, SortOrder: r.SortOrder})
			}
		}
	}
	if dict, err := loadChannelDict(ctx, s.db); err == nil {
		cat := &Catalog{channels: dict}
		d.TargetChannels = cat.resolveChannels(t.ChannelsJSON)
	}
	if s.headlineStatus != nil && t.Deleted == 0 {
		if st, err := s.headlineStatus(ctx, id); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 读模板 %d 的头条联动失败，headlineStatus 置 null: %v", id, err)
		} else if st != nil {
			d.HeadlineStatus = st
		}
	}
	return d, nil
}

// adminSlots 后台回显的入参 schema：规整成 {schemaVersion, items}（旧键 slots 一并转过来）；没有 → null；
// 库里的脏数据原样回显（让运营看到并修正），同时打日志。
func adminSlots(id int64, raw json.RawMessage) json.RawMessage {
	schema, err := ParseSlotSchema(raw)
	if err != nil {
		logx.Errorf("plaza: 模板 %d 的 slots_json 不合法，后台原样回显: %v", id, err)
		return rawOrNilJSON(raw)
	}
	if schema == nil {
		return json.RawMessage("null")
	}
	return schema.Marshal()
}

func rawOrNilJSON(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("null")
	}
	return r
}

// splitPayload 已知键 / 其余键（保序）。
func splitPayload(payload []byte) (known, extra json.RawMessage) {
	items, ok := parseObject(payload)
	if !ok {
		return json.RawMessage("{}"), json.RawMessage("{}")
	}
	var k, e []kv
	for _, it := range items {
		if knownPayloadKeys[it.key] {
			k = append(k, it)
		} else {
			e = append(e, it)
		}
	}
	return writeObject(k), writeObject(e)
}

// ---------------------------------------------------------------- 行业内排序

// AdminIndustryTemplates 该行业内的模板按行业内顺序列出（排序视图用）。一级行业含二级模板的合并视图：
// 顺序 = 每个模板在「本级及其二级的关系行」里的最小 sort_order；含「仅挂在二级的模板」时只读。
type AdminIndustryTemplates struct {
	IndustryID int64              `json:"industryId"`
	ReadOnly   bool               `json:"readOnly"`
	Hint       string             `json:"hint,omitempty"`
	Items      []AdminTemplateRow `json:"items"`
}

func (s *Service) AdminIndustryTemplates(ctx context.Context, industryID int64) (*AdminIndustryTemplates, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	var in model.Industry
	if err := s.db.NewSelect().Model(&in).Where("ti.id = ?", industryID).Where("ti.deleted = 0").Scan(ctx); err != nil {
		if noRows(err) {
			return nil, xerr.NotFoundf("行业")
		}
		return nil, err
	}
	ids, err := s.industryIDsWithChildren(ctx, industryID, !in.ParentID.Valid)
	if err != nil {
		return nil, err
	}
	var rels []model.TemplateIndustry
	if err := s.db.NewSelect().Model(&rels).Where("industry_id IN (?)", bun.In(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	best := map[int64]int{}
	direct := map[int64]bool{}
	for _, r := range rels {
		if cur, ok := best[r.TemplateID]; !ok || r.SortOrder < cur {
			best[r.TemplateID] = r.SortOrder
		}
		if r.IndustryID == industryID {
			direct[r.TemplateID] = true
		}
	}
	out := &AdminIndustryTemplates{IndustryID: industryID, Items: []AdminTemplateRow{}}
	if len(best) == 0 {
		return out, nil
	}
	tids := make([]int64, 0, len(best))
	for id := range best {
		tids = append(tids, id)
		if !direct[id] {
			out.ReadOnly = true
		}
	}
	if out.ReadOnly {
		out.Hint = "含仅挂在二级的模板，请进入具体二级行业排序，或筛选「直挂本级」后排序"
	}
	var rows []model.Template
	if err := s.db.NewSelect().Model(&rows).Where("t.id IN (?)", bun.In(tids)).Where("t.deleted = 0").Scan(ctx); err != nil {
		return nil, err
	}
	items, err := s.adminRows(ctx, rows, industryID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		so := best[items[i].ID]
		items[i].IndustrySortOrder = &so
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if *a.IndustrySortOrder != *b.IndustrySortOrder {
			return *a.IndustrySortOrder < *b.IndustrySortOrder
		}
		if a.UpdateTime != b.UpdateTime {
			return a.UpdateTime > b.UpdateTime
		}
		return a.ID > b.ID
	})
	out.Items = items
	return out, nil
}

// planOrder 行业内拖拽排序：被提交的模板按提交顺序从「它们现有 sort_order 的最小值」起、步长 10 重新编号。
// 返回 template_id → 新 sort_order。
func planOrder(existing map[int64]int, ordered []int64) (map[int64]int, error) {
	if len(ordered) == 0 {
		return nil, errParam("orderedTemplateIds 不能为空")
	}
	seen := map[int64]bool{}
	base := 0
	for i, id := range ordered {
		cur, ok := existing[id]
		if !ok {
			return nil, errParam(fmt.Sprintf("模板 %d 没有直挂在该行业里，请进入具体二级行业排序", id))
		}
		if seen[id] {
			return nil, errParam("orderedTemplateIds 里有重复的模板")
		}
		seen[id] = true
		if i == 0 || cur < base {
			base = cur
		}
	}
	out := make(map[int64]int, len(ordered))
	for i, id := range ordered {
		out[id] = base + i*sortStep
	}
	return out, nil
}

// AdminSetIndustryOrder 行业内拖拽排序：只改这些模板在该行业关系行上的 sort_order（一次事务）；其余模板不动，
// 同一模板在不同行业里可有不同位置。
// A 类写（只动 afv_plaza_template_industry.sort_order，不碰模板行）：交接期也放行。
func (s *Service) AdminSetIndustryOrder(ctx context.Context, op Operator, industryID int64, ordered []int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var rels []model.TemplateIndustry
		if err := tx.NewSelect().Model(&rels).Where("industry_id = ?", industryID).For("UPDATE").Scan(ctx); err != nil {
			return err
		}
		existing := map[int64]int{}
		for _, r := range rels {
			existing[r.TemplateID] = r.SortOrder
		}
		plan, err := planOrder(existing, ordered)
		if err != nil {
			return err
		}
		before, after := map[string]any{}, map[string]any{}
		for _, id := range ordered {
			before[fmt.Sprint(id)] = existing[id]
			after[fmt.Sprint(id)] = plan[id]
			if _, err := tx.NewUpdate().Model((*model.TemplateIndustry)(nil)).Set("sort_order = ?", plan[id]).
				Where("industry_id = ?", industryID).Where("template_id = ?", id).Exec(ctx); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, op, "INDUSTRY_TEMPLATE_ORDER", ResIndustry, fmt.Sprint(industryID), "", before, after)
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}

// ---------------------------------------------------------------- 以用户视角预览

// PreviewItem 预览项：用户端同形字段 + 状态与「用户当前是否可见」。
type PreviewItem struct {
	TemplateItem
	State       string `json:"state"`
	UserVisible bool   `json:"userVisible"`
}

// PreviewPage 预览分页。
type PreviewPage struct {
	Current int           `json:"current"`
	Pages   int           `json:"pages"`
	Total   int           `json:"total"`
	AsOf    string        `json:"asOf"`
	Data    []PreviewItem `json:"data"`
}

// AdminPreview 以用户视角预览（industry 必填，同用户端口径）：默认只含此刻用户真能看到的（已发布且在窗口内）；
// asOf 预览某个时刻；includeDraft 同时含草稿 / 已下线并标出「用户当前不可见」。
func (s *Service) AdminPreview(ctx context.Context, p ListParams, asOf string, includeDraft bool) (*PreviewPage, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Industry) == "" {
		return nil, ErrIndustryRequired()
	}
	if p.MediaType != "" && p.MediaType != model.MediaImage && p.MediaType != model.MediaVideo {
		return nil, errParam("mediaType 只能是 image 或 video")
	}
	if p.Page < 1 {
		p.Page = 1
	}
	if p.Size < 1 {
		p.Size = defaultPageSize
	}
	if p.Size > maxPageSize {
		p.Size = maxPageSize
	}
	at := s.now()
	if t, err := parseAdminTime(asOf); err != nil {
		return nil, err
	} else if t.Valid {
		at = t.Time
	}
	kw, err := validKeyword(p.Keyword)
	if err != nil {
		return nil, err
	}
	cat, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	real := cat
	if includeDraft {
		all, err := s.rep.NonDeletedTemplates(ctx)
		if err != nil {
			return nil, err
		}
		inds, err := s.src.Industries(ctx)
		if err != nil {
			return nil, err
		}
		rels, err := s.src.Relations(ctx)
		if err != nil {
			return nil, err
		}
		chs, err := s.src.Channels(ctx)
		if err != nil {
			return nil, err
		}
		fake := make([]model.Template, len(all))
		for i, t := range all {
			t.State, t.OnlineAt, t.OfflineAt = model.StatePublished, sqlNullTime(), sqlNullTime()
			fake[i] = t
		}
		cat = newCatalog(inds, rels, fake, chs, at, false) // 含草稿的预览：刻意不看 enabled
		_ = real
	}
	sc, err := s.resolve(cat, p.Industry, p.Sub)
	if err != nil {
		return nil, err
	}
	res := cat.List(sc, ListQuery{MediaType: p.MediaType, Keyword: kw, Page: p.Page, Size: p.Size}, at)
	usage := s.usageMap(ctx)
	out := &PreviewPage{Current: p.Page, Total: res.Total, Pages: (res.Total + p.Size - 1) / p.Size, AsOf: at.In(cst).Format(timeLayout), Data: []PreviewItem{}}
	for _, rec := range res.Recs {
		pi := PreviewItem{TemplateItem: s.item(cat, rec, usage, at)}
		if includeDraft {
			if orig, err := s.adminTemplateRow(ctx, rec.t.ID); err == nil && orig != nil {
				pi.State = orig.State
				pi.UserVisible = recVisible(orig, at, !s.cfg.AdminWriteEnabled)
			}
		} else {
			pi.State, pi.UserVisible = model.StatePublished, true
		}
		out.Data = append(out.Data, pi)
	}
	return out, nil
}

func (s *Service) adminTemplateRow(ctx context.Context, id int64) (*model.Template, error) {
	var t model.Template
	if err := s.db.NewSelect().Model(&t).Where("t.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return &t, nil
}

func recVisible(t *model.Template, now time.Time, requireEnabled bool) bool {
	return (&tplRec{t: *t, requireEnabled: requireEnabled}).visibleAt(now)
}
