package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/xerr"
)

// 模板编辑器全流程（需 PLAZA_TEST_DSN 指向临时库）：技能下拉、整体编辑的字段保留 / 校验 / revision 冲突、
// 视频封面缺失（mediaStatus=PENDING）挡发布、变更历史带字段级 diff。
func TestIntegrationTemplateEditorFlow(t *testing.T) {
	s := integrationService(t)
	ctx := context.Background()

	// 技能表：本测试需要完整列，临时换成完整表，结束后还原成共享 stub，避免影响其它集成测试。
	for _, q := range []string{
		"DROP TABLE IF EXISTS afv_compat_agent_skill",
		"CREATE TABLE afv_compat_agent_skill (id BIGINT AUTO_INCREMENT PRIMARY KEY, agent_skill_id BIGINT NOT NULL, agent_skill_code VARCHAR(128) NOT NULL, title VARCHAR(128) NOT NULL, description VARCHAR(512) NULL, icon VARCHAR(512) NULL, category VARCHAR(64) NULL, weight INT NOT NULL DEFAULT 0, enabled TINYINT NOT NULL DEFAULT 1, config JSON NULL, deleted TINYINT NOT NULL DEFAULT 0)",
		"INSERT INTO afv_compat_agent_skill (agent_skill_id, agent_skill_code, title, category, weight, enabled, config) VALUES (101,'poster','海报技能','设计',5,1,'{\"big\":1}'),(102,'video','视频技能','设计',9,1,NULL),(103,'old','停用技能',NULL,0,0,NULL)",
		"INSERT INTO afv_compat_agent_skill (agent_skill_id, agent_skill_code, title, deleted) VALUES (104,'gone','已删除技能',1)",
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.db.ExecContext(ctx, "DROP TABLE IF EXISTS afv_compat_agent_skill")
		_, _ = s.db.ExecContext(ctx, skillStubDDL)
	})

	skills, err := s.AdminListSkills(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 3 || skills[0].AgentSkillID != "102" || skills[1].AgentSkillID != "101" || skills[2].AgentSkillID != "103" || skills[2].Enabled {
		t.Fatalf("技能列表（分类 → 权重大在前，停用带 enabled=false，已删除不出现）: %+v", skills)
	}
	if b, _ := json.Marshal(skills[0]); string(b) == "" || containsKey(b, "config") {
		t.Fatalf("技能项不应带 config: %s", b)
	}
	if got, _ := s.AdminListSkills(ctx, "海报"); len(got) != 1 || got[0].AgentSkillID != "101" {
		t.Fatalf("关键词过滤: %+v", got)
	}

	foot, err := s.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "editor_ind", Name: sp("编辑器行业"), Hint: sp("h")})
	foot = mustID(t, foot, err)

	// 新建：视频 → PROCESSING（后台任务抽封面 / 转码）；本测试没有存储 / 媒体能力，任务会因「无封面且没有抽帧能力」永久失败
	payload := json.RawMessage(`{"result":["https://x.example.com/a.webp"],"image_urls":["https://x.example.com/r.webp"],"guidance":"请上传产品图","prompt":"P1","description":"d","custom":{"k":[1,2]}}`)
	skill := "101"
	id, err := s.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "编辑器模板", Description: "d", MediaType: "video", VideoURL: "https://x.example.com/v.mp4",
		MediaStatus: "PENDING", Payload: payload, AgentSkillID: &skill, IndustryIDs: &[]int64{foot}})
	id = mustID(t, id, err)
	det, err := s.AdminGetTemplate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if det.MediaStatus != model.MediaStatusProcessing {
		t.Fatalf("新建视频应 PROCESSING: %s", det.MediaStatus)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{}); err == nil {
		t.Fatal("PROCESSING 视频不应能发布")
	}
	if n, err := s.RunMediaJobs(ctx); err != nil || n != 1 {
		t.Fatalf("任务应处理 1 行: n=%d err=%v", n, err)
	}
	if det, _ = s.AdminGetTemplate(ctx, id); det.MediaStatus != model.MediaStatusFailed || det.MediaError == nil || det.MediaAttempts != 1 {
		t.Fatalf("没有抽帧能力应永久失败并带原因: %+v", det)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{}); err == nil {
		t.Fatal("FAILED 视频不应能发布")
	}
	// 运营补封面：整体编辑 → 重新排队 → 任务放行；payload 未知键与元数据键保留
	poster := "https://x.example.com/p.jpg"
	wr, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "编辑器模板", Description: "d", MediaType: "video", VideoURL: "https://x.example.com/v.mp4",
		PosterURL: poster, Payload: payload, UpdatedAt: det.UpdateTime, Revision: &det.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.AdminGetTemplate(ctx, id); d.MediaStatus != model.MediaStatusProcessing || d.MediaError != nil || d.MediaAttempts != 0 {
		t.Fatalf("补封面应重新排队并清空失败状态: %+v", d)
	}
	if n, err := s.RunMediaJobs(ctx); err != nil || n != 1 {
		t.Fatalf("任务应处理 1 行: n=%d err=%v", n, err)
	}
	det2, _ := s.AdminGetTemplate(ctx, id)
	if det2.MediaStatus != model.MediaStatusReady || det2.AgentSkillID == nil || *det2.AgentSkillID != "101" {
		t.Fatalf("补封面后 READY 且技能绑定保留（省略 = 不改）: %+v", det2)
	}
	if !containsKey(det2.Payload, "custom") || !containsKey(det2.Payload, "prompt") {
		t.Fatalf("payload 未知键 / prompt 应保留: %s", det2.Payload)
	}
	if _, err := s.AdminPublishTemplate(ctx, adm, id, PublishInput{}); err != nil {
		t.Fatalf("补封面后应能发布: %v", err)
	}

	// 并发：旧 revision → TEMPLATE_CONFLICT
	old := det.Revision
	if _, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "x", UpdatedAt: wr.UpdatedAt, Revision: &old}); xerr.ReasonOf(err) != ReasonTemplateConflict {
		t.Fatalf("旧 revision 应冲突: %v", err)
	}
	// 校验：不存在的技能 / 非法图片地址
	bad := "999999"
	cur := wr.Revision + 1 // 发布 bump 过一次
	if d, _ := s.AdminGetTemplate(ctx, id); d != nil {
		cur = d.Revision
	}
	if _, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "x", UpdatedAt: wr.UpdatedAt, Revision: &cur, AgentSkillID: &bad}); codeOf(err) != xerr.NotFound {
		t.Fatalf("不存在的技能: %v", err)
	}
	if _, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "x", UpdatedAt: wr.UpdatedAt, Revision: &cur, Payload: json.RawMessage(`{"result":["not-a-url"]}`)}); codeOf(err) != xerr.ParamError {
		t.Fatalf("非法图片地址应 1003: %v", err)
	}

	// 变更历史：按模板分页，更新带字段级 diff（提示词变更不丢）
	pl2 := json.RawMessage(`{"result":["https://x.example.com/a.webp"],"prompt":"P2","custom":{"k":[1,2]}}`)
	d3, _ := s.AdminGetTemplate(ctx, id)
	if _, err := s.AdminUpdateTemplate(ctx, adm, id, TemplateInput{Title: "改名", MediaType: "video", VideoURL: "https://x.example.com/v.mp4", PosterURL: poster, Payload: pl2, UpdatedAt: d3.UpdateTime, Revision: &d3.Revision}); err != nil {
		t.Fatal(err)
	}
	h, err := s.AdminAudit(ctx, AuditFilter{ResourceType: ResTemplate, ResourceID: fmt.Sprint(id), Page: 1, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if h.Total < 3 || h.Items[0].Action != "TEMPLATE_UPDATE" {
		t.Fatalf("历史应含创建 / 编辑 / 发布: %+v", h)
	}
	var diff map[string]map[string]any
	if err := json.Unmarshal(h.Items[0].Diff, &diff); err != nil {
		t.Fatal(err)
	}
	if diff["title"]["to"] != "改名" || diff["payload"] == nil {
		t.Fatalf("diff 应含 title / payload 的 from-to: %s", h.Items[0].Diff)
	}
	if !containsKey(h.Items[0].After, "payload") {
		t.Fatalf("after 快照保留全文: %s", h.Items[0].After)
	}
}

func containsKey(b []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
