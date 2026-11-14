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
	"github.com/zeromicro/go-zero/core/logx"
)

// 行业字段上限（16.1）。
const (
	maxIndustryName    = 32
	maxIndustryHint    = 120
	maxAudienceName    = 32
	maxExamples        = 5
	maxExampleRunes    = 40
	maxNextActions     = 4
	maxNextActionRunes = 20
)

// IndustryInput 新建 / 编辑行业的入参。指针字段 nil = 不改（新建时按缺省）。code 只能新建时给，编辑不允许改。
type IndustryInput struct {
	Code          string    `json:"code"`
	Name          *string   `json:"name"`
	ParentID      *int64    `json:"parentId"`
	IconURL       *string   `json:"iconUrl"`
	CoverURL      *string   `json:"coverUrl"`
	Hint          *string   `json:"hint"`
	AudienceName  *string   `json:"audienceName"`
	Examples      *[]string `json:"examples"`
	NextActions   *[]string `json:"nextActions"`
	SortOrder     *int      `json:"sortOrder"`
	Enabled       *bool     `json:"enabled"`
	ShowWhenEmpty *bool     `json:"showWhenEmpty"`
}

func validateStringList(name string, in []string, max, maxRunes int) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if runeLen(s) > maxRunes {
			return nil, errParam(fmt.Sprintf("%s每条最多 %d 个字", name, maxRunes))
		}
		out = append(out, s)
	}
	if len(out) > max {
		return nil, errParam(fmt.Sprintf("%s最多 %d 条", name, max))
	}
	return out, nil
}

// applyIndustryFields 把入参合并进行业行并校验：name 必填 ≤32；启用的行业 hint 必填 ≤120；
// audienceName ≤32；examples ≤5 条每条 ≤40 字；nextActions ≤4 条每条 ≤20 字。
func applyIndustryFields(row *model.Industry, in IndustryInput) error {
	if in.Name != nil {
		row.Name = strings.TrimSpace(*in.Name)
	}
	if row.Name == "" || runeLen(row.Name) > maxIndustryName {
		return errParam("行业名称必填，最多 32 个字")
	}
	if in.IconURL != nil {
		row.IconURL = nullStr(in.IconURL)
	}
	if in.CoverURL != nil {
		row.CoverURL = nullStr(in.CoverURL)
	}
	if in.Hint != nil {
		row.Hint = nullStr(in.Hint)
	}
	if row.Hint.Valid && runeLen(row.Hint.String) > maxIndustryHint {
		return errParam("行业说明最多 120 个字")
	}
	if in.AudienceName != nil {
		row.AudienceName = nullStr(in.AudienceName)
	}
	if row.AudienceName.Valid && runeLen(row.AudienceName.String) > maxAudienceName {
		return errParam("人群称呼最多 32 个字")
	}
	if in.Examples != nil {
		list, err := validateStringList("输入示例", *in.Examples, maxExamples, maxExampleRunes)
		if err != nil {
			return err
		}
		row.ExamplesJSON = marshalOrNil(list)
	}
	if in.NextActions != nil {
		list, err := validateStringList("下一步建议", *in.NextActions, maxNextActions, maxNextActionRunes)
		if err != nil {
			return err
		}
		row.NextActionsJSON = marshalOrNil(list)
	}
	if in.SortOrder != nil {
		row.SortOrder = *in.SortOrder
	}
	if in.Enabled != nil {
		row.Enabled = b2i(*in.Enabled)
	}
	if in.ShowWhenEmpty != nil {
		row.ShowWhenEmpty = b2i(*in.ShowWhenEmpty)
	}
	if row.Enabled == 1 && !row.Hint.Valid {
		return errParam("启用的行业必须填写说明（行业门按钮下的副标题）")
	}
	return nil
}

func marshalOrNil(list []string) json.RawMessage {
	if len(list) == 0 {
		return nil
	}
	b, _ := json.Marshal(list)
	return b
}

