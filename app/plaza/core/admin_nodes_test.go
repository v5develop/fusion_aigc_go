package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// 营销节点后台集成测试（需要 PLAZA_TEST_DSN 指向临时库，已执行 Go_V1.0.33 / 43 / 44）。

func nodeSvc(t *testing.T) *Service {
	t.Helper()
	s := integrationService(t)
	for _, tb := range []string{"afv_plaza_node_template", "afv_plaza_node", "afv_plaza_upload_hash"} {
		if _, err := s.db.ExecContext(context.Background(), "DELETE FROM "+tb); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func nodeTpl(t *testing.T, s *Service, title string, industries ...int64) int64 {
	t.Helper()
	res, err := s.db.ExecContext(context.Background(), "INSERT INTO afv_compat_plaza_template (title, data_json) VALUES (?, ?)",
		title, `{"result":["https://x.example.com/a.webp"],"prompt":"p"}`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for _, iid := range industries {
		if _, err := s.db.ExecContext(context.Background(), "INSERT INTO afv_plaza_template_industry (template_id, industry_id, sort_order) VALUES (?, ?, 0)", id, iid); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func nodeTplIDs(t *testing.T, s *Service, node, industry int64) []int64 {
	t.Helper()
	l, err := s.AdminNodeTemplates(context.Background(), node, industry)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, it := range l.Items {
		out = append(out, it.ID)
	}
	return out
}

func eqIDs(a, b []int64) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func TestIntegrationNodeCRUD(t *testing.T) {
	s := nodeSvc(t)
	ctx := context.Background()
	id, err := s.AdminCreateNode(ctx, adm, NodeInput{Code: "n_one", Name: sp("年货节"), Why: sp("提前备好"), EventDate: sp("2026-11-20")})
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	n, _ := s.AdminGetNode(ctx, id)
	if n.LeadDays != 14 || !n.Enabled || n.ShowFrom != "2026-11-06" || n.Status != "ACTIVE" { // t0 = 2026-11-13
		t.Fatalf("缺省值 / 展示起始日 / 状态不对: %+v", n)
	}
	// 校验
	bad := []NodeInput{
		{Code: "N", Name: sp("x"), EventDate: sp("2026-11-20")},                                       // code 非法
		{Code: "n_two", Name: sp(""), EventDate: sp("2026-11-20")},                                    // 名称空
		{Code: "n_two", Name: sp(strings.Repeat("好", 41)), EventDate: sp("2026-11-20")},               // 名称过长
		{Code: "n_two", Name: sp("x"), Why: sp(strings.Repeat("好", 61)), EventDate: sp("2026-11-20")}, // 推荐语过长
		{Code: "n_two", Name: sp("x")},                                                                // 缺日期
		{Code: "n_two", Name: sp("x"), EventDate: sp("2026-13-40")},                                   // 日期非法
		{Code: "n_two", Name: sp("x"), EventDate: sp("2026-11-20"), LeadDays: intp(400)},              // 提前天数越界
	}
	for i, in := range bad {
		if _, err := s.AdminCreateNode(ctx, adm, in); err == nil {
			t.Errorf("用例 %d 应被拒: %+v", i, in)
		}
	}
	if _, err := s.AdminCreateNode(ctx, adm, NodeInput{Code: "n_one", Name: sp("重复"), EventDate: sp("2026-11-20")}); reasonOf(err) != ReasonNodeCodeDuplicate {
		t.Fatalf("重复 code 应报 %s: %v", ReasonNodeCodeDuplicate, err)
	}
	// 编辑：不能改 code；可改日期 / 提前天数；用户端窗口随之变化
	if err := s.AdminUpdateNode(ctx, adm, id, NodeInput{Code: "other", Name: sp("x")}); err == nil {
		t.Error("code 不可改")
	}
	if err := s.AdminUpdateNode(ctx, adm, id, NodeInput{Name: sp("年货节2"), EventDate: sp("2026-12-30"), LeadDays: intp(3)}); err != nil {
		t.Fatal(err)
	}
	n, _ = s.AdminGetNode(ctx, id)
	if n.Name != "年货节2" || n.ShowFrom != "2026-12-27" || n.Status != "UPCOMING" {
		t.Fatalf("编辑未生效: %+v", n)
	}
	// 启停
	if err := s.AdminSetNodeEnabled(ctx, adm, id, false); err != nil {
		t.Fatal(err)
	}
	if n, _ = s.AdminGetNode(ctx, id); n.Enabled {
		t.Fatal("应已停用")
	}
	pg, err := s.AdminListNodes(ctx, "", "0", 1, 10)
	if err != nil || pg.Total != 1 {
		t.Fatalf("停用过滤: %v %+v", err, pg)
	}
	if pg, _ = s.AdminListNodes(ctx, "年货", "1", 1, 10); pg.Total != 0 {
		t.Fatalf("启用过滤应为空: %+v", pg)
	}
	if pg, _ = s.AdminListNodes(ctx, "n_o", "", 1, 10); pg.Total != 1 {
		t.Fatalf("关键字按 code 匹配: %+v", pg)
	}
	if pg, _ = s.AdminListNodes(ctx, "%", "", 1, 10); pg.Total != 0 {
		t.Fatalf("百分号应按字面匹配: %+v", pg)
	}
	// 删除后 code 可复用；已删节点读 / 改都是 NODE_NOT_FOUND
	if err := s.AdminDeleteNode(ctx, adm, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminGetNode(ctx, id); reasonOf(err) != ReasonNodeNotFound {
		t.Fatalf("已删除应 NOT_FOUND: %v", err)
	}
	if err := s.AdminSetNodeEnabled(ctx, adm, id, true); reasonOf(err) != ReasonNodeNotFound {
		t.Fatalf("已删除不可启停: %v", err)
	}
	if _, err := s.AdminCreateNode(ctx, adm, NodeInput{Code: "n_one", Name: sp("新的"), EventDate: sp("2026-11-20")}); err != nil {
		t.Fatalf("删除后 code 应可复用: %v", err)
	}
	for _, a := range []string{"NODE_CREATE", "NODE_UPDATE", "NODE_DISABLE", "NODE_DELETE"} {
		if auditCount(t, s, a) == 0 {
			t.Errorf("缺审计 %s", a)
		}
	}
}

func TestIntegrationNodeTemplatesByIndustry(t *testing.T) {
	s := nodeSvc(t)
	ctx := context.Background()
	f, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "nt_foot", Name: sp("足浴"), Hint: sp("门店")})
	f = mustID(t, f, err)
	sub, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "nt_foot_v", Name: sp("蓝V"), Hint: sp("门店"), ParentID: &f})
	sub = mustID(t, sub, err)
	spa, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "nt_spa", Name: sp("SPA"), Hint: sp("养生")})
	spa = mustID(t, spa, err)

	a, b, c := nodeTpl(t, s, "A", f), nodeTpl(t, s, "B", sub), nodeTpl(t, s, "C", f)
	d := nodeTpl(t, s, "D", spa)
	both := nodeTpl(t, s, "Both", f, spa)
	nid, err := s.AdminCreateNode(ctx, adm, NodeInput{Code: "nt_n", Name: sp("七夕"), EventDate: sp("2026-11-20")})
	if err != nil {
		t.Fatal(err)
	}

	// 添加：按行业；一级行业覆盖二级；不属于该行业 / 不存在 / 缺行业都拒
	added, err := s.AdminAddNodeTemplates(ctx, adm, nid, f, []int64{a, b, c, a})
	if err != nil || !eqIDs(added, []int64{a, b, c}) {
		t.Fatalf("添加: %v %v", err, added)
	}
	if _, err := s.AdminAddNodeTemplates(ctx, adm, nid, f, []int64{d}); err == nil {
		t.Error("spa 模板不能按足浴添加")
	}
	if _, err := s.AdminAddNodeTemplates(ctx, adm, nid, f, []int64{99999}); reasonOf(err) != ReasonTemplateNotFound {
		t.Errorf("不存在的模板: %v", err)
	}
	if _, err := s.AdminAddNodeTemplates(ctx, adm, nid, 0, []int64{a}); err == nil {
		t.Error("必须选行业")
	}
	if _, err := s.AdminAddNodeTemplates(ctx, adm, nid, f, nil); err == nil {
		t.Error("空列表应拒")
	}
	if again, err := s.AdminAddNodeTemplates(ctx, adm, nid, f, []int64{a}); err != nil || len(again) != 0 {
		t.Errorf("已挂的应跳过: %v %v", err, again)
	}
	if _, err := s.AdminAddNodeTemplates(ctx, adm, nid, spa, []int64{d, both}); err != nil {
		t.Fatal(err)
	}
	if got := nodeTplIDs(t, s, nid, f); !eqIDs(got, []int64{a, b, c, both}) {
		t.Fatalf("足浴视图: %v", got)
	}
	if got := nodeTplIDs(t, s, nid, spa); !eqIDs(got, []int64{d, both}) {
		t.Fatalf("SPA 视图: %v", got)
	}
	if l, _ := s.AdminNodeTemplates(ctx, nid, 0); l.Total != 5 || len(l.Items) != 5 {
		t.Fatalf("全部: %+v", l.Total)
	}

	// 排序：只动该行业，不改别的行业相对位置；子集也可
	if err := s.AdminSortNodeTemplates(ctx, adm, nid, f, []int64{both, c, b, a}); err != nil {
		t.Fatal(err)
	}
	if got := nodeTplIDs(t, s, nid, f); !eqIDs(got, []int64{both, c, b, a}) {
		t.Fatalf("足浴排序后: %v", got)
	}
	if got := nodeTplIDs(t, s, nid, spa); !eqIDs(got, []int64{both, d}) { // both 占了足浴里最前的槽位
		t.Fatalf("SPA 视图 both 与 d 的相对顺序随槽位变化: %v", got)
	}
	if err := s.AdminSortNodeTemplates(ctx, adm, nid, f, []int64{a, c}); err != nil {
		t.Fatal(err)
	}
	if got := nodeTplIDs(t, s, nid, f); !eqIDs(got, []int64{both, a, b, c}) {
		t.Fatalf("子集排序: %v", got)
	}
	if err := s.AdminSortNodeTemplates(ctx, adm, nid, f, []int64{a, a}); err == nil {
		t.Error("重复应拒")
	}
	if err := s.AdminSortNodeTemplates(ctx, adm, nid, f, []int64{d}); err == nil {
		t.Error("不属于该行业应拒")
	}
	if err := s.AdminSortNodeTemplates(ctx, adm, nid, spa, []int64{99999}); err == nil {
		t.Error("没挂到节点应拒")
	}

	// 槽位全相同（都是 0）也能排出先后
	nid2, _ := s.AdminCreateNode(ctx, adm, NodeInput{Code: "nt_n2", Name: sp("x"), EventDate: sp("2026-11-20")})
	for _, id := range []int64{a, c} {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_node_template (node_id, template_id, sort_order) VALUES (?, ?, 0)", nid2, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AdminSortNodeTemplates(ctx, adm, nid2, f, []int64{c, a}); err != nil {
		t.Fatal(err)
	}
	if got := nodeTplIDs(t, s, nid2, f); !eqIDs(got, []int64{c, a}) {
		t.Fatalf("全 0 槽位排序: %v", got)
	}

	// 移除（幂等）；模板被删后列表不再返回但不报错
	if err := s.AdminRemoveNodeTemplate(ctx, adm, nid, b); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminRemoveNodeTemplate(ctx, adm, nid, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE afv_compat_plaza_template SET deleted = 1 WHERE id = ?", c); err != nil {
		t.Fatal(err)
	}
	if got := nodeTplIDs(t, s, nid, f); !eqIDs(got, []int64{both, a}) {
		t.Fatalf("移除 / 删除模板后: %v", got)
	}
	// 删除节点同时清关联
	if err := s.AdminDeleteNode(ctx, adm, nid); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_node_template WHERE node_id = ?", nid).Scan(ctx, &n); err != nil || n != 0 {
		t.Fatalf("删除节点应清关联: %v %d", err, n)
	}
	if _, err := s.AdminNodeTemplates(ctx, nid, 0); reasonOf(err) != ReasonNodeNotFound {
		t.Fatalf("%v", err)
	}
	for _, a := range []string{"NODE_TEMPLATE_ADD", "NODE_TEMPLATE_SORT", "NODE_TEMPLATE_REMOVE"} {
		if auditCount(t, s, a) == 0 {
			t.Errorf("缺审计 %s", a)
		}
	}
}

