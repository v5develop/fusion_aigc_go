package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/plaza/headline/algo"
	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// Meta 写操作的操作者信息（审计用）。
type Meta struct {
	OperatorID int64
	RequestID  string
	ClientIP   string
}

// WriteResult 写操作的响应：受影响的行 + 立即刷新的结果（刷新失败只告警，规则已落库，下个周期生效）。
type WriteResult struct {
	IDs       []int64 `json:"ids"`
	BatchID   string  `json:"batchId,omitempty"`
	Version   int64   `json:"version"`
	Refreshed bool    `json:"refreshed"`
}

func errScopeNotEditable() error {
	return xerr.NewReason(xerr.ParamError, ReasonScopeNotEditable, "全站总榜不接受人工规则，请选择行业")
}

func errNotInScope(scope string) error {
	return xerr.NewReason(xerr.ParamError, ReasonTemplateNotInScope, fmt.Sprintf("模板不属于行业 %s，不能在该行业的榜单上操作", scope))
}

func errSuppressed(scope string) error {
	return xerr.NewReason(xerr.Conflict, ReasonTemplateSuppressed, fmt.Sprintf("该模板在 %s 已被取消推荐或屏蔽，请先解除", scope))
}

func errDuplicate(scope string) error {
	return xerr.NewReason(xerr.Conflict, ReasonDuplicate, fmt.Sprintf("该模板在 %s 已有时间重叠的生效推荐", scope))
}

func errTemplateAbsent() error {
	return xerr.NewReason(xerr.NotFound, ReasonHeadlineTemplateAbsent, "模板不存在或对用户不可见")
}

func paramsInvalid(errs algo.FieldErrors) error {
	return xerr.NewReason(xerr.ParamError, ReasonParamsInvalid, "参数不合法："+errs.Error())
}

// resolveScopes 校验并规整范围：ALL 拒绝；* 原样保留（存一行，语义=所有包含该模板的范围，动态跟随模板的行业）；
// 显式范围必须是现存行业且包含该模板；alsoParent 给每个二级补上其一级。去重后按输入顺序返回。
func resolveScopes(cat *store.Catalog, tplID int64, scopes []string, alsoParent bool) ([]string, error) {
	scopes = dedupe(scopes)
	if len(scopes) == 0 {
		return nil, xerr.Param("请选择至少一个行业范围")
	}
	var out []string
	seen := map[string]bool{}
	push := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, sc := range scopes {
		switch sc {
		case algo.ScopeAll:
			return nil, errScopeNotEditable()
		case algo.ScopeAny:
			if len(cat.ScopesOf(tplID)) == 0 {
				return nil, errNotInScope("（模板还没有归入任何行业）")
			}
			push(sc)
			continue
		}
		s, ok := cat.Scope(sc)
		if !ok {
			return nil, errIndustryNotFound()
		}
		if !s.HasMember(tplID) {
			return nil, errNotInScope(s.Name)
		}
		push(sc)
		if alsoParent && s.ParentCode != "" {
			if p, ok := cat.Scope(s.ParentCode); ok && p.HasMember(tplID) {
				push(p.Code)
			}
		}
	}
	return out, nil
}

// suppressedIn 模板在 scope 是否有生效的抑制（范围 ∈ {scope, *}；scope=* 时任一抑制都算冲突）。
func suppressedIn(blocks []store.Block, tplID int64, scope string, now time.Time) bool {
	for _, b := range blocks {
		if b.PlazaTemplateID != tplID || !b.Algo().ActiveAt(now) {
			continue
		}
		if scope == algo.ScopeAny || b.ScopeCode == scope || b.ScopeCode == algo.ScopeAny {
			return true
		}
	}
	return false
}

func parseDate(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", strings.TrimSpace(s), algo.CST)
}

func dateWindow(start, end string, now time.Time) (time.Time, time.Time, error) {
	sd := time.Date(now.In(algo.CST).Year(), now.In(algo.CST).Month(), now.In(algo.CST).Day(), 0, 0, 0, 0, algo.CST)
	ed := sd.AddDate(0, 0, 6) // 缺省今天起 7 天（含首尾）
	var err error
	if start != "" {
		if sd, err = parseDate(start); err != nil {
			return sd, ed, xerr.Param("startDate 格式应为 yyyy-MM-dd")
		}
		if end == "" {
			ed = sd.AddDate(0, 0, 6)
		}
	}
	if end != "" {
		if ed, err = parseDate(end); err != nil {
			return sd, ed, xerr.Param("endDate 格式应为 yyyy-MM-dd")
		}
	}
	if ed.Before(sd) {
		return sd, ed, xerr.Param("结束日期不能早于开始日期")
	}
	return sd, ed, nil
}