func b2i(b bool) int8 {
	if b {
		return 1
	}
	return 0
}

func industrySnapshot(r model.Industry) map[string]any {
	m := map[string]any{
		"id": r.ID, "code": r.Code, "name": r.Name, "iconUrl": strPtr(r.IconURL), "coverUrl": strPtr(r.CoverURL),
		"hint": strPtr(r.Hint), "audienceName": strPtr(r.AudienceName), "sortOrder": r.SortOrder,
		"enabled": r.Enabled == 1, "showWhenEmpty": r.ShowWhenEmpty == 1,
		"examples": jsonStrings(r.ExamplesJSON), "nextActions": jsonStrings(r.NextActionsJSON),
	}
	if r.ParentID.Valid {
		m["parentId"] = r.ParentID.Int64
	} else {
		m["parentId"] = nil
	}
	return m
}

// AdminCreateIndustry 新建行业。parentId 的目标必须是一级（只允许两级）；code 全局唯一且不得为 ALL。
func (s *Service) AdminCreateIndustry(ctx context.Context, op Operator, in IndustryInput) (int64, error) {
	if err := s.requireDB(); err != nil {
		return 0, err
	}
	in.Code = strings.TrimSpace(in.Code)
	if err := validateIndustryCode(in.Code); err != nil {
		return 0, err
	}
	row := model.Industry{Code: in.Code, Enabled: 1}
	if err := applyIndustryFields(&row, in); err != nil {
		return 0, err
	}
	if op.ID > 0 {
		row.CreatedBy = sql.NullInt64{Int64: op.ID, Valid: true}
		row.UpdatedBy = row.CreatedBy
	}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if in.ParentID != nil {
			var parent model.Industry
			if err := tx.NewSelect().Model(&parent).Where("ti.id = ?", *in.ParentID).Where("ti.deleted = 0").Scan(ctx); err != nil {
				if noRows(err) {
					return errParam("父行业不存在")
				}
				return err
			}
			if parent.ParentID.Valid {
				return xerr.NewReason(xerr.ParamError, ReasonIndustryDepthExceeded, "只支持两级行业，二级下面不能再建子级")
			}
			row.ParentID = sql.NullInt64{Int64: *in.ParentID, Valid: true}
		}
		n, err := tx.NewSelect().Model((*model.Industry)(nil)).Where("code = ?", row.Code).Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return xerr.NewReason(xerr.Conflict, ReasonIndustryCodeDuplicate, "行业 code 已存在")
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			if isDuplicate(err) {
				return xerr.NewReason(xerr.Conflict, ReasonIndustryCodeDuplicate, "行业 code 已存在")
			}
			return err
		}
		return s.audit(ctx, tx, op, "INDUSTRY_CREATE", ResIndustry, fmt.Sprint(row.ID), "", nil, industrySnapshot(row))
	})
	if err != nil {
		return 0, err
	}
	s.Invalidate(ctx)
	return row.ID, nil
}

// IndustryUpdateResult 编辑结果：停用一级会让其子树不可见，返回受影响的模板数 / 偏好人数供二次确认的展示。
type IndustryUpdateResult struct {
	ID                int64 `json:"id"`
	AffectedTemplates int   `json:"affectedTemplates"`
	AffectedUsers     int   `json:"affectedUsers"`
}

