package core

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
)

// 我的模板（DESIGN 附章 A.1-#6、EXPERIENCE 3.9 / 6.x R-05）：
//   - 把一次做同款的设置（模板 id、入参值、版数、文字）存成私有模板；严格按行业——创建时记下所选行业，
//     读取时「记录的行业属于所选范围，或模板自己属于所选范围」才算当前行业的。
//   - 入参按模板「当前」slots schema 校验（复用 ValidateInputs）；schema 之后变了，读取时标出哪些设置已失效
//     （staleKeys / stale），只下发仍然有效的那部分入参，由前端恢复并提示「部分设置已失效」。
//   - 模板被下线后记录保留，读取时 templateAvailable=false（可改名 / 删除，不能恢复）。
//   - 每人上限 100。

const (
	// MaxMyTemplates 每人我的模板上限。
	MaxMyTemplates = 100
	myNameMaxRunes = 40
	myTextMaxRunes = 500
	maxMyVariants  = 4
	// StaleHint 部分设置已失效的提示文案。
	StaleHint = "部分设置已失效"
)

// ReasonMyTemplateLimit 我的模板已满（4002）。
const ReasonMyTemplateLimit = "MY_TEMPLATE_LIMIT"

// MyTemplate 我的模板行（afv_plaza_my_template，Go 独写）。
type MyTemplate struct {
	bun.BaseModel `bun:"table:afv_plaza_my_template,alias:mt"`

	ID         int64           `bun:"id,pk,autoincrement"`
	UserID     int64           `bun:"user_id,notnull"`
	Name       string          `bun:"name,notnull"`
	TemplateID int64           `bun:"template_id,notnull"`
	IndustryID int64           `bun:"industry_id,notnull"`
	InputsJSON json.RawMessage `bun:"inputs_json,type:json,nullzero"`
	Variants   int             `bun:"variants,notnull"`
	Resolution string          `bun:"resolution,notnull"`
	Text       string          `bun:"text,notnull"`
	CreateTime time.Time       `bun:"create_time,notnull,default:current_timestamp"`
	UpdateTime time.Time       `bun:"update_time,notnull,default:current_timestamp"`
}

// MyTemplateInput 创建（全字段）/ 修改（nil = 不改）。
type MyTemplateInput struct {
	Name        *string
	TemplateID  int64
	Industry    string // 创建时所选行业 code（可空：取模板自己的第一个有效行业）
	SubIndustry string
	Inputs      *map[string]json.RawMessage
	Variants    *int
	// Resolution 出图档位 1k / 2k / 4k（nil = 创建时缺省 1k / 修改时不改；校验同 /quote）
	Resolution *string
	Text       *string
}

// MyTemplateItem 我的模板（读取形状）。
type MyTemplateItem struct {
	ID                int64          `json:"id"`
	Name              string         `json:"name"`
	TemplateID        int64          `json:"templateId"`
	Industry          string         `json:"industry"` // 创建时所选行业 code（行业已失效则空）
	Inputs            map[string]any `json:"inputs"`   // 仍然有效的入参（失效的已剔除，见 staleKeys）
	Variants          int            `json:"variants"`
	Resolution        string         `json:"resolution"`
	Text              string         `json:"text"`
	CreatedAt         string         `json:"createdAt"`
	UpdatedAt         string         `json:"updatedAt"`
	TemplateAvailable bool           `json:"templateAvailable"`
	Template          *TemplateItem  `json:"template"` // 含 slots；不可用时 null
	Stale             bool           `json:"stale"`
	StaleKeys         []string       `json:"staleKeys"`
	StaleHint         string         `json:"staleHint,omitempty"`
}

// MyTemplateList GET /plaza/my-templates 的响应。
type MyTemplateList struct {
	Items []MyTemplateItem `json:"items"`
	Total int              `json:"total"`
	Limit int              `json:"limit"`
}

func errMyLimit() error {
	return xerr.NewReason(xerr.Conflict, ReasonMyTemplateLimit, "我的模板已满（最多 100 个），删除一些再保存")
}