func checkWhy(why string) (string, error) {
	why = strings.TrimSpace(why)
	if why == "" {
		return "", xerr.Param("推荐语不能为空")
	}
	if len([]rune(why)) > 60 {
		return "", xerr.Param("推荐语最多 60 字")
	}
	return why, nil
}

func checkPin(pin *int) error {
	if pin != nil && (*pin < 1 || *pin > 50) {
		return xerr.Param("名次须在 1~50 之间")
	}
	return nil
}

// overlaps 同模板同范围、启用、未删除、日期窗口重叠的规则（排除 exceptIDs）。
func overlaps(rules []store.HeadlineRule, tplID int64, scope string, sd, ed time.Time, except map[int64]bool) bool {
	for _, r := range rules {
		if except[r.ID] || r.PlazaTemplateID != tplID || r.ScopeCode != scope || r.Enabled != 1 || r.Deleted != 0 {
			continue
		}
		if !r.StartDate.After(ed) && !r.EndDate.Before(sd) {
			return true
		}
	}
	return false
}

// afterWrite 写成功后：清本进程缓存，同步触发 RefreshNow(OPS)；刷新失败只告警。
func (s *Service) afterWrite(ctx context.Context, res *WriteResult, operator int64) *WriteResult {
	s.invalidateRules()
	out, err := s.RefreshNow(ctx, TriggerOps, operator)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza headline: 写后刷新失败（规则已落库，下个周期生效）: %v", err)
		return res
	}
	res.Version, res.Refreshed = out.Version, out.Refreshed
	return res
}

func (s *Service) audit(ctx context.Context, tx bun.IDB, m Meta, action, rtype, rid, batch string, before, after any) error {
	missing, err := s.repo.Audit(ctx, tx, store.AuditEntry{OperatorID: m.OperatorID, Action: action, ResourceType: rtype, ResourceID: rid,
		BatchID: batch, Before: before, After: after, RequestID: m.RequestID, ClientIP: m.ClientIP})
	if missing {
		logx.WithContext(ctx).Errorf("plaza headline: 审计表 afv_plaza_audit_log 不存在（Go_V1.0.33 未执行），本次 %s 未留痕", action)
	}
	return err
}

// CreateRuleReq 手动推荐。
type CreateRuleReq struct {
	PlazaTemplateID int64    `json:"plazaTemplateId"`
	Scopes          []string `json:"scopes"`
	Why             string   `json:"why"`
	PinPosition     *int     `json:"pinPosition,optional"`
	StartDate       string   `json:"startDate,optional"`
	EndDate         string   `json:"endDate,optional"`
	Enabled         *bool    `json:"enabled,optional"`
	SortOrder       int      `json:"sortOrder,optional"`
}

// CreateRule 手动推荐：每个范围写一行、共用 batch_id；缺省窗口今天起 7 天。
// 校验：模板可见、范围包含该模板（ALL 拒绝）、各范围不在抑制名单、同模板同范围窗口重叠的生效规则不重复。
func (s *Service) CreateRule(ctx context.Context, m Meta, req CreateRuleReq) (*WriteResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if t, ok := cat.Templates[req.PlazaTemplateID]; !ok || !t.VisibleAt(now) {
		return nil, errTemplateAbsent()
	}
	why, err := checkWhy(req.Why)
	if err != nil {
		return nil, err
	}
	if err := checkPin(req.PinPosition); err != nil {
		return nil, err
	}
	sd, ed, err := dateWindow(req.StartDate, req.EndDate, now)
	if err != nil {
		return nil, err
	}
	scopes, err := resolveScopes(cat, req.PlazaTemplateID, req.Scopes, false)
	if err != nil {
		return nil, err
	}
	blocks, err := s.repo.ListBlocks(ctx, "", req.PlazaTemplateID, true, now)
	if err != nil {
		return nil, err
	}
	existing, err := s.repo.RulesOfTemplate(ctx, req.PlazaTemplateID)
	if err != nil {
		return nil, err
	}
	enabled := req.Enabled == nil || *req.Enabled
	for _, sc := range scopes {
		if suppressedIn(blocks, req.PlazaTemplateID, sc, now) {
			return nil, errSuppressed(scopeName(cat, sc))
		}
		if enabled && overlaps(existing, req.PlazaTemplateID, sc, sd, ed, nil) {
			return nil, errDuplicate(scopeName(cat, sc))
		}
	}
	return s.insertRules(ctx, m, req.PlazaTemplateID, scopes, why, req.PinPosition, sd, ed, enabled, req.SortOrder, false)
}