// AdminUpdateIndustry 改名称 / 图标 / 说明 / 示例 / enabled / showWhenEmpty；不允许改 code。
func (s *Service) AdminUpdateIndustry(ctx context.Context, op Operator, id int64, in IndustryInput) (*IndustryUpdateResult, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Code) != "" {
		var cur model.Industry
		if err := s.db.NewSelect().Model(&cur).Column("code").Where("ti.id = ?", id).Scan(ctx); err == nil && cur.Code != strings.TrimSpace(in.Code) {
			return nil, errParam("行业 code 创建后不可修改")
		}
	}
	res := &IndustryUpdateResult{ID: id}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var row model.Industry
		if err := tx.NewSelect().Model(&row).Where("ti.id = ?", id).Where("ti.deleted = 0").For("UPDATE").Scan(ctx); err != nil {
			if noRows(err) {
				return xerr.NotFoundf("行业")
			}
			return err
		}
		before := industrySnapshot(row)
		if err := applyIndustryFields(&row, in); err != nil {
			return err
		}
		if row.Enabled == 0 && before["enabled"] == true {
			res.AffectedTemplates, res.AffectedUsers = affectedBy(ctx, tx, row)
		}
		set := map[string]any{
			"name": row.Name, "icon_url": nullableStr(row.IconURL), "cover_url": nullableStr(row.CoverURL),
			"hint": nullableStr(row.Hint), "audience_name": nullableStr(row.AudienceName),
			"examples_json": nullableJSON(row.ExamplesJSON), "next_actions_json": nullableJSON(row.NextActionsJSON),
			"sort_order": row.SortOrder, "enabled": row.Enabled, "show_when_empty": row.ShowWhenEmpty,
		}
		if op.ID > 0 {
			set["updated_by"] = op.ID
		}
		q := tx.NewUpdate().Model((*model.Industry)(nil)).Where("id = ?", id)
		for k, v := range set { // 定向列更新，不整行写回
			q = q.Set("? = ?", bun.Ident(k), v)
		}
		if _, err := q.Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "INDUSTRY_UPDATE", ResIndustry, fmt.Sprint(id), "", before, industrySnapshot(row))
	})
	if err != nil {
		return nil, err
	}
	s.Invalidate(ctx)
	return res, nil
}

func nullableStr(n sql.NullString) any {
	if !n.Valid {
		return nil
	}
	return n.String
}