func errMyNotFound() error { return xerr.New(xerr.NotFound, "这个模板不存在或已被删除") }

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if s == "" {
		return "", errParam("name 不能为空")
	}
	if utf8.RuneCountInString(s) > myNameMaxRunes {
		return "", errParam("name 最多 40 个字")
	}
	return s, nil
}

func cleanMyText(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > myTextMaxRunes {
		return "", errParam("text 最多 500 个字")
	}
	return s, nil
}

func checkVariants(v int) error {
	if v < 1 || v > maxMyVariants {
		return errParam("variants 必须在 1 到 4 之间")
	}
	return nil
}

// validateMyInputs 按模板当前 slots schema 校验并规整入参（不可见模板 → ErrTemplateGone）。
func (s *Service) validateMyInputs(c *Catalog, templateID int64, inputs map[string]json.RawMessage) ([]byte, error) {
	rec, err := c.Get(templateID, s.now())
	if err != nil {
		return nil, err
	}
	schema, perr := ParseSlotSchema(rec.t.SlotsJSON)
	if perr != nil {
		return nil, xerr.New(xerr.ServerError, "模板入参定义有误，请联系运营")
	}
	norm, err := ValidateInputs(schema, inputs)
	if err != nil {
		return nil, err
	}
	return json.Marshal(norm)
}

