package core

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
)

// ---------------------------------------------------------------- 渠道字典（16.2）

// ChannelInput 渠道新建 / 编辑；code 创建后不可改（模板的 channels_json 按它引用）。
type ChannelInput struct {
	Code      string  `json:"code"`
	Name      *string `json:"name"`
	MediaType *string `json:"mediaType"`
	Width     *int    `json:"width"`
	Height    *int    `json:"height"`
	Ratio     *string `json:"ratio"`
	Note      *string `json:"note"`
	SortOrder *int    `json:"sortOrder"`
	Enabled   *bool   `json:"enabled"`
}

// AdminChannel 后台渠道行。
type AdminChannel struct {
	ID        int64   `json:"id"`
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	MediaType string  `json:"mediaType"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Ratio     string  `json:"ratio"`
	Note      *string `json:"note"`
	SortOrder int     `json:"sortOrder"`
	Enabled   bool    `json:"enabled"`
	// TemplateCount 引用该渠道的模板数（含回收站里的）；TrashCount 是其中在回收站的个数。大于 0 时不能删除。
	TemplateCount int `json:"templateCount"`
	TrashCount    int `json:"trashCount"`
}

func channelView(c model.Channel) AdminChannel {
	return AdminChannel{ID: c.ID, Code: c.Code, Name: c.Name, MediaType: c.MediaType, Width: c.Width, Height: c.Height,
		Ratio: c.Ratio, Note: strPtr(c.Note), SortOrder: c.SortOrder, Enabled: c.Enabled == 1}
}

func channelSnapshot(c model.Channel) map[string]any {
	b, _ := jsonMarshal2(channelView(c))
	return b
}

func (s *Service) AdminListChannels(ctx context.Context) ([]AdminChannel, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	rows, err := s.rep.Channels(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := channelRefCounts(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]AdminChannel, 0, len(rows))
	for _, r := range rows {
		v := channelView(r)
		v.TemplateCount, v.TrashCount = refs[r.Code].total, refs[r.Code].trash
		out = append(out, v)
	}
	return out, nil
}

var channelCodeRe = industryCodeRe

func applyChannel(row *model.Channel, in ChannelInput) error {
	if in.Name != nil {
		row.Name = strings.TrimSpace(*in.Name)
	}
	if row.Name == "" || runeLen(row.Name) > 32 {
		return errParam("渠道名称必填，最多 32 个字")
	}
	if in.MediaType != nil {
		row.MediaType = strings.TrimSpace(*in.MediaType)
	}
	if row.MediaType == "" {
		row.MediaType = model.MediaImage
	}
	if row.MediaType != model.MediaImage && row.MediaType != model.MediaVideo {
		return errParam("渠道 mediaType 只能是 image 或 video")
	}
	if in.Width != nil {
		row.Width = *in.Width
	}
	if in.Height != nil {
		row.Height = *in.Height
	}
	if row.Width <= 0 || row.Height <= 0 || row.Width > 20000 || row.Height > 20000 {
		return errParam("渠道宽高必须是 1~20000 的整数")
	}
	if in.Ratio != nil {
		row.Ratio = strings.TrimSpace(*in.Ratio)
	}
	if row.Ratio == "" || len(row.Ratio) > 8 {
		return errParam("渠道比例必填（如 9:16），最多 8 个字符")
	}
	if in.Note != nil {
		row.Note = nullStr(in.Note)
	}
	if row.Note.Valid && runeLen(row.Note.String) > 120 {
		return errParam("渠道备注最多 120 个字")
	}
	if in.SortOrder != nil {
		row.SortOrder = *in.SortOrder
	}
	if in.Enabled != nil {
		row.Enabled = b2i(*in.Enabled)
	}
	return nil
}

func (s *Service) AdminCreateChannel(ctx context.Context, op Operator, in ChannelInput) (int64, error) {
	if err := s.requireDB(); err != nil {
		return 0, err
	}
	in.Code = strings.TrimSpace(in.Code)
	if !channelCodeRe.MatchString(in.Code) || len(in.Code) > 48 {
		return 0, errParam("渠道 code 只能是小写字母开头的字母、数字、下划线，长度 2~48")
	}
	row := model.Channel{Code: in.Code, Enabled: 1}
	if err := applyChannel(&row, in); err != nil {
		return 0, err
	}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			if isDuplicate(err) {
				return xerr.New(xerr.Conflict, "渠道 code 已存在（已删除渠道的 code 也不可复用）")
			}
			return err
		}
		return s.audit(ctx, tx, op, "CHANNEL_CREATE", ResChannel, fmt.Sprint(row.ID), "", nil, channelSnapshot(row))
	})
	if err != nil {
		return 0, err
	}
	s.Invalidate(ctx)
	return row.ID, nil
}

func (s *Service) AdminUpdateChannel(ctx context.Context, op Operator, id int64, in ChannelInput) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var row model.Channel
		if err := tx.NewSelect().Model(&row).Where("ch.id = ?", id).Where("ch.deleted = 0").For("UPDATE").Scan(ctx); err != nil {
			if noRows(err) {
				return xerr.NotFoundf("渠道")
			}
			return err
		}
		if c := strings.TrimSpace(in.Code); c != "" && c != row.Code {
			return errParam("渠道 code 创建后不可修改")
		}
		before := channelSnapshot(row)
		if err := applyChannel(&row, in); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*model.Channel)(nil)).Where("id = ?", id).
			Set("name = ?", row.Name).Set("media_type = ?", row.MediaType).Set("width = ?", row.Width).Set("height = ?", row.Height).
			Set("ratio = ?", row.Ratio).Set("note = ?", nullableStr(row.Note)).Set("sort_order = ?", row.SortOrder).Set("enabled = ?", row.Enabled).
			Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "CHANNEL_UPDATE", ResChannel, fmt.Sprint(id), "", before, channelSnapshot(row))
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}

// ---------------------------------------------------------------- 示例素材（17.5）

// SampleInput 示例素材新建 / 编辑。scope 为 INDUSTRY 时 industryId 必填，为 TEMPLATE 时 templateId 必填。
type SampleInput struct {
	ScopeType  string  `json:"scopeType"`
	IndustryID int64   `json:"industryId"`
	TemplateID int64   `json:"templateId"`
	URL        *string `json:"url"`
	ThumbURL   *string `json:"thumbUrl"`
	Title      *string `json:"title"`
	Role       *string `json:"role"` // INPUT | OUTPUT；缺省（新建）= OUTPUT，更新缺省 = 不改
	SortOrder  *int    `json:"sortOrder"`
	Enabled    *bool   `json:"enabled"`
}

// AdminSample 后台示例素材行。
type AdminSample struct {
	ID         int64   `json:"id"`
	ScopeType  string  `json:"scopeType"`
	IndustryID *int64  `json:"industryId"`
	TemplateID *int64  `json:"templateId"`
	Kind       string  `json:"kind"`
	URL        string  `json:"url"`
	ThumbURL   *string `json:"thumbUrl"`
	Title      string  `json:"title"`
	Role       string  `json:"role"`
	SortOrder  int     `json:"sortOrder"`
	Enabled    bool    `json:"enabled"`
}

func sampleView(r model.SampleAsset) AdminSample {
	a := AdminSample{ID: r.ID, ScopeType: r.ScopeType, Kind: r.Kind, URL: r.URL, ThumbURL: strPtr(r.ThumbURL), Title: r.Title, Role: roleOrDefault(r.Role), SortOrder: r.SortOrder, Enabled: r.Enabled == 1}
	if r.IndustryID.Valid {
		v := r.IndustryID.Int64
		a.IndustryID = &v
	}
	if r.TemplateID.Valid {
		v := r.TemplateID.Int64
		a.TemplateID = &v
	}
	return a
}

func sampleSnapshot(r model.SampleAsset) map[string]any {
	b, _ := jsonMarshal2(sampleView(r))
	return b
}

// validHTTPURL 示例素材地址必须是 http(s) 绝对地址（上传走公有桶通路；桶目录校验见 6.2.3，由上传确认接口负责）。
func validHTTPURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != "" && len(u) <= 1024
}

func (s *Service) AdminListSamples(ctx context.Context, scopeType string, industryID, templateID int64) ([]AdminSample, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	var rows []model.SampleAsset
	q := s.db.NewSelect().Model(&rows).Where("sa.deleted = 0").Order("sa.sort_order ASC", "sa.id ASC")
	switch scopeType {
	case model.SampleScopeIndustry:
		q = q.Where("sa.scope_type = ?", scopeType).Where("sa.industry_id = ?", industryID)
	case model.SampleScopeTemplate:
		q = q.Where("sa.scope_type = ?", scopeType).Where("sa.template_id = ?", templateID)
	default:
		return nil, errParam("scopeType 只能是 INDUSTRY 或 TEMPLATE")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]AdminSample, 0, len(rows))
	for _, r := range rows {
		out = append(out, sampleView(r))
	}
	return out, nil
}

func applySample(row *model.SampleAsset, in SampleInput) error {
	if in.URL != nil {
		row.URL = strings.TrimSpace(*in.URL)
	}
	if !validHTTPURL(row.URL) {
		return errParam("示例素材地址必须是 http(s) 地址")
	}
	if in.ThumbURL != nil {
		row.ThumbURL = nullStr(in.ThumbURL)
		if row.ThumbURL.Valid && !validHTTPURL(row.ThumbURL.String) {
			return errParam("缩略图地址必须是 http(s) 地址")
		}
	}
	if in.Title != nil {
		row.Title = strings.TrimSpace(*in.Title)
	}
	if row.Title == "" || runeLen(row.Title) > 40 {
		return errParam("示例标题必填，最多 40 个字")
	}
	if in.Role != nil {
		if *in.Role != model.SampleRoleInput && *in.Role != model.SampleRoleOutput {
			return errParam("role 只能是 INPUT 或 OUTPUT")
		}
		row.Role = *in.Role
	}
	if row.Role == "" {
		row.Role = model.SampleRoleOutput
	}
	if in.SortOrder != nil {
		row.SortOrder = *in.SortOrder
	}
	if in.Enabled != nil {
		row.Enabled = b2i(*in.Enabled)
	}
	return nil
}

// AdminCreateSample 新建示例素材：每个范围（某行业 / 某模板）最多 6 条。
func (s *Service) AdminCreateSample(ctx context.Context, op Operator, in SampleInput) (int64, error) {
	if err := s.requireDB(); err != nil {
		return 0, err
	}
	row := model.SampleAsset{ScopeType: in.ScopeType, Kind: "image", Enabled: 1}
	switch in.ScopeType {
	case model.SampleScopeIndustry:
		if in.IndustryID <= 0 {
			return 0, errParam("行业级示例需要 industryId")
		}
		row.IndustryID = sql.NullInt64{Int64: in.IndustryID, Valid: true}
	case model.SampleScopeTemplate:
		if in.TemplateID <= 0 {
			return 0, errParam("模板级示例需要 templateId")
		}
		row.TemplateID = sql.NullInt64{Int64: in.TemplateID, Valid: true}
	default:
		return 0, errParam("scopeType 只能是 INDUSTRY 或 TEMPLATE")
	}
	if err := applySample(&row, in); err != nil {
		return 0, err
	}
	if op.ID > 0 {
		row.CreatedBy = sql.NullInt64{Int64: op.ID, Valid: true}
	}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if row.ScopeType == model.SampleScopeIndustry {
			if n, err := tx.NewSelect().Model((*model.Industry)(nil)).Where("id = ?", in.IndustryID).Where("deleted = 0").Count(ctx); err != nil {
				return err
			} else if n == 0 {
				return xerr.NewReason(xerr.NotFound, ReasonIndustryNotFound, "行业不存在")
			}
		} else if n, err := tx.NewSelect().Model((*model.Template)(nil)).Where("id = ?", in.TemplateID).Where("deleted = 0").Count(ctx); err != nil {
			return err
		} else if n == 0 {
			return xerr.NewReason(xerr.NotFound, ReasonTemplateNotFound, "模板不存在")
		}
		q := tx.NewSelect().Model((*model.SampleAsset)(nil)).Where("deleted = 0").Where("scope_type = ?", row.ScopeType)
		if row.ScopeType == model.SampleScopeIndustry {
			q = q.Where("industry_id = ?", in.IndustryID)
		} else {
			q = q.Where("template_id = ?", in.TemplateID)
		}
		n, err := q.Count(ctx)
		if err != nil {
			return err
		}
		if n >= MaxSamples {
			return errParam(fmt.Sprintf("每个范围最多 %d 条示例素材", MaxSamples))
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "SAMPLE_CREATE", ResSample, fmt.Sprint(row.ID), "", nil, sampleSnapshot(row))
	})
	return row.ID, err
}

func (s *Service) AdminUpdateSample(ctx context.Context, op Operator, id int64, in SampleInput) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var row model.SampleAsset
		if err := tx.NewSelect().Model(&row).Where("sa.id = ?", id).Where("sa.deleted = 0").For("UPDATE").Scan(ctx); err != nil {
			if noRows(err) {
				return xerr.NotFoundf("示例素材")
			}
			return err
		}
		before := sampleSnapshot(row)
		if err := applySample(&row, in); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*model.SampleAsset)(nil)).Where("id = ?", id).
			Set("url = ?", row.URL).Set("thumb_url = ?", nullableStr(row.ThumbURL)).Set("title = ?", row.Title).
			Set("role = ?", row.Role).Set("sort_order = ?", row.SortOrder).Set("enabled = ?", row.Enabled).Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "SAMPLE_UPDATE", ResSample, fmt.Sprint(id), "", before, sampleSnapshot(row))
	})
}

func (s *Service) AdminDeleteSample(ctx context.Context, op Operator, id int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var row model.SampleAsset
		if err := tx.NewSelect().Model(&row).Where("sa.id = ?", id).Where("sa.deleted = 0").For("UPDATE").Scan(ctx); err != nil {
			if noRows(err) {
				return xerr.NotFoundf("示例素材")
			}
			return err
		}
		if _, err := tx.NewUpdate().Model((*model.SampleAsset)(nil)).Set("deleted = 1").Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "SAMPLE_DELETE", ResSample, fmt.Sprint(id), "", sampleSnapshot(row), nil)
	})
}

// ---------------------------------------------------------------- 行业建议（后台）

// SuggestionFilter 行业建议聚合列表的筛选。
type SuggestionFilter struct {
	Status     string // NEW | ADOPTED | IGNORED
	From, To   string
	Sort       string // count | recent
	Page, Size int
}

// SuggestionAgg 按规范化文本聚合的行业建议。
type SuggestionAgg struct {
	NormText   string  `json:"normText"`
	Text       string  `json:"text"`
	Count      int     `json:"count"`
	Sids       int     `json:"sids"`
	LastAt     string  `json:"lastAt"`
	Status     string  `json:"status"`
	IndustryID *int64  `json:"industryId"`
	HandledAt  *string `json:"handledAt"`
}

// SuggestionPage 行业建议分页；NoTextCount 是「只表示想要新行业、没填文字」的提交总数（只汇总趋势）。
type SuggestionPage struct {
	Total       int             `json:"total"`
	Page        int             `json:"page"`
	Size        int             `json:"size"`
	NoTextCount int             `json:"noTextCount"`
	Items       []SuggestionAgg `json:"items"`
}

// AdminSuggestions 按 norm_text 聚合：文本、次数、去重 sid 数、最近时间、状态。
func (s *Service) AdminSuggestions(ctx context.Context, f SuggestionFilter) (*SuggestionPage, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	page, size := normPage(f.Page, f.Size, 20)
	where := "WHERE s.norm_text IS NOT NULL"
	var args []any
	if f.From != "" {
		t, err := parseAdminTime(f.From)
		if err != nil {
			return nil, err
		}
		where += " AND s.create_time >= FROM_UNIXTIME(? / 1000)"
		args = append(args, t.Time.UnixMilli())
	}
	if f.To != "" {
		t, err := parseAdminTime(f.To)
		if err != nil {
			return nil, err
		}
		where += " AND s.create_time < FROM_UNIXTIME(? / 1000)"
		args = append(args, t.Time.UnixMilli())
	}
	having := ""
	if f.Status != "" {
		if f.Status != "NEW" && f.Status != "ADOPTED" && f.Status != "IGNORED" {
			return nil, errParam("status 只能是 NEW / ADOPTED / IGNORED")
		}
		having = " HAVING status = ?"
	}
	order := "cnt DESC, last_at DESC"
	if f.Sort == "recent" {
		order = "last_at DESC, cnt DESC"
	}
	base := "SELECT s.norm_text, MIN(s.text) AS text, COUNT(*) AS cnt, COUNT(DISTINCT s.sid) AS sids, MAX(s.create_time) AS last_at, " +
		"COALESCE(MAX(st.status), 'NEW') AS status, MAX(st.industry_id) AS industry_id, MAX(st.handled_at) AS handled_at " +
		"FROM afv_plaza_industry_suggestion s LEFT JOIN afv_plaza_industry_suggestion_status st ON st.norm_text = s.norm_text " +
		where + " GROUP BY s.norm_text" + having
	qargs := append([]any{}, args...)
	if having != "" {
		qargs = append(qargs, f.Status)
	}
	type aggRow struct {
		NormText   string         `bun:"norm_text"`
		Text       sql.NullString `bun:"text"`
		Cnt        int            `bun:"cnt"`
		Sids       int            `bun:"sids"`
		LastAt     sql.NullTime   `bun:"last_at"`
		Status     string         `bun:"status"`
		IndustryID sql.NullInt64  `bun:"industry_id"`
		HandledAt  sql.NullTime   `bun:"handled_at"`
	}
	var total int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM ("+base+") x", qargs...).Scan(ctx, &total); err != nil {
		return nil, err
	}
	var rows []aggRow
	pargs := append(append([]any{}, qargs...), size, (page-1)*size)
	if err := s.db.NewRaw(base+" ORDER BY "+order+" LIMIT ? OFFSET ?", pargs...).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	var noText int
	if err := s.db.NewSelect().Model((*model.IndustrySuggestion)(nil)).Where("norm_text IS NULL").ColumnExpr("COUNT(*)").Scan(ctx, &noText); err != nil {
		return nil, err
	}
	out := &SuggestionPage{Total: total, Page: page, Size: size, NoTextCount: noText, Items: make([]SuggestionAgg, 0, len(rows))}
	for _, r := range rows {
		it := SuggestionAgg{NormText: r.NormText, Text: r.Text.String, Count: r.Cnt, Sids: r.Sids, Status: r.Status, HandledAt: timeStr(r.HandledAt)}
		if r.LastAt.Valid {
			it.LastAt = r.LastAt.Time.Format(timeLayout)
		}
		if r.IndustryID.Valid {
			v := r.IndustryID.Int64
			it.IndustryID = &v
		}
		out.Items = append(out.Items, it)
	}
	return out, nil
}

// AdminHandleSuggestion 标记处理：adopt（带 industryId，前端先用预填表单建好行业再调）/ ignore。
func (s *Service) AdminHandleSuggestion(ctx context.Context, op Operator, normText, status string, industryID int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	if normText = strings.TrimSpace(normText); normText == "" {
		return errParam("缺少 normText")
	}
	if status != "ADOPTED" && status != "IGNORED" {
		return errParam("status 只能是 ADOPTED 或 IGNORED")
	}
	if status == "ADOPTED" && industryID <= 0 {
		return errParam("采纳需要关联新建的行业 industryId")
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if n, err := tx.NewSelect().Model((*model.IndustrySuggestion)(nil)).Where("norm_text = ?", normText).Count(ctx); err != nil {
			return err
		} else if n == 0 {
			return xerr.NotFoundf("行业建议")
		}
		if status == "ADOPTED" {
			if n, err := tx.NewSelect().Model((*model.Industry)(nil)).Where("id = ?", industryID).Where("deleted = 0").Count(ctx); err != nil {
				return err
			} else if n == 0 {
				return xerr.NewReason(xerr.NotFound, ReasonIndustryNotFound, "关联的行业不存在")
			}
		}
		var ind any
		if industryID > 0 {
			ind = industryID
		}
		if _, err := tx.NewRaw("INSERT INTO afv_plaza_industry_suggestion_status (norm_text, status, industry_id, handled_by, handled_at) VALUES (?, ?, ?, ?, NOW()) "+
			"ON DUPLICATE KEY UPDATE status = VALUES(status), industry_id = VALUES(industry_id), handled_by = VALUES(handled_by), handled_at = NOW()",
			normText, status, ind, nullInt(op.ID)).Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "SUGGESTION_"+status, ResSuggestion, normText, "", nil, map[string]any{"status": status, "industryId": ind})
	})
}

// ---------------------------------------------------------------- 搜索缺口（后台，17.4）

// MissFilter 搜索缺口筛选。
type MissFilter struct {
	Industry   string
	From, To   string
	Status     string // NEW | HANDLED | IGNORED
	Sort       string // cnt | recent
	Page, Size int
}

// MissItem 一个零结果搜索词的汇总。
type MissItem struct {
	IndustryCode string  `json:"industryCode"`
	NormTerm     string  `json:"normTerm"`
	Count        int     `json:"count"`
	FirstDay     string  `json:"firstDay"`
	LastDay      string  `json:"lastDay"`
	Status       string  `json:"status"`
	HandledAt    *string `json:"handledAt"`
}

// MissPage 搜索缺口分页（只有聚合，没有任何用户信息）。
type MissPage struct {
	Total int        `json:"total"`
	Page  int        `json:"page"`
	Size  int        `json:"size"`
	Items []MissItem `json:"items"`
}

// AdminSearchMisses 零结果搜索词：按词汇总（词、行业、次数、首次 / 最近日期、状态）。给了 from / to 就按日表在区间内重新累计。
func (s *Service) AdminSearchMisses(ctx context.Context, f MissFilter) (*MissPage, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	page, size := normPage(f.Page, f.Size, 20)
	if f.Status != "" && f.Status != "NEW" && f.Status != "HANDLED" && f.Status != "IGNORED" {
		return nil, errParam("status 只能是 NEW / HANDLED / IGNORED")
	}
	var args []any
	var base string
	if f.From != "" || f.To != "" {
		cond := "WHERE 1=1"
		if f.Industry != "" {
			cond += " AND d.industry_code = ?"
			args = append(args, f.Industry)
		}
		if f.From != "" {
			t, err := parseAdminTime(f.From)
			if err != nil {
				return nil, err
			}
			cond += " AND d.`day` >= ?"
			args = append(args, t.Time.In(cst).Format("2006-01-02"))
		}
		if f.To != "" {
			t, err := parseAdminTime(f.To)
			if err != nil {
				return nil, err
			}
			cond += " AND d.`day` <= ?"
			args = append(args, t.Time.In(cst).Format("2006-01-02"))
		}
		base = "SELECT d.industry_code, d.norm_term, SUM(d.cnt) AS cnt, MIN(d.`day`) AS first_day, MAX(d.`day`) AS last_day, " +
			"COALESCE(MAX(m.status), 'NEW') AS status, MAX(m.handled_at) AS handled_at " +
			"FROM afv_plaza_search_miss_daily d LEFT JOIN afv_plaza_search_miss_term m ON m.industry_code = d.industry_code AND m.norm_term = d.norm_term " +
			cond + " GROUP BY d.industry_code, d.norm_term"
	} else {
		cond := "WHERE 1=1"
		if f.Industry != "" {
			cond += " AND m.industry_code = ?"
			args = append(args, f.Industry)
		}
		base = "SELECT m.industry_code, m.norm_term, m.total_cnt AS cnt, m.first_day, m.last_day, m.status, m.handled_at FROM afv_plaza_search_miss_term m " + cond
	}
	wrapped := "SELECT * FROM (" + base + ") x"
	if f.Status != "" {
		wrapped += " WHERE x.status = ?"
		args = append(args, f.Status)
	}
	order := "x.cnt DESC, x.last_day DESC"
	if f.Sort == "recent" {
		order = "x.last_day DESC, x.cnt DESC"
	}
	type missRow struct {
		IndustryCode string       `bun:"industry_code"`
		NormTerm     string       `bun:"norm_term"`
		Cnt          int          `bun:"cnt"`
		FirstDay     sql.NullTime `bun:"first_day"`
		LastDay      sql.NullTime `bun:"last_day"`
		Status       string       `bun:"status"`
		HandledAt    sql.NullTime `bun:"handled_at"`
	}
	var total int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM ("+wrapped+") y", args...).Scan(ctx, &total); err != nil {
		return nil, err
	}
	var rows []missRow
	if err := s.db.NewRaw(wrapped+" ORDER BY "+order+" LIMIT ? OFFSET ?", append(append([]any{}, args...), size, (page-1)*size)...).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := &MissPage{Total: total, Page: page, Size: size, Items: make([]MissItem, 0, len(rows))}
	for _, r := range rows {
		it := MissItem{IndustryCode: r.IndustryCode, NormTerm: r.NormTerm, Count: r.Cnt, Status: r.Status, HandledAt: timeStr(r.HandledAt)}
		if r.FirstDay.Valid {
			it.FirstDay = r.FirstDay.Time.Format("2006-01-02")
		}
		if r.LastDay.Valid {
			it.LastDay = r.LastDay.Time.Format("2006-01-02")
		}
		out.Items = append(out.Items, it)
	}
	return out, nil
}

// AdminHandleSearchMiss 标记「已处理 / 忽略」。
func (s *Service) AdminHandleSearchMiss(ctx context.Context, op Operator, industryCode, term, status string) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	if status != "HANDLED" && status != "IGNORED" && status != "NEW" {
		return errParam("status 只能是 HANDLED / IGNORED / NEW")
	}
	industryCode, term = strings.TrimSpace(industryCode), strings.TrimSpace(term)
	if industryCode == "" || term == "" {
		return errParam("缺少 industry 或 term")
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*model.SearchMissTerm)(nil)).Set("status = ?", status).
			Set("handled_by = ?", nullInt(op.ID)).Set("handled_at = NOW()").
			Where("industry_code = ?", industryCode).Where("norm_term = ?", term).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var exist int
			if err := tx.NewSelect().Model((*model.SearchMissTerm)(nil)).Where("industry_code = ?", industryCode).Where("norm_term = ?", term).ColumnExpr("COUNT(*)").Scan(ctx, &exist); err != nil {
				return err
			}
			if exist == 0 {
				return xerr.NotFoundf("搜索词")
			}
		}
		return s.audit(ctx, tx, op, "SEARCH_MISS_"+status, ResSearchMiss, industryCode+"/"+term, "", nil, map[string]any{"status": status})
	})
}