func nullableJSON(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

// affectedBy 停用某行业波及的模板数（含其子级）与偏好人数。
func affectedBy(ctx context.Context, tx bun.IDB, row model.Industry) (templates, users int) {
	ids := []int64{row.ID}
	var kids []int64
	_ = tx.NewSelect().Model((*model.Industry)(nil)).Column("id").Where("parent_id = ?", row.ID).Where("deleted = 0").Scan(ctx, &kids)
	ids = append(ids, kids...)
	_ = tx.NewSelect().Model((*model.TemplateIndustry)(nil)).ColumnExpr("COUNT(DISTINCT template_id)").Where("industry_id IN (?)", bun.In(ids)).Scan(ctx, &templates)
	_ = tx.NewSelect().Model((*model.UserIndustryPref)(nil)).ColumnExpr("COUNT(DISTINCT user_id)").Where("industry_id IN (?)", bun.In(ids)).Scan(ctx, &users)
	return
}

// SortItem 批量排序项。
type SortItem struct {
	ID        int64  `json:"id"`
	ParentID  *int64 `json:"parentId"`
	SortOrder int    `json:"sortOrder"`
}

// planSort 校验批量排序：深度 ≤2、父级必须是一级、不能挂到自己下、有子级的一级不能降为二级。
// all 是全部未删除行业（id → 行）；返回每个 id 的最终 parent（0 = 一级）。
func planSort(all map[int64]model.Industry, items []SortItem) (map[int64]int64, error) {
	if len(items) == 0 {
		return nil, errParam("排序项不能为空")
	}
	final := map[int64]int64{} // 全部行业的最终 parent
	for id, r := range all {
		final[id] = r.ParentID.Int64
	}
	seen := map[int64]bool{}
	for _, it := range items {
		if _, ok := all[it.ID]; !ok {
			return nil, errParam(fmt.Sprintf("行业 %d 不存在", it.ID))
		}
		if seen[it.ID] {
			return nil, errParam("排序项里有重复的行业")
		}
		seen[it.ID] = true
		p := int64(0)
		if it.ParentID != nil {
			p = *it.ParentID
		}
		if p == it.ID {
			return nil, errParam("行业不能挂在自己下面")
		}
		final[it.ID] = p
	}
	for id, p := range final {
		if p == 0 {
			continue
		}
		if _, ok := all[p]; !ok {
			return nil, errParam(fmt.Sprintf("父行业 %d 不存在", p))
		}
		if final[p] != 0 {
			return nil, xerr.NewReason(xerr.ParamError, ReasonIndustryDepthExceeded, "只支持两级行业，不能挂到二级下面")
		}
		// 有子级的一级不能降为二级：p 是 id 的父级，而 id 自己（作为 p 的子级）还挂着子级
		for _, cp := range final {
			if cp == id {
				return nil, xerr.NewReason(xerr.ParamError, ReasonIndustryDepthExceeded, "有子级的一级行业不能降为二级")
			}
		}
	}
	return final, nil
}

// AdminSortIndustries 批量调整顺序与层级（一次事务）。
func (s *Service) AdminSortIndustries(ctx context.Context, op Operator, items []SortItem) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var rows []model.Industry
		if err := tx.NewSelect().Model(&rows).Where("ti.deleted = 0").For("UPDATE").Scan(ctx); err != nil {
			return err
		}
		all := map[int64]model.Industry{}
		for _, r := range rows {
			all[r.ID] = r
		}
		if _, err := planSort(all, items); err != nil {
			return err
		}
		before := map[string]any{}
		after := map[string]any{}
		for _, it := range items {
			cur := all[it.ID]
			before[fmt.Sprint(it.ID)] = map[string]any{"parentId": cur.ParentID.Int64, "sortOrder": cur.SortOrder}
			p := any(nil)
			if it.ParentID != nil && *it.ParentID != 0 {
				p = *it.ParentID
			}
			if _, err := tx.NewUpdate().Model((*model.Industry)(nil)).
				Set("parent_id = ?", p).Set("sort_order = ?", it.SortOrder).Where("id = ?", it.ID).Exec(ctx); err != nil {
				return err
			}
			var pv int64
			if it.ParentID != nil {
				pv = *it.ParentID
			}
			after[fmt.Sprint(it.ID)] = map[string]any{"parentId": pv, "sortOrder": it.SortOrder}
		}
		return s.audit(ctx, tx, op, "INDUSTRY_SORT", ResIndustry, "", "", before, after)
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}

// checkReassignTarget 删除时迁移目标：必须存在、不是自己、同级别或是被删行业的父级；
// 被删行业有子级时目标必须是一级（子级改挂到它下面，仍是两级）。
func checkReassignTarget(src, target model.Industry, hasChildren bool) error {
	if target.ID == src.ID {
		return errParam("迁移目标不能是自己")
	}
	srcIsRoot := !src.ParentID.Valid
	tgtIsRoot := !target.ParentID.Valid
	sameLevel := srcIsRoot == tgtIsRoot
	isParent := src.ParentID.Valid && src.ParentID.Int64 == target.ID
	if !sameLevel && !isParent {
		return errParam("迁移目标必须是同级行业或其父级")
	}
	if hasChildren && !tgtIsRoot {
		return xerr.NewReason(xerr.ParamError, ReasonIndustryDepthExceeded, "有子级的行业只能迁移到一级行业")
	}
	return nil
}

