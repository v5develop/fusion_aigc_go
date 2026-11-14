package core

import (
	"context"
	"strings"

	"github.com/uptrace/bun"
)

// 节点内容推荐（EXPERIENCE 7 节 / V-03，DESIGN 附章 A.1-#2）：仅内容推荐，不含任何优惠 / 折扣 / 赠送。
//   - 节点（年货节、七夕、换季……）由运营在 afv_plaza_node 维护（种子 + 后台 /plaza/admin/nodes 读写，见 admin_nodes.go）；
//   - 节点前 lead_days 天（含节点当天）才出现；同一行业里没有「属于所选行业且此刻可见」的节点模板 → 该节点不出现；
//   - 严格垂直：industry 必填，节点的模板集合只取所选行业范围内的；
//   - 返回按节点日期升序（最近的在前）、最多 maxNodes 个，每个节点最多 maxNodeTemplates 个模板。
//     前端同一时刻只展示 1 个、关闭后按账号记 7 天（localStorage），这些规则都在前端，服务端不记用户状态。

const (
	maxNodes         = 3
	maxNodeTemplates = 12
)

// NodeItem 一个正在窗口内的营销节点。
type NodeItem struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Why       string `json:"why"`
	EventDate string `json:"eventDate"` // yyyy-MM-dd
	DaysLeft  int    `json:"daysLeft"`  // 距节点当天还有几天（当天 = 0）
	// TemplateCount 该行业在此节点下的模板总数（Templates 可能被截到 12 个）。
	TemplateCount int            `json:"templateCount"`
	Templates     []TemplateItem `json:"templates"`
}

// NodeList GET /plaza/nodes 的响应。
type NodeList struct {
	Items []NodeItem `json:"items"`
}

type nodeRow struct {
	ID        int64  `bun:"id"`
	Code      string `bun:"code"`
	Name      string `bun:"name"`
	Why       string `bun:"why"`
	EventDate string `bun:"event_date"`
	DaysLeft  int    `bun:"days_left"`
}

type nodeTplRow struct {
	NodeID     int64 `bun:"node_id"`
	TemplateID int64 `bun:"template_id"`
}

// Nodes 近期营销节点及该行业对应的节点模板集合。industry 必填；匿名可调用。
func (s *Service) Nodes(ctx context.Context, industry, sub string) (*NodeList, error) {
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
	now := s.now()
	today := now.In(cst).Format("2006-01-02")
	var nodes []nodeRow
	err = s.db.NewRaw("SELECT id, code, name, why, DATE_FORMAT(event_date, '%Y-%m-%d') AS event_date, DATEDIFF(event_date, ?) AS days_left "+
		"FROM afv_plaza_node WHERE enabled = 1 AND deleted = 0 AND event_date >= ? AND DATEDIFF(event_date, ?) <= lead_days "+
		"ORDER BY event_date ASC, sort_order ASC, id ASC LIMIT 20", today, today, today).Scan(ctx, &nodes)
	if err != nil {
		return nil, err
	}
	out := &NodeList{Items: []NodeItem{}}
	if len(nodes) == 0 {
		return out, nil
	}
	ids := make([]int64, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	var links []nodeTplRow
	if err := s.db.NewRaw("SELECT node_id, template_id FROM afv_plaza_node_template WHERE node_id IN (?) ORDER BY sort_order ASC, id ASC",
		bun.In(ids)).Scan(ctx, &links); err != nil {
		return nil, err
	}
	byNode := map[int64][]int64{}
	for _, l := range links {
		byNode[l.NodeID] = append(byNode[l.NodeID], l.TemplateID)
	}
	usage := s.usageMap(ctx)
	for _, n := range nodes {
		it := NodeItem{Code: n.Code, Name: n.Name, Why: n.Why, EventDate: n.EventDate, DaysLeft: n.DaysLeft, Templates: []TemplateItem{}}
		for _, tid := range byNode[n.ID] {
			rec, ok := c.InScope(sc, tid, now)
			if !ok {
				continue
			}
			it.TemplateCount++
			if len(it.Templates) < maxNodeTemplates {
				it.Templates = append(it.Templates, s.item(c, rec, usage, now))
			}
		}
		if it.TemplateCount == 0 {
			continue // 该行业在这个节点下没有可用模板：不出现
		}
		out.Items = append(out.Items, it)
		if len(out.Items) >= maxNodes {
			break
		}
	}
	return out, nil
}