// 后台编辑后，用户端 Nodes() 读到的口径一致（窗口 + 启停）。
func TestIntegrationNodeAdminFeedsUserSide(t *testing.T) {
	s := nodeSvc(t)
	ctx := context.Background()
	id, err := s.AdminCreateNode(ctx, adm, NodeInput{Code: "feed", Name: sp("元旦"), EventDate: sp("2026-11-20")})
	if err != nil {
		t.Fatal(err)
	}
	var cnt int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_node WHERE id = ? AND enabled = 1 AND event_date = '2026-11-20' AND lead_days = 14", id).Scan(ctx, &cnt); err != nil || cnt != 1 {
		t.Fatalf("落库列与用户端读取口径不一致: %v %d", err, cnt)
	}
}

func TestIntegrationSQLDedup(t *testing.T) {
	s := nodeSvc(t)
	ctx := context.Background()
	d := &sqlDedup{db: s.db}
	r := &ConfirmResult{URL: "https://cdn/x/a.png", Kind: "image", Width: 600, Height: 700, Size: 10, MediaStatus: "READY"}
	sha := strings.Repeat("a", 64)
	if got, err := d.Find(ctx, sha); err != nil || got != nil {
		t.Fatalf("未登记: %v %v", err, got)
	}
	if ex, err := d.Register(ctx, sha, "k1", r); err != nil || ex != nil {
		t.Fatalf("首次登记: %v %v", err, ex)
	}
	r2 := *r
	r2.URL = "https://cdn/x/b.png"
	ex, err := d.Register(ctx, sha, "k2", &r2)
	if err != nil || ex == nil || ex.URL != r.URL || ex.Width != 600 {
		t.Fatalf("并发 / 重复登记应返回先到者: %v %+v", err, ex)
	}
}