// AdminDeleteIndustry 删除行业：无模板且无子级才可直接删；否则必须带 reassignTo，在同一事务内迁移关系行（含 sort_order）、
// 用户偏好与子级，再逻辑删除（code 改写为 <code>#del#<id> 以便复用）。没有任何受保护的行业行（种子行也可删，审计留痕）。
func (s *Service) AdminDeleteIndustry(ctx context.Context, op Operator, id, reassignTo int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	var prefUsers []int64 // 偏好指向被删行业的用户：提交后清他们的偏好缓存
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var src model.Industry
		if err := tx.NewSelect().Model(&src).Where("ti.id = ?", id).Where("ti.deleted = 0").For("UPDATE").Scan(ctx); err != nil {
			if noRows(err) {
				return xerr.NotFoundf("行业")
			}
			return err
		}
		var kids []model.Industry
		if err := tx.NewSelect().Model(&kids).Where("ti.parent_id = ?", id).Where("ti.deleted = 0").Scan(ctx); err != nil {
			return err
		}
		relCount, err := tx.NewSelect().Model((*model.TemplateIndustry)(nil)).Where("industry_id = ?", id).Count(ctx)
		if err != nil {
			return err
		}
		prefUsers = prefUsers[:0]
		if err := tx.NewSelect().Model((*model.UserIndustryPref)(nil)).Column("user_id").Where("industry_id = ?", id).Scan(ctx, &prefUsers); err != nil {
			return err
		}
		prefCount := len(prefUsers)
		if relCount > 0 || len(kids) > 0 {
			if reassignTo == 0 {
				return xerr.NewReason(xerr.Conflict, ReasonIndustryHasTemplates, "该行业下还有模板或子级，请选择迁移到哪个行业")
			}
		}
		if reassignTo != 0 {
			var target model.Industry
			if err := tx.NewSelect().Model(&target).Where("ti.id = ?", reassignTo).Where("ti.deleted = 0").Scan(ctx); err != nil {
				if noRows(err) {
					return errParam("迁移目标行业不存在")
				}
				return err
			}
			if err := checkReassignTarget(src, target, len(kids) > 0); err != nil {
				return err
			}
			if err := moveRelations(ctx, tx, id, reassignTo); err != nil {
				return err
			}
			if err := movePrefs(ctx, tx, id, reassignTo); err != nil {
				return err
			}
			if len(kids) > 0 { // 子级改挂到目标一级下
				if _, err := tx.NewUpdate().Model((*model.Industry)(nil)).Set("parent_id = ?", reassignTo).Where("parent_id = ?", id).Where("deleted = 0").Exec(ctx); err != nil {
					return err
				}
			}
		} else if prefCount > 0 { // 没有目标：偏好直接清掉（stale 的偏好行没有意义）
			if _, err := tx.NewDelete().Model((*model.UserIndustryPref)(nil)).Where("industry_id = ?", id).Exec(ctx); err != nil {
				return err
			}
		}
		suffix := fmt.Sprintf("#del#%d", id)
		code := src.Code
		if max := 64 - len(suffix); len(code) > max {
			code = code[:max]
		}
		if _, err := tx.NewUpdate().Model((*model.Industry)(nil)).
			Set("code = ?", code+suffix).Set("deleted = 1").Set("enabled = 0").Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		after := map[string]any{"deleted": true, "reassignTo": reassignTo, "movedTemplates": relCount, "movedChildren": len(kids)}
		return s.audit(ctx, tx, op, "INDUSTRY_DELETE", ResIndustry, fmt.Sprint(id), "", industrySnapshot(src), after)
	})
	if err != nil {
		return err
	}
	s.evictPrefs(ctx, prefUsers)
	s.Invalidate(ctx)
	return nil
}

// evictPrefs 删 / 迁移行业后清相关用户的偏好缓存（否则最长 prefTTL 内仍读到已删行业）。失败只记日志：缓存到期自愈。
func (s *Service) evictPrefs(ctx context.Context, userIDs []int64) {
	if s.rd == nil || len(userIDs) == 0 {
		return
	}
	const chunk = 500
	for i := 0; i < len(userIDs); i += chunk {
		end := min(i+chunk, len(userIDs))
		keys := make([]string, 0, end-i)
		for _, u := range userIDs[i:end] {
			keys = append(keys, prefKey(u))
		}
		if err := s.rd.Del(ctx, keys...).Err(); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 删除行业后清偏好缓存失败（%d 个用户，最迟 prefTTL 后自愈）: %v", len(keys), err)
		}
	}
}

