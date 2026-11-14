package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 集成测试：对真实 MySQL 跑后台全流程（建行业 / 模板、发布、定时、批量、排序、回收站、审计、示例素材、建议、缺口、偏好、最近用过）。
// 需要先在一个临时库里执行 Go_V1.0.33 / 34（并有 Java 的 afv_compat_plaza_template 表），再：
//
//	PLAZA_TEST_DSN='root:123456@tcp(127.0.0.1:43306)/plaza_scratch_tmp?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai&time_zone=%27%2B08%3A00%27' \
//	  go test ./app/plaza/core -run Integration -v
//
// 没设 PLAZA_TEST_DSN 时跳过。测试开头会清空 plaza 相关表（所以只能指向临时库，绝不要指向真实库）。
func integrationService(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("PLAZA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PLAZA_TEST_DSN，跳过集成测试")
	}
	if !strings.Contains(dsn, "scratch") && !strings.Contains(dsn, "test") {
		t.Fatalf("PLAZA_TEST_DSN 必须指向临时库（库名含 scratch 或 test）：%s", dsn)
	}
	conn := sqlx.NewMysql(dsn)
	db, err := bundb.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stubs := []string{
		// afv_agent_turn（Java 所有）：已有真表就用真表（headline/store 的集成测试也读它），stub 与插入都带上真表 NOT NULL 无默认的列。
		"CREATE TABLE IF NOT EXISTS afv_agent_turn (id BIGINT AUTO_INCREMENT PRIMARY KEY, user_id BIGINT NOT NULL, conversation_id VARCHAR(64) NOT NULL, turn_no INT NOT NULL, turn_id VARCHAR(64) NOT NULL, run_id VARCHAR(64) NOT NULL, status VARCHAR(32) NOT NULL, plaza_template_id BIGINT NULL, create_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, update_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)",
		// 技能表 / 耗时表（Java 所有）：真表多列 NOT NULL 无默认、本测试只用少数列，先删再建精简 stub。
		"DROP TABLE IF EXISTS afv_compat_agent_skill",
		skillStubDDL,
		"DROP TABLE IF EXISTS afv_task_latency",
		"CREATE TABLE afv_task_latency (task_id VARCHAR(64) PRIMARY KEY, entry_code VARCHAR(128), status VARCHAR(16), total_ms BIGINT, submitted_at DATETIME(3) NULL, compute_error VARCHAR(64) NULL, gateway_degraded TINYINT NOT NULL DEFAULT 0)",
	}
	for _, q := range stubs {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, tb := range []string{"afv_template_industry", "afv_plaza_template_industry", "afv_user_industry_pref", "afv_plaza_audit_log",
		"afv_plaza_industry_suggestion", "afv_plaza_industry_suggestion_status", "afv_plaza_search_miss_daily", "afv_plaza_search_miss_term",
		"afv_plaza_sample_asset", "afv_plaza_channel", "afv_compat_plaza_template", "afv_agent_turn", "afv_compat_agent_skill", "afv_task_latency"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+tb); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO afv_compat_agent_skill (agent_skill_id, agent_skill_code, title) VALUES (1876543210987654321, 'it_skill', '集成测试技能')"); err != nil {
		t.Fatal(err)
	}
	cfg := Config{}
	cfg.Industry.Enabled = true
	cfg.AdminWriteEnabled = true
	return New(db, nil, cfg)
}

// skillStubDDL afv_compat_agent_skill 共享 stub：NOT NULL 列与 Java 真表一致（agent_skill_id / agent_skill_code / title）。
const skillStubDDL = "CREATE TABLE afv_compat_agent_skill (id BIGINT AUTO_INCREMENT PRIMARY KEY, agent_skill_id BIGINT NOT NULL, agent_skill_code VARCHAR(128) NOT NULL, title VARCHAR(128) NOT NULL, deleted TINYINT NOT NULL DEFAULT 0)"

var adm = Operator{ID: 7, Name: "tester", IP: "127.0.0.1", RequestID: "rid", CanPublish: true}

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