func (s *Service) insertRules(ctx context.Context, m Meta, tplID int64, scopes []string, why string, pin *int, sd, ed time.Time, enabled bool, sortOrder int, adopted bool) (*WriteResult, error) {
	var batch *string
	res := &WriteResult{}
	if len(scopes) > 1 {
		b := newBatchID()
		batch, res.BatchID = &b, b
	}
	en, ad := 0, 0
	if enabled {
		en = 1
	}
	if adopted {
		ad = 1
	}
	op := nullable(m.OperatorID)
	err := s.repo.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, sc := range scopes {
			row := &store.HeadlineRule{PlazaTemplateID: tplID, ScopeCode: sc, BatchID: batch, StartDate: store.CSTTime{Time: sd}, EndDate: store.CSTTime{Time: ed},
				Why: why, SortOrder: sortOrder, Enabled: en, PinPosition: pin, AdoptedFromAuto: ad, CreatedBy: op, UpdatedBy: op}
			if err := s.repo.InsertRule(ctx, tx, row); err != nil {
				return err
			}
			res.IDs = append(res.IDs, row.ID)
			if err := s.audit(ctx, tx, m, "HEADLINE_CREATE", "HEADLINE_RULE", fmt.Sprint(row.ID), res.BatchID, nil, ruleSnapshot(row)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.afterWrite(ctx, res, m.OperatorID), nil
}

// ruleSnapshot 审计用的规则快照（日期按 yyyy-MM-dd）。
func ruleSnapshot(r *store.HeadlineRule) map[string]any {
	return map[string]any{"id": r.ID, "plazaTemplateId": r.PlazaTemplateID, "scope": r.ScopeCode, "batchId": r.BatchID,
		"startDate": fmtDate(r.StartDate.Time), "endDate": fmtDate(r.EndDate.Time), "why": r.Why, "sortOrder": r.SortOrder,
		"enabled": r.Enabled, "pinPosition": r.PinPosition, "adoptedFromAuto": r.AdoptedFromAuto}
}

// UpdateRuleReq 整体 PUT：startDate / endDate 必填（缺 → 400 PARAM_MISSING）。 编辑人工规则（整体替换可编辑字段）。
type UpdateRuleReq struct {
	Why          string `json:"why"`
	PinPosition  *int   `json:"pinPosition,optional"`
	StartDate    string `json:"startDate"`
	EndDate      string `json:"endDate"`
	Enabled      *bool  `json:"enabled,optional"`
	SortOrder    int    `json:"sortOrder,optional"`
	ApplyToBatch bool   `json:"applyToBatch,optional"`
}

// UpdateRule 编辑：bun 定向列更新；applyToBatch 时同一 batch_id 的各范围行一起改。
func (s *Service) UpdateRule(ctx context.Context, m Meta, id int64, req UpdateRuleReq) (*WriteResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	why, err := checkWhy(req.Why)
	if err != nil {
		return nil, err
	}
	if err := checkPin(req.PinPosition); err != nil {
		return nil, err
	}
	// 整体 PUT：起止日期必填。缺省时 dateWindow 会补「今天起 7 天」，编辑时漏传就会把运营设好的窗口悄悄改掉
	if strings.TrimSpace(req.StartDate) == "" || strings.TrimSpace(req.EndDate) == "" {
		logx.WithContext(ctx).Infof("plaza headline: 规则编辑拒收 判据=startDate/endDate 缺失（整体 PUT 必填） 规则=%d startDate=%q endDate=%q",
			id, req.StartDate, req.EndDate)
		return nil, xerr.New(xerr.ParamMissing, "startDate 与 endDate 必填（yyyy-MM-dd）")
	}
	now := s.now()
	sd, ed, err := dateWindow(req.StartDate, req.EndDate, now)
	if err != nil {
		return nil, err
	}
	enabled := 1
	if req.Enabled != nil && !*req.Enabled {
		enabled = 0
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	res := &WriteResult{}
	err = s.repo.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		targets, err := s.targets(ctx, tx, id, req.ApplyToBatch)
		if err != nil {
			return err
		}
		except := map[int64]bool{}
		for _, r := range targets {
			except[r.ID] = true
		}
		existing, err := s.repo.RulesOfTemplate(ctx, targets[0].PlazaTemplateID)
		if err != nil {
			return err
		}
		blocks, err := s.repo.ListBlocks(ctx, "", targets[0].PlazaTemplateID, true, now)
		if err != nil {
			return err
		}
		for _, r := range targets {
			if enabled == 1 && overlaps(existing, r.PlazaTemplateID, r.ScopeCode, sd, ed, except) {
				return errDuplicate(scopeName(cat, r.ScopeCode))
			}
			if enabled == 1 && suppressedIn(blocks, r.PlazaTemplateID, r.ScopeCode, now) {
				return errSuppressed(scopeName(cat, r.ScopeCode))
			}
			before := ruleSnapshot(&r)
			set := map[string]any{"why": why, "start_date": fmtDate(sd), "end_date": fmtDate(ed), "enabled": enabled,
				"sort_order": req.SortOrder, "pin_position": req.PinPosition, "updated_by": nullable(m.OperatorID)}
			if err := s.repo.UpdateRule(ctx, tx, r.ID, set); err != nil {
				return err
			}
			after := r
			after.Why, after.StartDate, after.EndDate, after.Enabled, after.SortOrder, after.PinPosition = why, store.CSTTime{Time: sd}, store.CSTTime{Time: ed}, enabled, req.SortOrder, req.PinPosition
			if err := s.audit(ctx, tx, m, "HEADLINE_UPDATE", "HEADLINE_RULE", fmt.Sprint(r.ID), deref(r.BatchID), before, ruleSnapshot(&after)); err != nil {
				return err
			}
			res.IDs = append(res.IDs, r.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.afterWrite(ctx, res, m.OperatorID), nil
}

// targets 单条或同批次的规则（未删除）。
func (s *Service) targets(ctx context.Context, tx bun.IDB, id int64, batch bool) ([]store.HeadlineRule, error) {
	r, err := s.repo.GetRule(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, xerr.NotFoundf("头条规则")
	}
	if !batch || r.BatchID == nil || *r.BatchID == "" {
		return []store.HeadlineRule{*r}, nil
	}
	return s.repo.RulesOfBatch(ctx, tx, *r.BatchID)
}

// DeleteRule 删除（逻辑删除）；applyToBatch 时同批一起删。删除 adopted_from_auto=1 的规则 = 取消接管。
func (s *Service) DeleteRule(ctx context.Context, m Meta, id int64, applyToBatch bool) (*WriteResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	res := &WriteResult{}
	err := s.repo.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		targets, err := s.targets(ctx, tx, id, applyToBatch)
		if err != nil {
			return err
		}
		for _, r := range targets {
			if err := s.repo.DeleteRule(ctx, tx, r.ID, m.OperatorID); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, m, "HEADLINE_DELETE", "HEADLINE_RULE", fmt.Sprint(r.ID), deref(r.BatchID), ruleSnapshot(&r), nil); err != nil {
				return err
			}
			res.IDs = append(res.IDs, r.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.afterWrite(ctx, res, m.OperatorID), nil
}

// adoptOrUpdate 在 scope 上找该模板生效的显式规则；没有就「接管」：新建 adopted_from_auto=1 的规则
// （推荐语沿用当前榜单上的文案，窗口今天起 7 天），然后对它做 mutate 的定向更新。
func (s *Service) adoptOrUpdate(ctx context.Context, m Meta, tplID int64, scope string, mutate func(r *store.HeadlineRule) map[string]any) (*WriteResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if scope == "" || scope == algo.ScopeAny {
		return nil, xerr.Param("定位 / 改推荐语需要指定一个行业")
	}
	if _, err := resolveScopes(cat, tplID, []string{scope}, false); err != nil {
		return nil, err
	}
	if t, ok := cat.Templates[tplID]; !ok || !t.VisibleAt(now) {
		return nil, errTemplateAbsent()
	}
	blocks, err := s.repo.ListBlocks(ctx, "", tplID, true, now)
	if err != nil {
		return nil, err
	}
	if suppressedIn(blocks, tplID, scope, now) {
		return nil, errSuppressed(scopeName(cat, scope))
	}
	rules, err := s.repo.RulesOfTemplate(ctx, tplID)
	if err != nil {
		return nil, err
	}
	var target *store.HeadlineRule
	for i := range rules {
		r := rules[i]
		if r.ScopeCode == scope && r.Algo().ActiveOn(now) {
			if target == nil || r.ID > target.ID {
				target = &r
			}
		}
	}
	res := &WriteResult{}
	err = s.repo.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if target == nil {
			sd := time.Date(now.In(algo.CST).Year(), now.In(algo.CST).Month(), now.In(algo.CST).Day(), 0, 0, 0, 0, algo.CST)
			why := s.currentWhy(ctx, scope, tplID)
			op := nullable(m.OperatorID)
			row := &store.HeadlineRule{PlazaTemplateID: tplID, ScopeCode: scope, StartDate: store.CSTTime{Time: sd}, EndDate: store.CSTTime{Time: sd.AddDate(0, 0, 6)},
				Why: why, Enabled: 1, AdoptedFromAuto: 1, CreatedBy: op, UpdatedBy: op}
			if err := s.repo.InsertRule(ctx, tx, row); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, m, "HEADLINE_ADOPT", "HEADLINE_RULE", fmt.Sprint(row.ID), "", nil, ruleSnapshot(row)); err != nil {
				return err
			}
			target = row
		}
		before := ruleSnapshot(target)
		set := mutate(target)
		set["updated_by"] = nullable(m.OperatorID)
		if err := s.repo.UpdateRule(ctx, tx, target.ID, set); err != nil {
			return err
		}
		res.IDs = []int64{target.ID}
		return s.audit(ctx, tx, m, "HEADLINE_UPDATE", "HEADLINE_RULE", fmt.Sprint(target.ID), deref(target.BatchID), before, ruleSnapshot(target))
	})
	if err != nil {
		return nil, err
	}
	return s.afterWrite(ctx, res, m.OperatorID), nil
}

// currentWhy 该模板在该范围最新榜单上的推荐语；没有则按算法文案生成（≤60 字）。
func (s *Service) currentWhy(ctx context.Context, scope string, tplID int64) string {
	if snap, err := s.repo.LatestSnapshot(ctx); err == nil && snap != nil {
		if items, err := s.repo.ItemsOfTemplate(ctx, snap.ID, tplID); err == nil {
			for _, it := range items {
				if it.ScopeCode == scope && it.Why != "" {
					return it.Why
				}
			}
		}
	}
	return "运营推荐"
}

// Position 在该范围定位 / 取消定位（position=nil）；模板当前是算法项则自动接管。拖拽只提交被拖的那一项。
func (s *Service) Position(ctx context.Context, m Meta, tplID int64, scope string, position *int) (*WriteResult, error) {
	if err := checkPin(position); err != nil {
		return nil, err
	}
	return s.adoptOrUpdate(ctx, m, tplID, scope, func(r *store.HeadlineRule) map[string]any {
		r.PinPosition = position
		return map[string]any{"pin_position": position}
	})
}

// SetWhy 改推荐语（算法项自动接管）。
func (s *Service) SetWhy(ctx context.Context, m Meta, tplID int64, scope, why string) (*WriteResult, error) {
	why, err := checkWhy(why)
	if err != nil {
		return nil, err
	}
	return s.adoptOrUpdate(ctx, m, tplID, scope, func(r *store.HeadlineRule) map[string]any {
		r.Why = why
		return map[string]any{"why": why}
	})
}

// SuppressReq 取消推荐 / 屏蔽。
type SuppressReq struct {
	PlazaTemplateID int64    `json:"plazaTemplateId"`
	Scopes          []string `json:"scopes"`
	// Duration 时长快捷项：24h | sunday | custom | permanent（优先）；不传时看 Kind + UntilAt
	Duration   string `json:"duration,optional"`
	CustomDate string `json:"customDate,optional"` // duration=custom：yyyy-MM-dd，当日 24:00 结束
	Kind       string `json:"kind,optional"`       // CANCEL | BLOCK
	UntilAt    string `json:"untilAt,optional"`    // kind=CANCEL：yyyy-MM-ddTHH:mm:ss（CST），须晚于现在
	Reason     string `json:"reason"`
	AlsoParent bool   `json:"alsoParent,optional"` // 同时作用于上级行业（实际多写一行）
}

// Suppress 取消到某时刻 / 永久屏蔽：每个范围一行（再次操作覆盖），同一次提交共用 batch_id。
func (s *Service) Suppress(ctx context.Context, m Meta, req SuppressReq) (*WriteResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if _, ok := cat.Templates[req.PlazaTemplateID]; !ok {
		return nil, errTemplateAbsent()
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return nil, xerr.Param("请填写原因")
	}
	if len([]rune(reason)) > 200 {
		return nil, xerr.Param("原因最多 200 字")
	}
	kind, until, err := suppressWindow(req, now)
	if err != nil {
		return nil, err
	}
	scopes, err := resolveScopes(cat, req.PlazaTemplateID, req.Scopes, req.AlsoParent)
	if err != nil {
		return nil, err
	}
	res := &WriteResult{}
	var batch *string
	if len(scopes) > 1 {
		b := newBatchID()
		batch, res.BatchID = &b, b
	}
	op := nullable(m.OperatorID)
	err = s.repo.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, sc := range scopes {
			old, err := s.repo.BlockOf(ctx, tx, req.PlazaTemplateID, sc)
			if err != nil {
				return err
			}
			row := &store.Block{PlazaTemplateID: req.PlazaTemplateID, ScopeCode: sc, Kind: kind, Reason: reason, BatchID: batch, CreatedBy: op}
			if until != nil {
				row.UntilAt = store.CSTTime{Time: *until}
			}
			if err := s.repo.UpsertBlock(ctx, tx, row); err != nil {
				return err
			}
			cur, err := s.repo.BlockOf(ctx, tx, req.PlazaTemplateID, sc)
			if err != nil {
				return err
			}
			var before any
			if old != nil {
				before = blockSnapshot(old)
			}
			if cur != nil {
				res.IDs = append(res.IDs, cur.ID)
				if err := s.audit(ctx, tx, m, "HEADLINE_SUPPRESS", "HEADLINE_BLOCK", fmt.Sprint(cur.ID), res.BatchID, before, blockSnapshot(cur)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.afterWrite(ctx, res, m.OperatorID), nil
}

func suppressWindow(req SuppressReq, now time.Time) (string, *time.Time, error) {
	if req.Duration != "" {
		var custom time.Time
		if req.Duration == algo.DurationCustom {
			d, err := parseDate(req.CustomDate)
			if err != nil {
				return "", nil, xerr.Param("customDate 格式应为 yyyy-MM-dd")
			}
			custom = d
		}
		kind, until, err := algo.SuppressUntil(req.Duration, custom, now)
		if err != nil {
			return "", nil, xerr.Param(err.Error())
		}
		return kind, until, nil
	}
	switch strings.ToUpper(req.Kind) {
	case algo.BlockBlock:
		return algo.BlockBlock, nil, nil
	case algo.BlockCancel:
		t, err := time.ParseInLocation("2006-01-02T15:04:05", strings.TrimSpace(req.UntilAt), algo.CST)
		if err != nil {
			if t, err = time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(req.UntilAt), algo.CST); err != nil {
				return "", nil, xerr.Param("untilAt 格式应为 yyyy-MM-ddTHH:mm:ss")
			}
		}
		if !t.After(now) {
			return "", nil, xerr.Param(algo.ErrUntilNotFuture.Error())
		}
		return algo.BlockCancel, &t, nil
	}
	return "", nil, xerr.Param("请选择取消时长（24h / sunday / custom / permanent）")
}

func blockSnapshot(b *store.Block) map[string]any {
	out := map[string]any{"id": b.ID, "plazaTemplateId": b.PlazaTemplateID, "scope": b.ScopeCode, "kind": b.Kind, "reason": b.Reason, "batchId": b.BatchID, "untilAt": nil}
	if !b.UntilAt.IsZero() {
		out["untilAt"] = fmtTime(b.UntilAt.Time)
	}
	return out
}

// Unsuppress 解除（物理删除 + 审计）；applyToBatch 时同批一起解除。解除后重新参与评选，不保证立刻入榜。
func (s *Service) Unsuppress(ctx context.Context, m Meta, id int64, applyToBatch bool) (*WriteResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	res := &WriteResult{}
	err := s.repo.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		b, err := s.repo.GetBlock(ctx, tx, id)
		if err != nil {
			return err
		}
		if b == nil {
			return xerr.NotFoundf("抑制规则")
		}
		targets := []store.Block{*b}
		if applyToBatch && b.BatchID != nil && *b.BatchID != "" {
			if targets, err = s.repo.BlocksOfBatch(ctx, tx, *b.BatchID); err != nil {
				return err
			}
		}
		for _, t := range targets {
			if err := s.repo.DeleteBlock(ctx, tx, t.ID); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, m, "HEADLINE_UNSUPPRESS", "HEADLINE_BLOCK", fmt.Sprint(t.ID), deref(t.BatchID), blockSnapshot(&t), nil); err != nil {
				return err
			}
			res.IDs = append(res.IDs, t.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.afterWrite(ctx, res, m.OperatorID), nil
}

// ParamMeta 一个参数的说明（管理端表单渲染：默认值、范围、是否可行业覆盖）。
type ParamMeta struct {
	Key      string  `json:"key"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Override bool    `json:"industryOverride"`
	Note     string  `json:"note"`
}

// ParamsView GET /params。
type ParamsView struct {
	Defaults  algo.Params  `json:"defaults"`
	Current   algo.Config  `json:"current"`
	Version   int          `json:"version"`
	Hash      string       `json:"hash"`
	Meta      []ParamMeta  `json:"meta"`
	Industry  []string     `json:"industries"`
	Effective *algo.Params `json:"effective,omitempty"`
}

var paramMeta = []ParamMeta{
	{"top-n", 3, 30, true, "每个行业榜的算法 + 补位名额"},
	{"max-visible", 3, 50, false, "榜单最多条数（含人工），≥ top-n"},
	{"half-life-hours", 1, 168, false, "时间衰减半衰期（小时）"},
	{"velocity.min-base", 1, 100, false, "增速的最小基数（次/天）"},
	{"velocity.min", -1, 0, false, "增速下限"},
	{"velocity.max", 0, 10, false, "增速上限"},
	{"fresh-days", 0, 60, false, "新鲜度加成天数"},
	{"weight.decay", 0, 5, false, "权重：衰减热度"},
	{"weight.velocity", 0, 5, false, "权重：24h 增速"},
	{"weight.fresh", 0, 5, false, "权重：新鲜度"},
	{"weight.users", 0, 5, false, "权重：去重用户数"},
	{"weight.ctr", 0, 5, false, "权重：点击率（预留，曝光管道落地前为 0）"},
	{"min-decayed", 0, 1000, true, "入选门槛：衰减热度"},
	{"min-unique-users", 1, 1000, true, "入选门槛：去重用户数"},
	{"rel-min-ratio", 0, 1, false, "相对阈值系数（小行业按自身量级筛选）"},
	{"per-user-daily-cap", 1, 20, false, "同用户同模板每天最多计数（改后历史在下次小时校准重算）"},
	{"rapid-dedupe-seconds", 0, 3600, false, "快速重复点击去重秒数（同上）"},
	{"max-per-media", 1, 30, false, "每种媒体类型最多几个，≤ top-n"},
	{"hysteresis-delta", 0, 0.5, false, "迟滞：在榜项分数打折门槛"},
	{"min-dwell-minutes", 0, 1440, false, "上榜最短驻留（分钟）"},
	{"max-rank-move", 1, 30, false, "每次刷新名次最多变化"},
	{"cold-start-fill", 0, 1, false, "冷启动补位开关"},
	{"snapshot-retention-days", 1, 90, false, "历史快照保留天数"},
	{"score-normalize", 0, 0, false, "none / scope_max（按范围归一化衰减项）"},
	{"norm-scale", 0, 100, false, "归一化系数"},
	{"global-top-n", 3, 100, false, "全站总榜条数（仅后台）"},
	{"display-min-uses", 0, 1000, false, "近 7 天少于它不展示具体数字"},
}

// Params 参数读取（默认值、范围、当前生效值、各行业覆盖）。
func (s *Service) Params(ctx context.Context, scope string) (*ParamsView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cfg, ctl, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(cat.Scopes))
	for _, sc := range cat.Scopes {
		codes = append(codes, sc.Code)
	}
	sort.Strings(codes)
	v := &ParamsView{Defaults: s.conf.Defaults(), Current: cfg, Version: ctl.ParamsVersion, Hash: cfg.Hash(), Meta: paramMeta, Industry: codes}
	if v.Current.Overrides == nil {
		v.Current.Overrides = map[string]algo.ScopeOverride{}
	}
	if scope != "" {
		p := cfg.For(scope)
		v.Effective = &p
	}
	return v, nil
}

// SaveParamsReq PUT /params。
type SaveParamsReq struct {
	Config  algo.Config `json:"config"`
	Version int         `json:"version"` // GET /params 返回的 version，防并发覆盖
}

// SaveParams 逐项范围 + 跨字段校验（失败 HEADLINE_PARAMS_INVALID 指出字段）；成功写控制行（CAS 版本）、审计改前后、触发刷新。
func (s *Service) SaveParams(ctx context.Context, m Meta, req SaveParamsReq) (*WriteResult, algo.FieldErrors, error) {
	if err := s.ready(); err != nil {
		return nil, nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, nil, err
	}
	if errs := req.Config.Validate(cat.ScopeCodes()); len(errs) > 0 {
		return nil, errs, paramsInvalid(errs)
	}
	before, _, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	raw, err := req.Config.CanonicalJSON()
	if err != nil {
		return nil, nil, err
	}
	err = s.repo.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		ok, err := s.repo.SaveParams(ctx, tx, raw, req.Version, m.OperatorID)
		if err != nil {
			return err
		}
		if !ok {
			return xerr.NewReason(xerr.Conflict, ReasonParamsConflict, "参数已被他人修改，请刷新后再改")
		}
		var after any
		_ = json.Unmarshal(raw, &after)
		return s.audit(ctx, tx, m, "PARAMS_UPDATE", "HEADLINE_PARAMS", "1", "", before, after)
	})
	if err != nil {
		return nil, nil, err
	}
	return s.afterWrite(ctx, &WriteResult{IDs: []int64{1}}, m.OperatorID), nil, nil
}

// ListRules 规则列表（同 batch_id 在前端合并展示）。
func (s *Service) ListRules(ctx context.Context, f store.RuleFilter) ([]RuleView, int, error) {
	if err := s.ready(); err != nil {
		return nil, 0, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, 0, err
	}
	now := s.now()
	rows, total, err := s.repo.ListRules(ctx, f, now)
	if err != nil {
		return nil, 0, err
	}
	out := make([]RuleView, 0, len(rows))
	for _, r := range rows {
		out = append(out, ruleView(r, cat, now))
	}
	return out, total, nil
}

// ListSuppressions 抑制列表（可按范围筛、只看生效中的）。
func (s *Service) ListSuppressions(ctx context.Context, scope string, activeOnly bool) ([]BlockView, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListBlocks(ctx, scope, 0, activeOnly, s.now())
	if err != nil {
		return nil, err
	}
	out := make([]BlockView, 0, len(rows))
	for _, b := range rows {
		out = append(out, blockView(b, cat))
	}
	return out, nil
}

// SnapshotList 版本列表。
func (s *Service) SnapshotList(ctx context.Context, page, size int) ([]SnapshotMeta, int, error) {
	if err := s.ready(); err != nil {
		return nil, 0, err
	}
	rows, total, err := s.repo.ListSnapshots(ctx, page, size)
	if err != nil {
		return nil, 0, err
	}
	out := make([]SnapshotMeta, len(rows))
	for i := range rows {
		out[i] = s.meta(&rows[i], false)
	}
	return out, total, nil
}

func nullable(id int64) *int64 {
	if id <= 0 {
		return nil
	}
	return &id
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// newBatchID 同一次「多范围」操作共用的批次号（UUID v4 文本，36 字符）。
func newBatchID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