// moveRelations 把 from 的关系行迁到 to（保留 sort_order）；模板已挂在 to 的，丢掉 from 的那行（去重）。
// 先在 Go 里取出 to 已有的模板 id 再删：MySQL 不允许 DELETE 的子查询引用被删的同一张表（1093）。
func moveRelations(ctx context.Context, tx bun.IDB, from, to int64) error {
	var have []int64
	if err := tx.NewSelect().Model((*model.TemplateIndustry)(nil)).Column("template_id").Where("industry_id = ?", to).Scan(ctx, &have); err != nil {
		return err
	}
	if len(have) > 0 {
		if _, err := tx.NewDelete().Model((*model.TemplateIndustry)(nil)).Where("industry_id = ?", from).Where("template_id IN (?)", bun.In(have)).Exec(ctx); err != nil {
			return err
		}
	}
	_, err := tx.NewUpdate().Model((*model.TemplateIndustry)(nil)).Set("industry_id = ?", to).Where("industry_id = ?", from).Exec(ctx)
	return err
}

func movePrefs(ctx context.Context, tx bun.IDB, from, to int64) error {
	var have []int64
	if err := tx.NewSelect().Model((*model.UserIndustryPref)(nil)).Column("user_id").Where("industry_id = ?", to).Scan(ctx, &have); err != nil {
		return err
	}
	if len(have) > 0 {
		if _, err := tx.NewDelete().Model((*model.UserIndustryPref)(nil)).Where("industry_id = ?", from).Where("user_id IN (?)", bun.In(have)).Exec(ctx); err != nil {
			return err
		}
	}
	_, err := tx.NewUpdate().Model((*model.UserIndustryPref)(nil)).Set("industry_id = ?", to).Where("industry_id = ?", from).Exec(ctx)
	return err
}

// ---------------------------------------------------------------- 行业树（后台）

// TemplateCount 行业下的模板计数。
type TemplateCount struct {
	Total     int `json:"total"`
	Published int `json:"published"`
	Image     int `json:"image"`
	Video     int `json:"video"`
}

// AdminIndustryNode 后台行业树节点（含停用 / 空行业）。
type AdminIndustryNode struct {
	ID            int64               `json:"id"`
	Code          string              `json:"code"`
	Name          string              `json:"name"`
	ParentID      *int64              `json:"parentId"`
	IconURL       *string             `json:"iconUrl"`
	CoverURL      *string             `json:"coverUrl"`
	Hint          *string             `json:"hint"`
	AudienceName  *string             `json:"audienceName"`
	Examples      []string            `json:"examples"`
	NextActions   []string            `json:"nextActions"`
	SortOrder     int                 `json:"sortOrder"`
	Enabled       bool                `json:"enabled"`
	ShowWhenEmpty bool                `json:"showWhenEmpty"`
	TemplateCount TemplateCount       `json:"templateCount"`
	UserCount     int                 `json:"userCount"`
	Replicate7d   int64               `json:"replicate7d"`
	UserVisible   bool                `json:"userVisible"`
	Children      []AdminIndustryNode `json:"children"`
}