func mustID(t *testing.T, id int64, err error) int64 {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestIntegrationAdminAndUserFlow(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()

	// ---- 行业：校验与两级
	foot := mustID(t, 0, nil)
	var err error
	foot, err = s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "foot", Name: sp("足浴"), Hint: sp("按摩 / 泡脚门店"), SortOrder: intp(20)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "foot", Name: sp("重复"), Hint: sp("x")}); reasonOf(err) != ReasonIndustryCodeDuplicate {
		t.Fatalf("code 重复: %v", err)
	}
	for _, bad := range []string{"ALL", "all", "A1", "1ab", "x", "a-b"} {
		if _, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: bad, Name: sp("n"), Hint: sp("h")}); err == nil {
			t.Errorf("code %q 应被拒", bad)
		}
	}
	if _, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "nohint", Name: sp("无说明")}); codeOf(err) != xerr.ParamError {
		t.Fatalf("启用的行业必须有说明: %v", err)
	}
	blue, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "foot_bluev", Name: sp("蓝V号"), Hint: sp("企业认证的商家号"), ParentID: &foot})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "foot_x", Name: sp("三级"), Hint: sp("h"), ParentID: &blue}); reasonOf(err) != ReasonIndustryDepthExceeded {
		t.Fatalf("二级下不能再建子级: %v", err)
	}
	spa, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "spa", Name: sp("SPA"), Hint: sp("养生馆"), SortOrder: intp(10)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminUpdateIndustry(ctx, adm, spa, IndustryInput{Code: "other_code"}); codeOf(err) != xerr.ParamError {
		t.Fatalf("code 不可改: %v", err)
	}

	// ---- 模板：校验、草稿、发布前置检查
	if _, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: " "}); codeOf(err) != xerr.ParamError {
		t.Fatal("标题必填")
	}
	if _, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "v", MediaType: "video"}); codeOf(err) != xerr.ParamError {
		t.Fatal("视频必填 videoUrl")
	}
	if _, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "bad", AgentSkillID: sp("999")}); codeOf(err) != xerr.NotFound {
		t.Fatalf("技能不存在: %v", err)
	}
	if _, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "bad", Channels: json.RawMessage(`["ghost"]`)}); codeOf(err) != xerr.ParamError {
		t.Fatal("渠道不存在")
	}
	payload := json.RawMessage(`{"prompt":"SECRET","guidance":"放一张门头照","result":["https://b.oss-cn-hangzhou.aliyuncs.com/r1.webp"],"description":"d1"}`)
	t1 := mustID(t, 0, nil)
	t1, err = s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "足浴团购海报", Description: "d1", Tags: []string{"足浴", "团购"},
		Payload: payload, AgentSkillID: sp("1876543210987654321"), IndustryIDs: &[]int64{blue}})
	if err != nil {
		t.Fatal(err)
	}
	det, err := s.AdminGetTemplate(ctx, t1)
	if err != nil || det.State != model.StateDraft || det.Enabled || len(det.IndustryIDs) != 1 || *det.AgentSkillID != "1876543210987654321" {
		t.Fatalf("新建默认草稿且不可见: %+v %v", det, err)
	}
	if !strings.Contains(string(det.PayloadKnown), "prompt") {
		t.Fatalf("后台详情含 prompt（运营要编辑）: %s", det.PayloadKnown)
	}
	// 草稿用户端不可见
	if pg, _ := s.Templates(ctx, ListParams{Industry: "foot", Page: 1, Size: 10}); pg.Total != 0 {
		t.Fatalf("草稿不可见: %+v", pg)
	}
	// 未绑行业不让发布（可 force）
	t2, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "无行业", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, t2, PublishInput{}); codeOf(err) != xerr.ParamError {
		t.Fatalf("没有行业不可发布: %v", err)
	}
	if r, err := s.AdminPublishTemplate(ctx, adm, t2, PublishInput{Force: true}); err != nil || len(r.Warnings) != 1 {
		t.Fatalf("force 发布带警告: %+v %v", r, err)
	}
	// 没有效果图不可发布
	t3, _ := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "无图", Payload: json.RawMessage(`{"prompt":"p"}`), IndustryIDs: &[]int64{spa}})
	if _, err := s.AdminPublishTemplate(ctx, adm, t3, PublishInput{}); codeOf(err) != xerr.ParamError {
		t.Fatalf("没有效果图不可发布: %v", err)
	}
	// 窗口先后
	if _, err := s.AdminPublishTemplate(ctx, adm, t1, PublishInput{OnlineAt: "2030-01-02 00:00:00", OfflineAt: "2030-01-01 00:00:00"}); codeOf(err) != xerr.ParamError {
		t.Fatalf("下线必须晚于上线: %v", err)
	}

	// ---- 发布 → 用户端可见；一级含二级；下线 → 消失
	if _, err := s.AdminPublishTemplate(ctx, adm, t1, PublishInput{}); err != nil {
		t.Fatal(err)
	}
	for _, in := range [][2]string{{"foot", ""}, {"foot", "foot_bluev"}} {
		pg, err := s.Templates(ctx, ListParams{Industry: in[0], Sub: in[1], Page: 1, Size: 10})
		if err != nil || pg.Total != 1 || pg.Data[0].PlazaTemplateID != t1 {
			t.Fatalf("%v 应见到发布的模板: %+v %v", in, pg, err)
		}
		if strings.Contains(string(pg.Data[0].Payload), "SECRET") {
			t.Fatal("用户端不得下发 prompt")
		}
	}
	if pg, _ := s.Templates(ctx, ListParams{Industry: "spa", Page: 1, Size: 10}); pg.Total != 0 {
		t.Fatalf("别的行业看不到: %+v", pg)
	}
	d2, _ := s.Industries(ctx)
	if !d2.GateEnabled || len(d2.Industries) != 1 || d2.Industries[0].Code != "foot" || len(d2.Industries[0].Children) != 1 {
		t.Fatalf("目录只含有可见模板的行业（spa 没有）: %+v", d2)
	}
	c, err := s.Counts(ctx, "foot", "")
	if err != nil || c.Image != 1 || c.Video != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	if err := s.AdminOfflineTemplate(ctx, adm, t1); err != nil {
		t.Fatal(err)
	}
	if pg, _ := s.Templates(ctx, ListParams{Industry: "foot", Page: 1, Size: 10}); pg.Total != 0 {
		t.Fatalf("下线后不可见: %+v", pg)
	}

	// ---- 定时上线：未到点不可见，enabled=0；到点后（用时钟快进）可见，边界任务翻转 enabled
	soon := time.Now().Add(2 * time.Hour).In(cst).Format("2006-01-02 15:04:05")
	if _, err := s.AdminPublishTemplate(ctx, adm, t1, PublishInput{OnlineAt: soon}); err != nil {
		t.Fatal(err)
	}
	det, _ = s.AdminGetTemplate(ctx, t1)
	if det.DisplayState != DisplayScheduled || det.Enabled {
		t.Fatalf("定时中: %+v", det.AdminTemplateRow)
	}
	if pg, _ := s.Templates(ctx, ListParams{Industry: "foot", Page: 1, Size: 10}); pg.Total != 0 {
		t.Fatal("定时未到不可见")
	}
	real := s.now
	s.now = func() time.Time { return real().Add(3 * time.Hour) }
	if pg, _ := s.Templates(ctx, ListParams{Industry: "foot", Page: 1, Size: 10}); pg.Total != 1 {
		t.Fatalf("到点后读时过滤立即可见（不依赖任务）: %+v", pg)
	}
	s.now = real
	// 把 online_at 改到过去，边界任务应把 enabled 翻成 1
	if _, err := s.db.ExecContext(ctx, "UPDATE afv_compat_plaza_template SET online_at = DATE_SUB(NOW(), INTERVAL 1 MINUTE) WHERE id = ?", t1); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SyncLifecycle(ctx); err != nil || n != 1 {
		t.Fatalf("边界任务翻转 1 个: %d %v", n, err)
	}
	det, _ = s.AdminGetTemplate(ctx, t1)
	if !det.Enabled || det.DisplayState != DisplayPublished {
		t.Fatalf("翻转后: %+v", det.AdminTemplateRow)
	}

	// ---- 编辑：payload 省略保留、industryIds 省略不改、乐观并发
	upd := det.UpdateTime
	if _, err := s.AdminUpdateTemplate(ctx, adm, t1, TemplateInput{Title: "足浴团购海报 v2", Tags: []string{"足浴"}}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("updatedAt 缺失 → PARAM_MISSING: %v", err)
	}
	if _, err := s.AdminUpdateTemplate(ctx, adm, t1, TemplateInput{Title: "足浴团购海报 v2", Tags: []string{"足浴"}, UpdatedAt: "2000-01-01 00:00:00"}); reasonOf(err) != ReasonTemplateConflict {
		t.Fatalf("updatedAt 不符 → 冲突: %v", err)
	}
	wr, err := s.AdminUpdateTemplate(ctx, adm, t1, TemplateInput{Title: "足浴团购海报 v2", Tags: []string{"足浴"}, UpdatedAt: upd})
	if err != nil || wr.Revision != det.Revision+1 || wr.UpdatedAt == "" {
		t.Fatalf("编辑返回新的 revision / updatedAt: %+v %v", wr, err)
	}
	// revision 优先：同一秒内用旧 revision 再存（updatedAt 仍相同）→ 冲突
	stale := det.Revision
	if _, err := s.AdminUpdateTemplate(ctx, adm, t1, TemplateInput{Title: "足浴团购海报 v2", Tags: []string{"足浴"}, UpdatedAt: wr.UpdatedAt, Revision: &stale}); reasonOf(err) != ReasonTemplateConflict {
		t.Fatalf("旧 revision → 冲突: %v", err)
	}
	det, _ = s.AdminGetTemplate(ctx, t1)
	if det.Title != "足浴团购海报 v2" || len(det.IndustryIDs) != 1 || !strings.Contains(string(det.Payload), "放一张门头照") || *det.AgentSkillID != "1876543210987654321" || det.Description != "" {
		t.Fatalf("payload / 行业 / 技能保持，description 缺省被清: %+v", det)
	}
	if _, err := s.AdminUpdateTemplate(ctx, adm, t1, TemplateInput{Title: "x", IndustryIDs: &[]int64{}, UpdatedAt: updOf(t, s, t1)}); err != nil {
		t.Fatal(err)
	}
	if det, _ = s.AdminGetTemplate(ctx, t1); len(det.IndustryIDs) != 0 {
		t.Fatalf("[] 清空行业: %+v", det.IndustryIDs)
	}
	if _, err := s.AdminUpdateTemplate(ctx, adm, t1, TemplateInput{Title: "足浴团购海报", IndustryIDs: &[]int64{blue, foot}, UpdatedAt: updOf(t, s, t1)}); err != nil {
		t.Fatal(err)
	}

	// ---- 列表：筛选 / 排序 / 未分配
	t4, _ := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "视频模板", MediaType: "video", VideoURL: "https://v.example.com/a.mp4", PosterURL: "https://v.example.com/a.jpg", Payload: payload, IndustryIDs: &[]int64{foot}})
	if _, err := s.RunMediaJobs(ctx); err != nil { // 视频先 PROCESSING，任务处理完（已有封面、无转码能力）→ READY 才能发布
		t.Fatal(err)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, t4, PublishInput{}); err != nil {
		t.Fatal(err)
	}
	lst, err := s.AdminListTemplates(ctx, TemplateFilter{Page: 1, Size: 20})
	if err != nil || lst.Total < 4 || lst.UnassignedCount != 1 { // t3 草稿挂 spa；t2 force 发布无行业 → 未分配 1
		t.Fatalf("%+v %v", lst, err)
	}
	for name, f := range map[string]TemplateFilter{
		"关键词":    {Keyword: "视频"},
		"行业含子级":  {IndustryID: foot, IncludeChildren: true},
		"仅直挂":    {IndustryID: foot},
		"媒体":     {MediaType: "video"},
		"状态已发布":  {State: "PUBLISHED"},
		"状态草稿":   {State: "DRAFT"},
		"未分配":    {Unassigned: true},
		"创建人":    {CreatedBy: 7},
		"排序更新时间": {Sort: "updatedAt", Order: "asc"},
		"排序做同款":  {Sort: "uses7d"},
		"排序行业内":  {Sort: "industryOrder", IndustryID: foot},
		"上线区间":   {OnlineFrom: "2000-01-01 00:00:00", OnlineTo: "2100-01-01 00:00:00"},
	} {
		f.Page, f.Size = 1, 20
		if _, err := s.AdminListTemplates(ctx, f); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r, _ := s.AdminListTemplates(ctx, TemplateFilter{Keyword: "视频", Page: 1, Size: 20}); r.Total != 1 {
		t.Fatalf("关键词筛选: %+v", r)
	}
	if r, _ := s.AdminListTemplates(ctx, TemplateFilter{Unassigned: true, Page: 1, Size: 20}); r.Total != 1 || r.Items[0].ID != t2 {
		t.Fatalf("未分配筛选: %+v", r)
	}
	if _, err := s.AdminListTemplates(ctx, TemplateFilter{State: "WAT"}); codeOf(err) != xerr.ParamError {
		t.Fatal("非法 state")
	}

	// ---- 行业内排序：一级合并视图含仅挂二级的模板时只读；二级内可排
	t5, _ := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "蓝V第二个", Payload: payload, IndustryIDs: &[]int64{blue}})
	view, err := s.AdminIndustryTemplates(ctx, foot)
	if err != nil || len(view.Items) < 2 {
		t.Fatalf("%+v %v", view, err)
	}
	bview, _ := s.AdminIndustryTemplates(ctx, blue)
	if bview.ReadOnly || len(bview.Items) != 2 {
		t.Fatalf("二级视图可排序: %+v", bview)
	}
	if err := s.AdminSetIndustryOrder(ctx, adm, blue, []int64{t5, t1}); err != nil {
		t.Fatal(err)
	}
	bview, _ = s.AdminIndustryTemplates(ctx, blue)
	if bview.Items[0].ID != t5 || bview.Items[1].ID != t1 {
		t.Fatalf("行业内顺序: %v", []int64{bview.Items[0].ID, bview.Items[1].ID})
	}
	if err := s.AdminSetIndustryOrder(ctx, adm, blue, []int64{t4}); codeOf(err) != xerr.ParamError { // t4 没挂 blue
		t.Fatalf("不在该行业的模板不能排: %v", err)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, t5, PublishInput{}); err != nil {
		t.Fatal(err)
	}
	pg, _ := s.Templates(ctx, ListParams{Industry: "foot", Sub: "foot_bluev", Page: 1, Size: 10})
	if len(pg.Data) != 2 || pg.Data[0].PlazaTemplateID != t5 {
		t.Fatalf("用户端列表按行业内顺序: %+v", pg.Data)
	}

	// ---- 复制 / 批量 / 回收站
	dup, err := s.AdminDuplicateTemplate(ctx, adm, t1)
	if err != nil {
		t.Fatal(err)
	}
	dd, _ := s.AdminGetTemplate(ctx, dup)
	if dd.State != model.StateDraft || !strings.HasSuffix(dd.Title, "（副本）") || len(dd.IndustryIDs) != 2 || *dd.AgentSkillID != "1876543210987654321" {
		t.Fatalf("复制: %+v", dd)
	}
	res, err := s.AdminBatch(ctx, adm, BatchInput{Action: "publish", IDs: []int64{dup, t3, 99999}})
	if err != nil || len(res.OK) != 1 || len(res.Failed) != 2 || res.BatchID == "" {
		t.Fatalf("批量部分失败返回原因: %+v %v", res, err)
	}
	pubBatch := res.BatchID
	if _, err := s.AdminBatch(ctx, adm, BatchInput{Action: "assign", IDs: []int64{t3, t2}, IndustryIDs: []int64{foot}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.AdminListTemplates(ctx, TemplateFilter{Unassigned: true, Page: 1, Size: 20}); r.Total != 0 {
		t.Fatalf("批量归类后无未分配: %+v", r)
	}
	if res, err = s.AdminBatch(ctx, adm, BatchInput{Action: "setOrder", IDs: []int64{t3}, IndustryIDs: []int64{foot}}); err != nil || len(res.OK) != 1 {
		t.Fatalf("置顶: %+v %v", res, err)
	}
	if _, err := s.AdminBatch(ctx, adm, BatchInput{Action: "bogus", IDs: []int64{1}}); codeOf(err) != xerr.ParamError {
		t.Fatal("非法动作")
	}
	if err := s.AdminDeleteTemplate(ctx, adm, dup); err != nil {
		t.Fatal(err)
	}
	if pg, _ := s.Templates(ctx, ListParams{Industry: "foot", Keyword: "副本", Page: 1, Size: 10}); pg.Total != 0 {
		t.Fatal("回收站内不可见")
	}
	tr, _ := s.AdminTrash(ctx, 1, 10)
	if tr.Total != 1 || tr.Items[0].PurgeAt == nil {
		t.Fatalf("回收站: %+v", tr)
	}
	if err := s.AdminPurgeTemplate(ctx, adm, t1); codeOf(err) != xerr.ParamError {
		t.Fatalf("不在回收站的不能彻底删除: %v", err)
	}
	if err := s.AdminRestoreTemplate(ctx, adm, dup); err != nil {
		t.Fatal(err)
	}
	if dd, _ = s.AdminGetTemplate(ctx, dup); dd.State != model.StateOffline || dd.Deleted {
		t.Fatalf("恢复后是 OFFLINE 不自动上线: %+v", dd.AdminTemplateRow)
	}
	if err := s.AdminDeleteTemplate(ctx, adm, dup); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminPurgeTemplate(ctx, adm, dup); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminGetTemplate(ctx, dup); err == nil {
		t.Fatal("彻底删除后应不存在")
	}
	if n, _ := s.db.NewSelect().Model((*model.TemplateIndustry)(nil)).Where("template_id = ?", dup).Count(ctx); n != 0 {
		t.Fatal("关系行应一并删除")
	}
	// 回收站过期清理
	if err := s.AdminDeleteTemplate(ctx, adm, t3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE afv_compat_plaza_template SET deleted_at = DATE_SUB(NOW(), INTERVAL 31 DAY) WHERE id = ?", t3); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeExpiredTrash(ctx); err != nil || n != 1 {
		t.Fatalf("到期清理: %d %v", n, err)
	}

	// ---- 预览
	pv, err := s.AdminPreview(ctx, ListParams{Industry: "foot", Page: 1, Size: 20}, "", true)
	if err != nil || pv.Total == 0 {
		t.Fatalf("%+v %v", pv, err)
	}
	if _, err := s.AdminPreview(ctx, ListParams{Page: 1}, "", false); codeOf(err) != xerr.ParamMissing {
		t.Fatal("预览必填行业")
	}

	// ---- 示例素材：模板级 → 行业级，≤6，不取别的行业
	for i := 0; i < 6; i++ {
		if _, err := s.AdminCreateSample(ctx, adm, SampleInput{ScopeType: "INDUSTRY", IndustryID: foot, URL: sp(fmt.Sprintf("https://x.com/i%d.jpg", i)), Title: sp("行业示例")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AdminCreateSample(ctx, adm, SampleInput{ScopeType: "INDUSTRY", IndustryID: foot, URL: sp("https://x.com/i7.jpg"), Title: sp("第七条")}); codeOf(err) != xerr.ParamError {
		t.Fatalf("每范围最多 6 条: %v", err)
	}
	if _, err := s.AdminCreateSample(ctx, adm, SampleInput{ScopeType: "INDUSTRY", IndustryID: spa, URL: sp("https://x.com/spa.jpg"), Title: sp("spa 示例")}); err != nil {
		t.Fatal(err)
	}
	tsid, err := s.AdminCreateSample(ctx, adm, SampleInput{ScopeType: "TEMPLATE", TemplateID: t5, URL: sp("https://x.com/t5.jpg"), Title: sp("模板示例")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminCreateSample(ctx, adm, SampleInput{ScopeType: "TEMPLATE", TemplateID: t5, URL: sp("ftp://bad"), Title: sp("x")}); codeOf(err) != xerr.ParamError {
		t.Fatal("非 http(s) 地址被拒")
	}
	smp, err := s.Samples(ctx, "foot", "foot_bluev", t5)
	if err != nil || len(smp) != 6 || smp[0].Scope != "TEMPLATE" || smp[0].Title != "模板示例" {
		t.Fatalf("模板级在前、最多 6 条: %+v %v", smp, err)
	}
	for _, x := range smp {
		if x.Title == "spa 示例" {
			t.Fatal("绝不返回其他行业的示例")
		}
	}
	if _, err := s.Samples(ctx, "spa", "", t5); codeOf(err) != xerr.NotFound {
		t.Fatalf("模板不属于该行业: %v", err)
	}
	if _, err := s.Samples(ctx, "", "", 0); codeOf(err) != xerr.ParamMissing {
		t.Fatal("samples 必填行业")
	}
	if err := s.AdminDeleteSample(ctx, adm, tsid); err != nil {
		t.Fatal(err)
	}

	// ---- 渠道
	chID, err := s.AdminCreateChannel(ctx, adm, ChannelInput{Code: "douyin_9_16", Name: sp("抖音 9:16"), Width: intp(1080), Height: intp(1920), Ratio: sp("9:16")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AdminUpdateChannel(ctx, adm, chID, ChannelInput{Code: "other_code"}); codeOf(err) != xerr.ParamError {
		t.Fatal("渠道 code 不可改")
	}
	if _, err := s.AdminUpdateTemplate(ctx, adm, t5, TemplateInput{Title: "蓝V第二个", Channels: json.RawMessage(`["douyin_9_16"]`), UpdatedAt: updOf(t, s, t5)}); err != nil {
		t.Fatal(err)
	}
	it, err := s.TemplateByID(ctx, t5)
	if err != nil || len(it.TargetChannels) != 1 || it.TargetChannels[0].Width != 1080 {
		t.Fatalf("targetChannels: %+v %v", it, err)
	}

	// ---- 偏好
	if _, err := s.PutPreference(ctx, 42, []int64{blue}, 0); err != nil {
		t.Fatal(err)
	}
	pref, _ := s.GetPreference(ctx, 42)
	if pref.Stale || pref.Primary.Code != "foot_bluev" {
		t.Fatalf("%+v", pref)
	}
	if _, err := s.PutPreference(ctx, 42, []int64{foot}, 0); err != nil { // 整体替换
		t.Fatal(err)
	}
	if pref, _ = s.GetPreference(ctx, 42); len(pref.IndustryIDs) != 1 || pref.Primary.Code != "foot" {
		t.Fatalf("整体替换: %+v", pref)
	}

	// ---- 最近用过：严格同行业 + 可见
	for i, row := range [][2]int64{{42, t1}, {42, t5}, {42, t1}, {42, t4}, {43, t5}} {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_agent_turn (user_id, conversation_id, turn_no, turn_id, run_id, status, plaza_template_id, create_time, update_time) "+
			"VALUES (?, 'c-it', ?, ?, ?, 'completed', ?, NOW(), NOW())", row[0], i+1, fmt.Sprintf("t-it-%d", i), fmt.Sprintf("r-it-%d", i), row[1]); err != nil {
			t.Fatal(err)
		}
	}
	rc, err := s.Recent(ctx, 42, "foot", "", "", 0)
	if err != nil || len(rc) != 3 {
		t.Fatalf("%+v %v", rc, err)
	}
	for _, r := range rc {
		if r.PlazaTemplateID == t1 && r.UseCount != 2 {
			t.Fatalf("useCount 去重累计: %+v", r)
		}
	}
	if rc, _ = s.Recent(ctx, 42, "foot", "foot_bluev", "", 0); len(rc) != 2 {
		t.Fatalf("二级范围: %+v", rc)
	}
	if rc, _ = s.Recent(ctx, 42, "foot", "", "video", 0); len(rc) != 1 || rc[0].PlazaTemplateID != t4 {
		t.Fatalf("mediaType 过滤: %+v", rc)
	}
	if rc, _ = s.Recent(ctx, 42, "spa", "", "", 0); len(rc) != 0 {
		t.Fatalf("别的行业不含: %+v", rc)
	}
	if _, err := s.Recent(ctx, 42, "", "", "", 0); codeOf(err) != xerr.ParamMissing {
		t.Fatal("recent 必填行业")
	}
	usage, _ := s.rep.Uses7d(ctx)
	if usage[t1] != 2 || usage[t5] != 2 {
		t.Fatalf("Uses7d: %v", usage)
	}

	// ---- 行业建议（真实库）：去重 / 限频 / 聚合 / 处理
	sidA, sidB := "aaaaaaaa11111111aaaaaaaa11111111", "bbbbbbbb22222222bbbbbbbb22222222"
	for _, in := range []SuggestionInput{
		{Text: "美甲", Sid: sidA, IPHash: "ipA"}, {Text: " 美 甲 ", Sid: sidA, IPHash: "ipA"}, // 同 sid 同文本同日去重
		{Text: "美甲", Sid: sidB, IPHash: "ipB", UserID: 5}, {Sid: sidB, IPHash: "ipB"}, // 无文本
	} {
		if err := s.SubmitSuggestion(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SubmitSuggestion(ctx, SuggestionInput{Text: "宠物店", Sid: sidA, IPHash: "ipA"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitSuggestion(ctx, SuggestionInput{Text: "民宿", Sid: sidA, IPHash: "ipA"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitSuggestion(ctx, SuggestionInput{Text: "火锅", Sid: sidA, IPHash: "ipA"}); reasonOf(err) != ReasonSuggestionRateLimited {
		t.Fatalf("同 sid 当天第 4 条被限频: %v", err)
	}
	sg, err := s.AdminSuggestions(ctx, SuggestionFilter{})
	if err != nil || sg.NoTextCount != 1 {
		t.Fatalf("%+v %v", sg, err)
	}
	var meijia *SuggestionAgg
	for i := range sg.Items {
		if sg.Items[i].NormText == "美甲" {
			meijia = &sg.Items[i]
		}
	}
	if meijia == nil || meijia.Count != 2 || meijia.Sids != 2 || meijia.Status != "NEW" || sg.Items[0].NormText != "美甲" {
		t.Fatalf("聚合：美甲 2 次 2 个 sid，按次数降序排首位: %+v", sg.Items)
	}
	if err := s.AdminHandleSuggestion(ctx, adm, "美甲", "ADOPTED", spa); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminHandleSuggestion(ctx, adm, "美甲", "ADOPTED", 0); codeOf(err) != xerr.ParamError {
		t.Fatal("采纳需要行业")
	}
	if sg, _ = s.AdminSuggestions(ctx, SuggestionFilter{Status: "ADOPTED"}); sg.Total != 1 {
		t.Fatalf("按状态筛: %+v", sg)
	}
	var raw int
	_ = s.db.NewSelect().Model((*model.IndustrySuggestion)(nil)).ColumnExpr("COUNT(*)").Where("text = ? OR text = ?", "13800138000", "a@b.com").Scan(ctx, &raw)
	if raw != 0 {
		t.Fatal("联系方式不入库")
	}

	// ---- 搜索缺口（无 Redis 路径：请求内直接 upsert；后台聚合与标记）
	for i := 0; i < 2; i++ {
		if _, err := s.Templates(ctx, ListParams{Industry: "foot", Keyword: "按摩套餐", Page: 1, Size: 10, IPHash: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := s.AdminSearchMisses(ctx, MissFilter{Industry: "foot"})
	if err != nil || ms.Total != 2 || ms.Items[0].Count != 2 || ms.Items[0].Status != "NEW" || ms.Items[0].NormTerm != "按摩套餐" {
		t.Fatalf("%+v %v", ms, err)
	}
	if err := s.AdminHandleSearchMiss(ctx, adm, "foot", "按摩套餐", "HANDLED"); err != nil {
		t.Fatal(err)
	}
	ms, _ = s.AdminSearchMisses(ctx, MissFilter{Status: "HANDLED", From: "2000-01-01 00:00:00", To: "2100-01-01 00:00:00"})
	if ms.Total != 1 || ms.Items[0].Count != 2 {
		t.Fatalf("区间 + 状态: %+v", ms)
	}
	if err := s.AdminHandleSearchMiss(ctx, adm, "foot", "不存在的词", "HANDLED"); codeOf(err) != xerr.NotFound {
		t.Fatal("词不存在")
	}

	// ---- 行业树 / 排序 / 删除迁移
	tree, err := s.AdminIndustryTree(ctx)
	if err != nil || len(tree) != 2 {
		t.Fatalf("%+v %v", tree, err)
	}
	var ft AdminIndustryNode
	for _, n := range tree {
		if n.ID == foot {
			ft = n
		}
	}
	if ft.TemplateCount.Total < 3 || len(ft.Children) != 1 || ft.Children[0].TemplateCount.Total != 2 || !ft.UserVisible || ft.UserCount < 1 || ft.Replicate7d < 4 {
		t.Fatalf("树节点统计: %+v", ft)
	}
	if err := s.AdminSortIndustries(ctx, adm, []SortItem{{ID: foot, SortOrder: 1}, {ID: spa, SortOrder: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminSortIndustries(ctx, adm, []SortItem{{ID: foot, ParentID: &spa, SortOrder: 1}}); reasonOf(err) != ReasonIndustryDepthExceeded {
		t.Fatalf("有子级的一级不能降为二级: %v", err)
	}
	if err := s.AdminDeleteIndustry(ctx, adm, foot, 0); reasonOf(err) != ReasonIndustryHasTemplates {
		t.Fatalf("有模板必须 reassignTo: %v", err)
	}
	if err := s.AdminDeleteIndustry(ctx, adm, foot, foot); codeOf(err) != xerr.ParamError {
		t.Fatal("迁移目标不能是自己")
	}
	if err := s.AdminDeleteIndustry(ctx, adm, foot, spa); err != nil {
		t.Fatal(err)
	}
	if pg, _ := s.Templates(ctx, ListParams{Industry: "spa", Page: 1, Size: 50}); pg.Total < 3 {
		t.Fatalf("模板（含 sort_order）与偏好迁移到 spa: %+v", pg)
	}
	if _, err := s.Templates(ctx, ListParams{Industry: "foot", Page: 1, Size: 10}); reasonOf(err) != ReasonIndustryNotFound {
		t.Fatalf("删除后该行业失效: %v", err)
	}
	if pref, _ = s.GetPreference(ctx, 42); pref.Stale || pref.Primary.Code != "spa" {
		t.Fatalf("偏好迁到目标: %+v", pref)
	}
	if kids, _ := s.AdminIndustryTree(ctx); len(kids) != 1 || len(kids[0].Children) != 1 || kids[0].Children[0].Code != "foot_bluev" {
		t.Fatalf("子级改挂到目标一级下: %+v", kids)
	}
	// code 可复用
	if _, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "foot", Name: sp("足浴新"), Hint: sp("h")}); err != nil {
		t.Fatalf("逻辑删除后 code 可复用: %v", err)
	}

	// ---- 审计：同事务、前后快照、diff、批量 batch_id
	au, err := s.AdminAudit(ctx, AuditFilter{ResourceType: ResTemplate, ResourceID: fmt.Sprint(t1), Page: 1, Size: 50})
	if err != nil || au.Total < 4 {
		t.Fatalf("%+v %v", au, err)
	}
	var sawUpdate bool
	for _, e := range au.Items {
		if e.Action == "TEMPLATE_UPDATE" && strings.Contains(string(e.Diff), `"title"`) && e.OperatorID != nil && *e.OperatorID == 7 {
			sawUpdate = true
		}
	}
	if !sawUpdate {
		t.Fatalf("UPDATE 审计应含字段 diff 与操作人: %+v", au.Items)
	}
	ab, _ := s.AdminAudit(ctx, AuditFilter{Action: "TEMPLATE_PUBLISH", Page: 1, Size: 50})
	var batched int
	for _, e := range ab.Items {
		if e.BatchID != nil && *e.BatchID == pubBatch {
			batched++
		}
	}
	if batched != 1 {
		t.Fatalf("批量各条共用 batch_id: %d", batched)
	}
	// 写开关关闭：模板写被拒，行业写不受影响
	s.cfg.AdminWriteEnabled = false
	if _, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "x"}); reasonOf(err) != ReasonWriteDisabled {
		t.Fatalf("写权属开关: %v", err)
	}
	if _, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "ok_still", Name: sp("仍可写"), Hint: sp("h")}); err != nil {
		t.Fatalf("行业写不受开关影响: %v", err)
	}
}

func intp(i int) *int { return &i }

// 入参 schema：后台保存校验与规整、详情下发、列表不下发（对真实库）。
func TestIntegrationSlotsRoundTrip(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	ind, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "slotind", Name: sp("入参行业"), Hint: sp("h")})
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"prompt":"p","result":["https://x.com/r.webp"]}`)
	if _, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "坏入参", Payload: payload,
		Slots: json.RawMessage(`[{"key":"A","type":"text","label":"x"}]`)}); codeOf(err) != xerr.ParamError || !strings.Contains(msgOf(err), "items[0].key") {
		t.Fatalf("保存时带路径拒收: %v", err)
	}
	id, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "足浴门头", Payload: payload, IndustryIDs: &[]int64{ind},
		Slots: json.RawMessage(footSchema)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.NewRaw("SELECT CAST(slots_json AS CHAR) FROM afv_compat_plaza_template WHERE id = ?", id).Scan(ctx, &stored); err != nil || !strings.Contains(stored, `"schemaVersion": 1`) && !strings.Contains(stored, `"schemaVersion":1`) {
		t.Fatalf("存库带 schemaVersion=1: %s %v", stored, err)
	}
	d, err := s.TemplateByID(ctx, id)
	if err != nil || d.Slots == nil || d.Slots.Items[0].Key != "storefront" {
		t.Fatalf("%+v %v", d, err)
	}
	pg, _ := s.Templates(ctx, ListParams{Industry: "slotind", Page: 1, Size: 10})
	if b, _ := json.Marshal(pg); strings.Contains(string(b), "storefront") {
		t.Fatalf("列表不下发: %s", b)
	}
	if _, err := s.PrepareInputs(ctx, id, in("shop_name", `"x"`)); err == nil {
		t.Fatal("缺必填")
	}
	// 清空 slots：[] → NULL
	if _, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "足浴门头", Slots: json.RawMessage(`[]`), UpdatedAt: updOf(t, s, id)}); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.TemplateByID(ctx, id); d.Slots != nil {
		t.Fatal("清空后无入参定义")
	}

	// 只改入参：PUT /templates/{id}/slots
	det, err := s.AdminGetTemplate(ctx, id)
	if err != nil || string(det.Slots) != "null" {
		t.Fatalf("无入参时后台回显 null: %s %v", det.Slots, err)
	}
	res, err := s.AdminUpdateTemplateSlots(ctx, adm, id, TemplateSlotsInput{Slots: json.RawMessage(footSchema), UpdatedAt: det.UpdateTime})
	if err != nil || !strings.Contains(string(res.Slots), `"items"`) || res.UpdatedAt == "" {
		t.Fatalf("只改入参: %+v %v", res, err)
	}
	if det, _ = s.AdminGetTemplate(ctx, id); !strings.Contains(string(det.Slots), `"storefront"`) || !strings.HasPrefix(string(det.Slots), `{"schemaVersion":1,"items":[`) {
		t.Fatalf("后台回显 {schemaVersion, items}: %s", det.Slots)
	}
	// 校验失败：1003 · SLOTS_INVALID，data.field 为路径
	_, err = s.AdminUpdateTemplateSlots(ctx, adm, id, TemplateSlotsInput{Slots: json.RawMessage(`{"schemaVersion":1,"items":[{"key":"a","type":"image","label":"A","required":true,"minCount":0}]}`), UpdatedAt: updOf(t, s, id)})
	if se := AsSlotsError(err); se == nil || se.Reason != ReasonSlotsInvalid || se.Field() != "items[0].minCount" || codeOf(err) != xerr.ParamError {
		t.Fatalf("入参校验失败: %v", err)
	}
	// 乐观并发：带旧的 updatedAt → 4002 · CONFLICT
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.AdminUpdateTemplateSlots(ctx, adm, id, TemplateSlotsInput{Slots: json.RawMessage(apparelSchema), UpdatedAt: det.UpdateTime}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminUpdateTemplateSlots(ctx, adm, id, TemplateSlotsInput{Slots: json.RawMessage(footSchema), UpdatedAt: det.UpdateTime}); codeOf(err) != xerr.Conflict || xerr.ReasonOf(err) != ReasonConflict {
		t.Fatalf("应冲突: %v", err)
	}
	// 整体编辑不带 slots = 保留原入参
	if _, err := s.AdminUpdateTemplateSlots(ctx, adm, id, TemplateSlotsInput{Slots: json.RawMessage(footSchema)}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("入参保存缺 updatedAt → PARAM_MISSING: %v", err)
	}
	if _, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "足浴门头 v2", UpdatedAt: updOf(t, s, id)}); err != nil {
		t.Fatal(err)
	}
	if det, _ = s.AdminGetTemplate(ctx, id); !strings.Contains(string(det.Slots), `"products"`) {
		t.Fatalf("整体编辑省略 slots 应保留: %s", det.Slots)
	}
	// 旧存储形态（slots 键）读取兼容并规整
	if _, err := s.db.NewRaw("UPDATE afv_compat_plaza_template SET slots_json = ? WHERE id = ?", `{"schemaVersion":1,"slots":[{"key":"a","type":"text","label":"A"}]}`, id).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if det, _ = s.AdminGetTemplate(ctx, id); !strings.HasPrefix(string(det.Slots), `{"schemaVersion":1,"items":[{"key":"a"`) {
		t.Fatalf("旧键规整回显: %s", det.Slots)
	}
	// 头条钩子：彻底删除时在同一事务里调用；列表 / 详情带 headlineBrief / headlineStatus
	var purged []int64
	s.SetHeadlineCleaner(func(_ context.Context, _ bun.IDB, tid int64) error { purged = append(purged, tid); return nil })
	s.SetHeadlineBrief(func(_ context.Context, ids []int64) (map[int64]any, error) {
		m := map[int64]any{}
		for _, x := range ids {
			m[x] = map[string]any{"onBoard": true}
		}
		return m, nil
	})
	s.SetHeadlineStatus(func(_ context.Context, x int64) (any, error) { return map[string]any{"id": x}, nil })
	if det, _ = s.AdminGetTemplate(ctx, id); det.HeadlineBrief == nil || det.HeadlineStatus == nil {
		t.Fatalf("头条摘要 / 联动: %+v %+v", det.HeadlineBrief, det.HeadlineStatus)
	}
	if err := s.AdminDeleteTemplate(ctx, adm, id); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminPurgeTemplate(ctx, adm, id); err != nil || len(purged) != 1 || purged[0] != id {
		t.Fatalf("彻底删除调用头条清理: %v %v", purged, err)
	}
}

// updOf 读模板当前 updateTime（编辑接口 updatedAt 必填）。
func updOf(t *testing.T, s *Service, id int64) string {
	t.Helper()
	d, err := s.AdminGetTemplate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.UpdateTime
}

// L15 + M3：整体 PUT 换 videoUrl 重新排队 PROCESSING；revision 每次写 +1，条件更新判冲突；Java 写权属期间用户端要求 enabled=1。
func TestIntegrationRevisionMediaStatusAndEnabledGate(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()
	foot, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "revind", Name: sp("版本行业"), Hint: sp("hint")})
	foot = mustID(t, foot, err)
	payload := json.RawMessage(`{"result":["https://x.example.com/a.webp"],"prompt":"p"}`)
	id, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "视频", MediaType: "video", VideoURL: "https://v.example.com/a.mp4", Payload: payload, IndustryIDs: &[]int64{foot}})
	id = mustID(t, id, err)
	if _, err := s.db.ExecContext(ctx, "UPDATE afv_compat_plaza_template SET media_status = 'FAILED' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	det, _ := s.AdminGetTemplate(ctx, id)
	rev0 := det.Revision
	// 同一个视频地址：状态不动
	wr, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "视频", MediaType: "video", VideoURL: "https://v.example.com/a.mp4", UpdatedAt: det.UpdateTime})
	if err != nil || wr.Revision != rev0+1 {
		t.Fatalf("%+v %v", wr, err)
	}
	if det, _ = s.AdminGetTemplate(ctx, id); det.MediaStatus != model.MediaStatusFailed {
		t.Fatalf("没换视频不应重置: %s", det.MediaStatus)
	}
	// 换视频：旧失败作废，重新排队 PROCESSING（由后台任务处理）；用返回的 revision 继续保存
	r1 := wr.Revision
	wr, err = s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "视频", MediaType: "video", VideoURL: "https://v.example.com/b.mp4", UpdatedAt: wr.UpdatedAt, Revision: &r1})
	if err != nil || wr.Revision != r1+1 {
		t.Fatalf("%+v %v", wr, err)
	}
	if det, _ = s.AdminGetTemplate(ctx, id); det.MediaStatus != model.MediaStatusProcessing {
		t.Fatalf("换视频应重新排队为 PROCESSING: %s", det.MediaStatus)
	}
	// 只改入参接口同样按 revision
	if _, err := s.AdminUpdateTemplateSlots(ctx, adm, id, TemplateSlotsInput{Slots: json.RawMessage(`null`), UpdatedAt: wr.UpdatedAt, Revision: &r1}); xerr.ReasonOf(err) != ReasonConflict {
		t.Fatalf("旧 revision → CONFLICT: %v", err)
	}
	cur := wr.Revision
	sr, err := s.AdminUpdateTemplateSlots(ctx, adm, id, TemplateSlotsInput{Slots: json.RawMessage(`null`), UpdatedAt: wr.UpdatedAt, Revision: &cur})
	if err != nil || sr.Revision != cur+1 {
		t.Fatalf("%+v %v", sr, err)
	}
	// 视频换地址后在 PROCESSING，处理完（这里补一个封面让任务能放行）才能发布；发布后可见；模拟 Java 停用（只改 enabled）：Java 写权属口径下不可见
	if _, err := s.db.ExecContext(ctx, "UPDATE afv_compat_plaza_template SET poster_url = 'https://v.example.com/b.jpg' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunMediaJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{}); err != nil {
		t.Fatal(err)
	}
	if pg, _ := s.Templates(ctx, ListParams{Industry: "revind", Page: 1, Size: 10}); pg.Total != 1 {
		t.Fatalf("发布后可见: %+v", pg)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE afv_compat_plaza_template SET enabled = 0 WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg
	cfg.AdminWriteEnabled = false
	javaOwned := New(s.db, nil, cfg)
	if pg, _ := javaOwned.Templates(ctx, ListParams{Industry: "revind", Page: 1, Size: 10}); pg == nil || pg.Total != 0 {
		t.Fatalf("Java 写权属期间 enabled=0 不可见: %+v", pg)
	}
	if _, err := javaOwned.TemplateByID(ctx, id); codeOf(err) != xerr.NotFound {
		t.Fatalf("按 id 也不可见: %v", err)
	}
	// 边界任务不会替 Java 把无定时的 enabled=0 行翻回 1（H2）
	if n, err := s.SyncLifecycle(ctx); err != nil || n != 0 {
		t.Fatalf("无定时行不上翻: %d %v", n, err)
	}
}
