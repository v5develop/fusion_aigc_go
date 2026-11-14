package core

import (
	"context"
	"testing"
	"time"

	"aigc-local-life/app/plaza/model"
)

// t0 = 2026-11-13 12:00 CST。节点按日期窗口出现：[event_date - lead_days, event_date]。

func addNode(t *testing.T, s *Service, code string, eventDate string, lead int, sort int, enabled int, tpls ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	res, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_node (code, name, why, event_date, lead_days, sort_order, enabled) VALUES (?, ?, ?, ?, ?, ?, ?)",
		code, "节点-"+code, "why-"+code, eventDate, lead, sort, enabled)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for i, tid := range tpls {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_node_template (node_id, template_id, sort_order) VALUES (?, ?, ?)", id, tid, i); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func nodeCodes(l *NodeList) []string {
	out := []string{}
	for _, n := range l.Items {
		out = append(out, n.Code)
	}
	return out
}

func TestUserLibNodesWindowAndIndustry(t *testing.T) {
	s, src := userLibService(t)
	ctx := context.Background()
	addNode(t, s, "today", "2026-11-13", 14, 0, 1, 101)     // 节点当天：显示，daysLeft=0
	addNode(t, s, "near", "2026-11-20", 14, 0, 1, 101, 102) // 7 天后：显示
	addNode(t, s, "edge", "2026-11-27", 14, 0, 1, 102)      // 恰好 14 天：显示
	addNode(t, s, "far", "2026-11-28", 14, 0, 1, 101)       // 15 天：不到窗口
	addNode(t, s, "past", "2026-11-12", 14, 0, 1, 101)      // 已过：不显示
	addNode(t, s, "short", "2026-11-20", 3, 0, 1, 101)      // 自己的窗口只有 3 天：还没到
	addNode(t, s, "off", "2026-11-15", 14, 0, 0, 101)       // 停用
	addNode(t, s, "empty", "2026-11-15", 14, 0, 1)          // 没挂模板
	addNode(t, s, "spa_only", "2026-11-15", 14, 0, 1, 104)  // 模板只属于 spa
	draft := tpl(106, "F", "image", t0.Add(-time.Hour))     // 草稿：用户不可见
	draft.State = model.StateDraft
	src.tpls = append(src.tpls, draft)
	src.rels = append(src.rels, rel(106, 1, 0))
	s.Invalidate(ctx)
	addNode(t, s, "invisible", "2026-11-15", 14, 0, 1, 106, 999) // 模板不可见 / 不存在
	if _, err := s.db.ExecContext(ctx, "UPDATE afv_plaza_node SET deleted = 1 WHERE code = 'edge'"); err != nil {
		t.Fatal(err)
	}
	addNode(t, s, "edge2", "2026-11-27", 14, 0, 1, 102)

	l, err := s.Nodes(ctx, "foot_bath", "")
	if err != nil {
		t.Fatal(err)
	}
	// 日期升序；最多 3 个：today、near、edge2（edge 被逻辑删除）
	if got := nodeCodes(l); len(got) != 3 || got[0] != "today" || got[1] != "near" || got[2] != "edge2" {
		t.Fatalf("foot_bath 节点: %v", got)
	}
	if l.Items[0].DaysLeft != 0 || l.Items[1].DaysLeft != 7 || l.Items[2].DaysLeft != 14 ||
		l.Items[0].EventDate != "2026-11-13" || l.Items[1].Name != "节点-near" || l.Items[1].Why != "why-near" {
		t.Fatalf("字段: %+v", l.Items)
	}
	if l.Items[1].TemplateCount != 2 || len(l.Items[1].Templates) != 2 || l.Items[1].Templates[0].PlazaTemplateID != 101 {
		t.Fatalf("节点模板（按节点内顺序）: %+v", l.Items[1])
	}
	// 严格垂直：spa 只看到自己行业的节点模板
	l, _ = s.Nodes(ctx, "spa", "")
	if got := nodeCodes(l); len(got) != 1 || got[0] != "spa_only" || l.Items[0].Templates[0].PlazaTemplateID != 104 {
		t.Fatalf("spa 节点: %v", got)
	}
	// 行业里的节点只含该行业的模板：near 挂了 101、102，二级 bluev 没有它们
	if l, _ = s.Nodes(ctx, "foot_bath", "foot_bath_bluev"); len(l.Items) != 0 {
		t.Fatalf("二级没有节点模板: %v", nodeCodes(l))
	}
	// 模板下线后节点里不再有它；整个节点没有模板则不出现
	src.tpls[0].State = model.StateOffline // 101
	s.Invalidate(ctx)
	l, _ = s.Nodes(ctx, "foot_bath", "")
	if got := nodeCodes(l); len(got) != 2 || got[0] != "near" || len(l.Items[0].Templates) != 1 || l.Items[0].Templates[0].PlazaTemplateID != 102 {
		t.Fatalf("101 下线后: %v %+v", got, l.Items)
	}
	// 无节点：空数组而非 null
	if _, err := s.db.ExecContext(ctx, "DELETE FROM afv_plaza_node_template"); err != nil {
		t.Fatal(err)
	}
	if l, _ = s.Nodes(ctx, "foot_bath", ""); l.Items == nil || len(l.Items) != 0 {
		t.Fatalf("空: %+v", l)
	}
	if _, err := s.Nodes(ctx, "dead", ""); reasonOf(err) != ReasonIndustryNotFound {
		t.Fatalf("停用行业: %v", err)
	}
}