// AdminIndustryTree 完整树：每个节点带模板计数、偏好人数、近 7 天做同款轮次、「用户端是否可见」。
func (s *Service) AdminIndustryTree(ctx context.Context) ([]AdminIndustryNode, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	var inds []model.Industry
	if err := s.db.NewSelect().Model(&inds).Where("ti.deleted = 0").Order("ti.sort_order ASC", "ti.id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	var links []linkRow
	if err := s.db.NewRaw("SELECT r.industry_id, r.template_id, t.media_type, t.state FROM afv_plaza_template_industry r "+
		"JOIN afv_compat_plaza_template t ON t.id = r.template_id WHERE t.deleted = 0").Scan(ctx, &links); err != nil {
		return nil, err
	}
	var prefs []struct {
		IndustryID int64 `bun:"industry_id"`
		N          int   `bun:"n"`
	}
	if err := s.db.NewRaw("SELECT industry_id, COUNT(DISTINCT user_id) AS n FROM afv_user_industry_pref GROUP BY industry_id").Scan(ctx, &prefs); err != nil {
		return nil, err
	}
	var usage map[int64]int64
	if s.rep != nil {
		usage, _ = s.rep.Uses7d(ctx)
	}
	userCount := map[int64]int{}
	for _, p := range prefs {
		userCount[p.IndustryID] = p.N
	}
	cat, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	return buildAdminTree(inds, links, userCount, usage, func(id int64) bool { return cat.shownInDirectory(id, now) }), nil
}

type linkRow struct {
	IndustryID int64  `bun:"industry_id"`
	TemplateID int64  `bun:"template_id"`
	MediaType  string `bun:"media_type"`
	State      string `bun:"state"`
}

// buildAdminTree 纯函数：行业 + 关系 → 树（便于单测）。visible 由调用方给（用目录快照判断「用户端是否可见」）。
func buildAdminTree(inds []model.Industry, links []linkRow, userCount map[int64]int, usage map[int64]int64, visible func(id int64) bool) []AdminIndustryNode {
	type stat struct {
		seen map[int64]bool
		cnt  TemplateCount
		uses int64
	}
	byInd := map[int64]*stat{}
	get := func(id int64) *stat {
		if byInd[id] == nil {
			byInd[id] = &stat{seen: map[int64]bool{}}
		}
		return byInd[id]
	}
	parentOf := map[int64]int64{}
	for _, in := range inds {
		parentOf[in.ID] = in.ParentID.Int64
	}
	add := func(id int64, l linkRow) {
		st := get(id)
		if st.seen[l.TemplateID] {
			return
		}
		st.seen[l.TemplateID] = true
		st.cnt.Total++
		if l.State == model.StatePublished {
			st.cnt.Published++
		}
		if l.MediaType == model.MediaVideo {
			st.cnt.Video++
		} else {
			st.cnt.Image++
		}
		st.uses += usage[l.TemplateID]
	}
	for _, l := range links {
		add(l.IndustryID, l)
		if p := parentOf[l.IndustryID]; p != 0 { // 一级含其二级的模板（去重）
			add(p, l)
		}
	}
	node := func(in model.Industry) AdminIndustryNode {
		st := get(in.ID)
		n := AdminIndustryNode{
			ID: in.ID, Code: in.Code, Name: in.Name, IconURL: strPtr(in.IconURL), CoverURL: strPtr(in.CoverURL),
			Hint: strPtr(in.Hint), AudienceName: strPtr(in.AudienceName),
			Examples: jsonStrings(in.ExamplesJSON), NextActions: jsonStrings(in.NextActionsJSON),
			SortOrder: in.SortOrder, Enabled: in.Enabled == 1, ShowWhenEmpty: in.ShowWhenEmpty == 1,
			TemplateCount: st.cnt, UserCount: userCount[in.ID], Replicate7d: st.uses, Children: []AdminIndustryNode{},
		}
		if in.ParentID.Valid {
			v := in.ParentID.Int64
			n.ParentID = &v
		}
		return n
	}
	var roots []AdminIndustryNode
	for _, in := range inds {
		if in.ParentID.Valid {
			continue
		}
		rn := node(in)
		for _, ch := range inds {
			if ch.ParentID.Valid && ch.ParentID.Int64 == in.ID {
				rn.Children = append(rn.Children, node(ch))
				rn.UserCount += userCount[ch.ID]
			}
		}
		roots = append(roots, rn)
	}
	// 用户端是否可见：交给目录快照判断，仅作展示（灰色「用户端不可见」徽标）
	if visible != nil {
		for i := range roots {
			roots[i].UserVisible = visible(roots[i].ID)
			for j := range roots[i].Children {
				roots[i].Children[j].UserVisible = visible(roots[i].Children[j].ID)
			}
		}
	}
	return roots
}
