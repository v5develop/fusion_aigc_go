package core

// 营销节点后台（/plaza/admin/nodes）：节点本身的增删改启停，以及节点与模板关联的增删与排序（按行业）。
// afv_plaza_node / afv_plaza_node_template 是 Go 独占表，交接期（AdminWriteEnabled=false）也可写；
// 不碰 afv_compat_plaza_template，因此不受 requireWrite 约束。
// 节点的「行业」不另存：模板属于哪个行业看模板自己的行业关系（afv_plaza_template_industry），
// 后台按行业管理 = 只列出 / 添加 / 排序 属于该行业（含二级）的模板，用户端取数口径见 nodes.go。

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
)

const (
	ResNode = "NODE"

	ReasonNodeCodeDuplicate = "NODE_CODE_DUPLICATE" // 4002
	ReasonNodeNotFound      = "NODE_NOT_FOUND"      // 4001

	maxNodeName       = 40
	maxNodeWhy        = 60
	defaultLeadDays   = 14
	maxLeadDays       = 365
	maxNodeLinks      = 300 // 一个节点最多挂的模板数
	maxLinkBatch      = 50  // 一次最多添加几个
	nodeDeletedSuffix = "~del"
)

var nodeCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{1,63}$`)

// NodeInput 新建 / 编辑节点入参。指针字段 nil = 不改（新建时取缺省）；code 只能新建时给。
type NodeInput struct {
	Code      string  `json:"code"`
	Name      *string `json:"name"`
	Why       *string `json:"why"`
	EventDate *string `json:"eventDate"` // yyyy-MM-dd
	LeadDays  *int    `json:"leadDays"`
	SortOrder *int    `json:"sortOrder"`
	Enabled   *bool   `json:"enabled"`
}

// AdminNode 后台节点行。
type AdminNode struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	Why       string `json:"why"`
	EventDate string `json:"eventDate"`
	LeadDays  int    `json:"leadDays"`
	ShowFrom  string `json:"showFrom"` // 展示起始日 = eventDate - leadDays
	SortOrder int    `json:"sortOrder"`
	Enabled   bool   `json:"enabled"`
	// Status UPCOMING 还没到展示窗口 / ACTIVE 窗口内 / ENDED 已过节点当天。与启停无关。
	Status        string `json:"status"`
	TemplateCount int    `json:"templateCount"`
	CreateTime    string `json:"createTime"`
	UpdateTime    string `json:"updateTime"`
}

// AdminNodePage 节点分页。
type AdminNodePage struct {
	Total int         `json:"total"`
	Page  int         `json:"page"`
	Size  int         `json:"size"`
	Items []AdminNode `json:"items"`
}

type adminNodeRow struct {
	ID         int64  `bun:"id"`
	Code       string `bun:"code"`
	Name       string `bun:"name"`
	Why        string `bun:"why"`
	EventDate  string `bun:"event_date"`
	LeadDays   int    `bun:"lead_days"`
	SortOrder  int    `bun:"sort_order"`
	Enabled    int8   `bun:"enabled"`
	TplCount   int    `bun:"tpl_count"`
	CreateTime string `bun:"create_time"`
	UpdateTime string `bun:"update_time"`
}

const nodeSelect = "SELECT n.id, n.code, n.name, n.why, DATE_FORMAT(n.event_date, '%Y-%m-%d') AS event_date, n.lead_days, n.sort_order, n.enabled, " +
	"(SELECT COUNT(*) FROM afv_plaza_node_template l WHERE l.node_id = n.id) AS tpl_count, " +
	"DATE_FORMAT(n.create_time, '%Y-%m-%d %H:%i:%s') AS create_time, DATE_FORMAT(n.update_time, '%Y-%m-%d %H:%i:%s') AS update_time FROM afv_plaza_node n "

func (s *Service) nodeView(r adminNodeRow) AdminNode {
	n := AdminNode{ID: r.ID, Code: r.Code, Name: r.Name, Why: r.Why, EventDate: r.EventDate, LeadDays: r.LeadDays,
		SortOrder: r.SortOrder, Enabled: r.Enabled == 1, TemplateCount: r.TplCount, CreateTime: r.CreateTime, UpdateTime: r.UpdateTime}
	if ev, err := time.ParseInLocation("2006-01-02", r.EventDate, cst); err == nil {
		n.ShowFrom = ev.AddDate(0, 0, -r.LeadDays).Format("2006-01-02")
		today, _ := time.ParseInLocation("2006-01-02", s.now().In(cst).Format("2006-01-02"), cst)
		switch {
		case today.After(ev):
			n.Status = "ENDED"
		case today.Before(ev.AddDate(0, 0, -r.LeadDays)):
			n.Status = "UPCOMING"
		default:
			n.Status = "ACTIVE"
		}
	}
	return n
}

func nodeSnapshot(r adminNodeRow) map[string]any {
	return map[string]any{"id": r.ID, "code": r.Code, "name": r.Name, "why": r.Why, "eventDate": r.EventDate,
		"leadDays": r.LeadDays, "sortOrder": r.SortOrder, "enabled": r.Enabled == 1}
}

// AdminListNodes 节点列表：按节点日期倒序（最近 / 将来的在前）。enabled 空 = 全部，"1" / "0" 过滤。
func (s *Service) AdminListNodes(ctx context.Context, keyword, enabled string, page, size int) (*AdminNodePage, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	page, size = normPage(page, size, 20)
	where, args := "WHERE n.deleted = 0", []any{}
	if kw := strings.TrimSpace(keyword); kw != "" {
		where += " AND (n.name LIKE ? OR n.code LIKE ?)"
		like := "%" + escapeLike(kw) + "%"
		args = append(args, like, like)
	}
	switch enabled {
	case "1", "0":
		where += " AND n.enabled = ?"
		args = append(args, enabled)
	}
	var total int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_node n "+where, args...).Scan(ctx, &total); err != nil {
		return nil, err
	}
	var rows []adminNodeRow
	q := nodeSelect + where + " ORDER BY n.event_date DESC, n.sort_order ASC, n.id DESC LIMIT ? OFFSET ?"
	if err := s.db.NewRaw(q, append(args, size, (page-1)*size)...).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := &AdminNodePage{Total: total, Page: page, Size: size, Items: make([]AdminNode, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, s.nodeView(r))
	}
	return out, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Service) loadNode(ctx context.Context, tx bun.IDB, id int64, lock bool) (adminNodeRow, error) {
	var r adminNodeRow
	q := nodeSelect + "WHERE n.id = ? AND n.deleted = 0"
	if lock {
		q += " FOR UPDATE"
	}
	if err := tx.NewRaw(q, id).Scan(ctx, &r); err != nil {
		if noRows(err) {
			return r, xerr.NewReason(xerr.NotFound, ReasonNodeNotFound, "营销节点不存在")
		}
		return r, err
	}
	return r, nil
}

// AdminGetNode 单个节点。
func (s *Service) AdminGetNode(ctx context.Context, id int64) (*AdminNode, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	r, err := s.loadNode(ctx, s.db, id, false)
	if err != nil {
		return nil, err
	}
	v := s.nodeView(r)
	return &v, nil
}

// applyNodeFields 合并入参并校验：name 必填 ≤40；why ≤60（仅内容推荐，不含优惠 / 折扣 / 赠送，由运营把关）；
// eventDate 必填且是合法日期；leadDays ∈ [0,365]。
func applyNodeFields(row *adminNodeRow, in NodeInput) error {
	if in.Name != nil {
		row.Name = strings.TrimSpace(*in.Name)
	}
	if row.Name == "" || runeLen(row.Name) > maxNodeName {
		return errParam("节点名称必填，最多 40 个字")
	}
	if in.Why != nil {
		row.Why = strings.TrimSpace(*in.Why)
	}
	if runeLen(row.Why) > maxNodeWhy {
		return errParam("推荐语最多 60 个字")
	}
	if in.EventDate != nil {
		d := strings.TrimSpace(*in.EventDate)
		if _, err := time.ParseInLocation("2006-01-02", d, cst); err != nil {
			return errParam("节点日期格式不对，应为 yyyy-MM-dd")
		}
		row.EventDate = d
	}
	if row.EventDate == "" {
		return errParam("节点日期必填")
	}
	if in.LeadDays != nil {
		row.LeadDays = *in.LeadDays
	}
	if row.LeadDays < 0 || row.LeadDays > maxLeadDays {
		return errParam(fmt.Sprintf("提前展示天数需在 0~%d 之间", maxLeadDays))
	}
	if in.SortOrder != nil {
		row.SortOrder = *in.SortOrder
	}
	if in.Enabled != nil {
		row.Enabled = b2i(*in.Enabled)
	}
	return nil
}

// AdminCreateNode 新建节点。code 必填且唯一（含已删除的节点：删除时 code 会被改名释放，见 AdminDeleteNode）。
func (s *Service) AdminCreateNode(ctx context.Context, op Operator, in NodeInput) (int64, error) {
	if err := s.requireDB(); err != nil {
		return 0, err
	}
	code := strings.TrimSpace(in.Code)
	if !nodeCodeRe.MatchString(code) {
		return 0, errParam("节点 code 只能是小写字母 / 数字开头的字母、数字、下划线，长度 2~64")
	}
	row := adminNodeRow{Code: code, LeadDays: defaultLeadDays, Enabled: 1}
	if err := applyNodeFields(&row, in); err != nil {
		return 0, err
	}
	dup := xerr.NewReason(xerr.Conflict, ReasonNodeCodeDuplicate, "节点 code 已存在")
	var id int64
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var n int
		if err := tx.NewRaw("SELECT COUNT(*) FROM afv_plaza_node WHERE code = ?", code).Scan(ctx, &n); err != nil {
			return err
		}
		if n > 0 {
			return dup
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO afv_plaza_node (code, name, why, event_date, lead_days, sort_order, enabled) VALUES (?, ?, ?, ?, ?, ?, ?)",
			row.Code, row.Name, row.Why, row.EventDate, row.LeadDays, row.SortOrder, row.Enabled)
		if err != nil {
			if isDuplicate(err) {
				return dup
			}
			return err
		}
		id, _ = res.LastInsertId()
		row.ID = id
		return s.audit(ctx, tx, op, "NODE_CREATE", ResNode, fmt.Sprint(id), "", nil, nodeSnapshot(row))
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AdminUpdateNode 编辑节点（code 不可改）。
func (s *Service) AdminUpdateNode(ctx context.Context, op Operator, id int64, in NodeInput) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		row, err := s.loadNode(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if c := strings.TrimSpace(in.Code); c != "" && c != row.Code {
			return errParam("节点 code 创建后不能修改")
		}
		before := nodeSnapshot(row)
		if err := applyNodeFields(&row, in); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE afv_plaza_node SET name = ?, why = ?, event_date = ?, lead_days = ?, sort_order = ?, enabled = ? WHERE id = ?",
			row.Name, row.Why, row.EventDate, row.LeadDays, row.SortOrder, row.Enabled, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "NODE_UPDATE", ResNode, fmt.Sprint(id), "", before, nodeSnapshot(row))
	})
}

// AdminSetNodeEnabled 启停节点。
func (s *Service) AdminSetNodeEnabled(ctx context.Context, op Operator, id int64, enabled bool) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		row, err := s.loadNode(ctx, tx, id, true)
		if err != nil {
			return err
		}
		before := nodeSnapshot(row)
		row.Enabled = b2i(enabled)
		if _, err := tx.ExecContext(ctx, "UPDATE afv_plaza_node SET enabled = ? WHERE id = ?", row.Enabled, id); err != nil {
			return err
		}
		action := "NODE_DISABLE"
		if enabled {
			action = "NODE_ENABLE"
		}
		return s.audit(ctx, tx, op, action, ResNode, fmt.Sprint(id), "", before, nodeSnapshot(row))
	})
}

// AdminDeleteNode 逻辑删除节点：同时清掉它的模板关联，并把 code 改名释放（唯一键含已删行，否则同 code 永远建不回来）。
func (s *Service) AdminDeleteNode(ctx context.Context, op Operator, id int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		row, err := s.loadNode(ctx, tx, id, true)
		if err != nil {
			return err
		}
		before := nodeSnapshot(row)
		before["templateCount"] = row.TplCount
		suffix := fmt.Sprintf("%s%d", nodeDeletedSuffix, id)
		keep := []rune(row.Code)
		if len(keep)+len(suffix) > 64 {
			keep = keep[:64-len(suffix)]
		}
		if _, err := tx.ExecContext(ctx, "UPDATE afv_plaza_node SET deleted = 1, enabled = 0, code = ? WHERE id = ?", string(keep)+suffix, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM afv_plaza_node_template WHERE node_id = ?", id); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "NODE_DELETE", ResNode, fmt.Sprint(id), "", before, nil)
	})
}

// ---------------------------------------------------------------- 节点 × 模板（按行业）

// NodeTemplateItem 节点下的一个模板（行业维度视图）。
type NodeTemplateItem struct {
	AdminTemplateRow
	NodeSortOrder int `json:"nodeSortOrder"`
}

// AdminNodeTemplates 节点关联模板：industryID > 0 只取属于该行业（含二级）的；Total 是节点下全部关联数（所有行业）。
type AdminNodeTemplates struct {
	NodeID     int64              `json:"nodeId"`
	IndustryID int64              `json:"industryId"`
	Total      int                `json:"total"`
	Items      []NodeTemplateItem `json:"items"`
}

type nodeLink struct {
	TemplateID int64 `bun:"template_id"`
	SortOrder  int   `bun:"sort_order"`
}

// industryScope 行业（一级含二级）覆盖的行业 id；行业不存在返回 NotFound。
func (s *Service) industryScope(ctx context.Context, tx bun.IDB, industryID int64) ([]int64, error) {
	if industryID <= 0 {
		return nil, errParam("请选择行业")
	}
	var in model.Industry
	if err := tx.NewSelect().Model(&in).Where("ti.id = ?", industryID).Where("ti.deleted = 0").Scan(ctx); err != nil {
		if noRows(err) {
			return nil, xerr.NewReason(xerr.NotFound, ReasonIndustryNotFound, "行业不存在")
		}
		return nil, err
	}
	ids := []int64{industryID}
	if !in.ParentID.Valid {
		var kids []int64
		if err := tx.NewSelect().Model((*model.Industry)(nil)).Column("id").Where("parent_id = ?", industryID).Where("deleted = 0").Scan(ctx, &kids); err != nil {
			return nil, err
		}
		ids = append(ids, kids...)
	}
	return ids, nil
}

// templatesInScope 在 tplIDs 里挑出属于 scope 行业的模板 id。
func templatesInScope(ctx context.Context, tx bun.IDB, scope, tplIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(tplIDs) == 0 {
		return out, nil
	}
	var got []int64
	if err := tx.NewSelect().Model((*model.TemplateIndustry)(nil)).ColumnExpr("DISTINCT template_id").
		Where("industry_id IN (?)", bun.In(scope)).Where("template_id IN (?)", bun.In(tplIDs)).Scan(ctx, &got); err != nil {
		return nil, err
	}
	for _, id := range got {
		out[id] = true
	}
	return out, nil
}

func (s *Service) nodeLinks(ctx context.Context, tx bun.IDB, nodeID int64) ([]nodeLink, error) {
	var links []nodeLink
	err := tx.NewRaw("SELECT template_id, sort_order FROM afv_plaza_node_template WHERE node_id = ? ORDER BY sort_order ASC, id ASC", nodeID).Scan(ctx, &links)
	return links, err
}

// AdminNodeTemplates 列出节点关联的模板（按节点内顺序）。已删除的模板不返回（也不计入 Items，Total 仍含，便于发现悬挂关联）。
func (s *Service) AdminNodeTemplates(ctx context.Context, nodeID, industryID int64) (*AdminNodeTemplates, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	if _, err := s.loadNode(ctx, s.db, nodeID, false); err != nil {
		return nil, err
	}
	links, err := s.nodeLinks(ctx, s.db, nodeID)
	if err != nil {
		return nil, err
	}
	out := &AdminNodeTemplates{NodeID: nodeID, IndustryID: industryID, Total: len(links), Items: []NodeTemplateItem{}}
	if len(links) == 0 {
		return out, nil
	}
	tids := make([]int64, len(links))
	for i, l := range links {
		tids[i] = l.TemplateID
	}
	var allow map[int64]bool
	if industryID > 0 {
		scope, err := s.industryScope(ctx, s.db, industryID)
		if err != nil {
			return nil, err
		}
		if allow, err = templatesInScope(ctx, s.db, scope, tids); err != nil {
			return nil, err
		}
	}
	var rows []model.Template
	if err := s.db.NewSelect().Model(&rows).Where("t.id IN (?)", bun.In(tids)).Where("t.deleted = 0").Scan(ctx); err != nil {
		return nil, err
	}
	items, err := s.adminRows(ctx, rows, industryID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]AdminTemplateRow{}
	for _, it := range items {
		byID[it.ID] = it
	}
	for _, l := range links {
		it, ok := byID[l.TemplateID]
		if !ok || (allow != nil && !allow[l.TemplateID]) {
			continue
		}
		out.Items = append(out.Items, NodeTemplateItem{AdminTemplateRow: it, NodeSortOrder: l.SortOrder})
	}
	return out, nil
}

// AdminAddNodeTemplates 给节点按行业添加模板：模板必须存在、未删除且属于该行业；已挂的跳过；追加到节点内末尾。
// 返回实际新增的 id。
func (s *Service) AdminAddNodeTemplates(ctx context.Context, op Operator, nodeID, industryID int64, templateIDs []int64) ([]int64, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	ids := uniqueIDs(templateIDs)
	if len(ids) == 0 {
		return nil, errParam("请选择要添加的模板")
	}
	if len(ids) > maxLinkBatch {
		return nil, errParam(fmt.Sprintf("一次最多添加 %d 个模板", maxLinkBatch))
	}
	added := []int64{}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		node, err := s.loadNode(ctx, tx, nodeID, true)
		if err != nil {
			return err
		}
		scope, err := s.industryScope(ctx, tx, industryID)
		if err != nil {
			return err
		}
		var alive []int64
		if err := tx.NewSelect().Model((*model.Template)(nil)).Column("id").Where("id IN (?)", bun.In(ids)).Where("deleted = 0").Scan(ctx, &alive); err != nil {
			return err
		}
		aliveSet := map[int64]bool{}
		for _, id := range alive {
			aliveSet[id] = true
		}
		inScope, err := templatesInScope(ctx, tx, scope, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if !aliveSet[id] {
				return xerr.NewReason(xerr.NotFound, ReasonTemplateNotFound, fmt.Sprintf("模板 %d 不存在", id))
			}
			if !inScope[id] {
				return errParam(fmt.Sprintf("模板 %d 不属于所选行业", id))
			}
		}
		links, err := s.nodeLinks(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		have, maxSort := map[int64]bool{}, -1
		for _, l := range links {
			have[l.TemplateID] = true
			if l.SortOrder > maxSort {
				maxSort = l.SortOrder
			}
		}
		for _, id := range ids {
			if have[id] {
				continue
			}
			if len(links)+len(added) >= maxNodeLinks {
				return errParam(fmt.Sprintf("一个节点最多关联 %d 个模板", maxNodeLinks))
			}
			maxSort++
			if _, err := tx.ExecContext(ctx, "INSERT INTO afv_plaza_node_template (node_id, template_id, sort_order) VALUES (?, ?, ?)", nodeID, id, maxSort); err != nil {
				return err
			}
			added = append(added, id)
		}
		if len(added) == 0 {
			return nil
		}
		return s.audit(ctx, tx, op, "NODE_TEMPLATE_ADD", ResNode, fmt.Sprint(nodeID), "", nil,
			map[string]any{"code": node.Code, "industryId": industryID, "templateIds": added})
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// AdminRemoveNodeTemplate 取消节点与某模板的关联（本来就没关联时幂等返回）。
func (s *Service) AdminRemoveNodeTemplate(ctx context.Context, op Operator, nodeID, templateID int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		node, err := s.loadNode(ctx, tx, nodeID, true)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM afv_plaza_node_template WHERE node_id = ? AND template_id = ?", nodeID, templateID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return s.audit(ctx, tx, op, "NODE_TEMPLATE_REMOVE", ResNode, fmt.Sprint(nodeID),
			"", map[string]any{"code": node.Code, "templateIds": []int64{templateID}}, nil)
	})
}

// AdminSortNodeTemplates 调整某行业在节点内的模板顺序：templateIDs 是该行业下要排序的模板（必须都已关联且属于该行业，
// 可以是子集），按给定顺序占用它们原有的排序槽位——不会动其它行业模板的相对位置。
func (s *Service) AdminSortNodeTemplates(ctx context.Context, op Operator, nodeID, industryID int64, templateIDs []int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	if len(uniqueIDs(templateIDs)) != len(templateIDs) {
		return errParam("排序列表里有重复的模板")
	}
	if len(templateIDs) == 0 {
		return errParam("排序列表为空")
	}
	return bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		node, err := s.loadNode(ctx, tx, nodeID, true)
		if err != nil {
			return err
		}
		scope, err := s.industryScope(ctx, tx, industryID)
		if err != nil {
			return err
		}
		links, err := s.nodeLinks(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		cur := map[int64]int{}
		for _, l := range links {
			cur[l.TemplateID] = l.SortOrder
		}
		inScope, err := templatesInScope(ctx, tx, scope, templateIDs)
		if err != nil {
			return err
		}
		slots := make([]int, 0, len(templateIDs))
		for _, id := range templateIDs {
			so, ok := cur[id]
			if !ok {
				return errParam(fmt.Sprintf("模板 %d 没有关联到这个节点", id))
			}
			if !inScope[id] {
				return errParam(fmt.Sprintf("模板 %d 不属于所选行业", id))
			}
			slots = append(slots, so)
		}
		sort.Ints(slots)
		for i := 1; i < len(slots); i++ { // 槽位全相同（都是 0）时也要能表达出先后
			if slots[i] <= slots[i-1] {
				slots[i] = slots[i-1] + 1
			}
		}
		before := make([]int64, 0, len(templateIDs))
		{
			cp := append([]int64(nil), templateIDs...)
			sort.SliceStable(cp, func(i, j int) bool { return cur[cp[i]] < cur[cp[j]] })
			before = append(before, cp...)
		}
		for i, id := range templateIDs {
			if _, err := tx.ExecContext(ctx, "UPDATE afv_plaza_node_template SET sort_order = ? WHERE node_id = ? AND template_id = ?", slots[i], nodeID, id); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, op, "NODE_TEMPLATE_SORT", ResNode, fmt.Sprint(nodeID), "",
			map[string]any{"code": node.Code, "industryId": industryID, "templateIds": before},
			map[string]any{"code": node.Code, "industryId": industryID, "templateIds": templateIDs})
	})
}

func uniqueIDs(in []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