// CreateMyTemplate 新建。name 必填（≤40 字）；variants 缺省 1（1~4）；inputs 按模板当前 schema 校验。
func (s *Service) CreateMyTemplate(ctx context.Context, userID int64, in MyTemplateInput) (*MyTemplateItem, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	if userID <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	if in.TemplateID <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少 templateId")
	}
	name := ""
	if in.Name != nil {
		name = *in.Name
	}
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	variants := 1
	if in.Variants != nil {
		variants = *in.Variants
		if err := checkVariants(variants); err != nil {
			return nil, err
		}
	}
	resolution := defaultQuoteRe
	if in.Resolution != nil {
		if resolution, err = normalizeResolution(*in.Resolution); err != nil {
			return nil, err
		}
	}
	text := ""
	if in.Text != nil {
		if text, err = cleanMyText(*in.Text); err != nil {
			return nil, err
		}
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := c.Get(in.TemplateID, s.now())
	if err != nil {
		return nil, err
	}
	// 归属行业：给了就必须是模板所属范围，没给取模板自己的第一个有效行业
	var industryID int64
	if strings.TrimSpace(in.Industry) != "" {
		sc, err := s.resolve(c, in.Industry, in.SubIndustry)
		if err != nil {
			return nil, err
		}
		if _, ok := c.InScope(sc, in.TemplateID, s.now()); !ok {
			return nil, ErrTemplateGone()
		}
		industryID = sc.Sel.ID
	} else if refs := c.industriesOf(rec); len(refs) > 0 {
		industryID = refs[0].ID
	}
	inputs := map[string]json.RawMessage{}
	if in.Inputs != nil && *in.Inputs != nil {
		inputs = *in.Inputs
	}
	inputsJSON, err := s.validateMyInputs(c, in.TemplateID, inputs)
	if err != nil {
		return nil, err
	}
	now := s.now().In(cst).Format("2006-01-02 15:04:05")
	var id int64
	err = s.withUserLock(ctx, userID, func(ctx context.Context, tx bun.Tx) error {
		n, err := tx.NewSelect().Model((*MyTemplate)(nil)).Where("user_id = ?", userID).Count(ctx)
		if err != nil {
			return err
		}
		if n >= MaxMyTemplates {
			return errMyLimit()
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO afv_plaza_my_template (user_id, name, template_id, industry_id, inputs_json, variants, resolution, text, create_time, update_time) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			userID, name, in.TemplateID, industryID, string(inputsJSON), variants, resolution, text, now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getMyItem(ctx, userID, id)
}

// UpdateMyTemplate 修改名称 / 入参 / 版数 / 文字（给了才改）。给了 inputs 时按模板当前 schema 重新校验（模板已下线则拒）。
func (s *Service) UpdateMyTemplate(ctx context.Context, userID, id int64, in MyTemplateInput) (*MyTemplateItem, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	if userID <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	row, err := s.loadMy(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	q := s.db.NewUpdate().Model((*MyTemplate)(nil)).Where("id = ?", id).Where("user_id = ?", userID).
		Set("update_time = ?", s.now().In(cst).Format("2006-01-02 15:04:05"))
	if in.Name != nil {
		name, err := cleanName(*in.Name)
		if err != nil {
			return nil, err
		}
		q = q.Set("name = ?", name)
	}
	if in.Variants != nil {
		if err := checkVariants(*in.Variants); err != nil {
			return nil, err
		}
		q = q.Set("variants = ?", *in.Variants)
	}
	if in.Resolution != nil {
		r, err := normalizeResolution(*in.Resolution)
		if err != nil {
			return nil, err
		}
		q = q.Set("resolution = ?", r)
	}
	if in.Text != nil {
		text, err := cleanMyText(*in.Text)
		if err != nil {
			return nil, err
		}
		q = q.Set("text = ?", text)
	}
	if in.Inputs != nil {
		inputs := *in.Inputs
		if inputs == nil {
			inputs = map[string]json.RawMessage{}
		}
		c, err := s.catalog(ctx)
		if err != nil {
			return nil, err
		}
		b, err := s.validateMyInputs(c, row.TemplateID, inputs)
		if err != nil {
			return nil, err
		}
		q = q.Set("inputs_json = ?", string(b))
	}
	if _, err := q.Exec(ctx); err != nil {
		return nil, err
	}
	return s.getMyItem(ctx, userID, id)
}

// DeleteMyTemplate 删除（幂等：不存在也算成功；只能删自己的）。
func (s *Service) DeleteMyTemplate(ctx context.Context, userID, id int64) error {
	if s.db == nil {
		return ErrNotReady()
	}
	if userID <= 0 {
		return xerr.New(xerr.Unauthorized, "请先登录")
	}
	_, err := s.db.NewDelete().Model((*MyTemplate)(nil)).Where("id = ?", id).Where("user_id = ?", userID).Exec(ctx)
	return err
}

// ListMyTemplates 当前用户在所选行业里的我的模板，创建时间倒序。industry 必填。
func (s *Service) ListMyTemplates(ctx context.Context, userID int64, industry, sub string) (*MyTemplateList, error) {
	if userID <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	if strings.TrimSpace(industry) == "" {
		return nil, ErrIndustryRequired()
	}
	if s.db == nil {
		return nil, ErrNotReady()
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.resolve(c, industry, sub)
	if err != nil {
		return nil, err
	}
	var rows []MyTemplate
	if err := s.db.NewSelect().Model(&rows).Where("mt.user_id = ?", userID).
		Order("mt.id DESC").Limit(MaxMyTemplates).Scan(ctx); err != nil {
		return nil, err
	}
	now := s.now()
	usage := s.usageMap(ctx)
	out := &MyTemplateList{Items: []MyTemplateItem{}, Limit: MaxMyTemplates}
	for i := range rows {
		r := &rows[i]
		belongs := false
		for _, id := range sc.ids {
			belongs = belongs || id == r.IndustryID
		}
		if !belongs {
			_, belongs = c.InScope(sc, r.TemplateID, now)
		}
		if !belongs {
			continue
		}
		out.Items = append(out.Items, s.myItem(c, r, usage, now))
	}
	out.Total = len(out.Items)
	return out, nil
}

func (s *Service) loadMy(ctx context.Context, userID, id int64) (*MyTemplate, error) {
	var row MyTemplate
	err := s.db.NewSelect().Model(&row).Where("mt.id = ?", id).Where("mt.user_id = ?", userID).Limit(1).Scan(ctx)
	if err != nil {
		if noRows(err) {
			return nil, errMyNotFound()
		}
		return nil, err
	}
	return &row, nil
}

func (s *Service) getMyItem(ctx context.Context, userID, id int64) (*MyTemplateItem, error) {
	row, err := s.loadMy(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	it := s.myItem(c, row, s.usageMap(ctx), s.now())
	return &it, nil
}

// myItem 组装读取形状并做「失效检查」：模板不可用 → templateAvailable=false；模板在 → 用当前 schema 逐键检查已存的入参。
func (s *Service) myItem(c *Catalog, r *MyTemplate, usage map[int64]int64, now time.Time) MyTemplateItem {
	it := MyTemplateItem{
		ID: r.ID, Name: r.Name, TemplateID: r.TemplateID, Variants: r.Variants, Resolution: r.Resolution, Text: r.Text,
		CreatedAt: r.CreateTime.Format(timeLayout), UpdatedAt: r.UpdateTime.Format(timeLayout),
		Inputs: map[string]any{}, StaleKeys: []string{},
	}
	if n := c.LookupID(r.IndustryID); n != nil && n.effective() {
		it.Industry = n.Code
	}
	saved := map[string]json.RawMessage{}
	if len(r.InputsJSON) > 0 {
		_ = json.Unmarshal(r.InputsJSON, &saved)
	}
	rec, err := c.Get(r.TemplateID, now)
	if err != nil {
		for k, v := range saved { // 模板已下线：入参原样给出，仅供查看
			var x any
			if json.Unmarshal(v, &x) == nil {
				it.Inputs[k] = x
			}
		}
		return it
	}
	ti := s.item(c, rec, usage, now)
	schema, perr := ParseSlotSchema(rec.t.SlotsJSON)
	if perr == nil {
		ti.Slots = schema
	}
	it.TemplateAvailable, it.Template = true, &ti
	valid, stale := splitStaleInputs(schema, perr == nil, saved)
	it.Inputs, it.StaleKeys = valid, stale
	if len(stale) > 0 {
		it.Stale, it.StaleHint = true, StaleHint
	}
	return it
}

// splitStaleInputs 用模板当前 schema 检查已存入参：返回仍然有效的入参与失效的键（排序）。
// 失效 = schema 里已没有这个键，或值不再满足当前约束（类型 / 范围 / 选项 / 长度 / 数量…）。
// 「新增了必填项而没存值」不算失效（表单里自然是空的，用户补上即可）。schema 解析不了（脏数据）→ 全部视为失效。
func splitStaleInputs(schema *SlotSchema, schemaOK bool, saved map[string]json.RawMessage) (map[string]any, []string) {
	valid := map[string]any{}
	var stale []string
	if !schemaOK {
		for k := range saved {
			stale = append(stale, k)
		}
		sort.Strings(stale)
		return valid, stale
	}
	cur := make(map[string]json.RawMessage, len(saved))
	for k, v := range saved {
		cur[k] = v
	}
	staleSet := map[string]bool{}
	for round := 0; round < 8; round++ { // ValidateInputs 一次最多报 20 处：剔除后再验，直到没有新的失效键
		norm, err := ValidateInputs(schema, cur)
		if err == nil {
			for k, v := range norm {
				valid[k] = v
			}
			break
		}
		bad := map[string]bool{}
		if se := AsSlotsError(err); se != nil {
			for _, f := range se.Fields {
				if k := inputKeyOf(f.Field); k != "" {
					if _, has := cur[k]; has {
						bad[k] = true
					}
				} else { // 不指向具体键（如键数量超限）：整体失效
					for k := range cur {
						bad[k] = true
					}
				}
			}
		} else {
			for k := range cur {
				bad[k] = true
			}
		}
		if len(bad) == 0 { // 只剩「缺必填」之类：把能用的原样给出
			for k, v := range cur {
				var x any
				if json.Unmarshal(v, &x) == nil {
					valid[k] = x
				}
			}
			break
		}
		for k := range bad {
			staleSet[k] = true
			delete(cur, k)
		}
	}
	for k := range staleSet {
		stale = append(stale, k)
	}
	sort.Strings(stale)
	return valid, stale
}

// inputKeyOf "inputs.photo[0]" / "inputs.photo.x" → "photo"；不是 inputs.<key> 形态 → ""。
func inputKeyOf(path string) string {
	rest, ok := strings.CutPrefix(path, "inputs.")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, ".["); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
