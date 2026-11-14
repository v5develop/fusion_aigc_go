package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/xerr"
)

// 交接期（AdminWriteEnabled=false）：A 类写放行且不碰 Java 也写的列；B 类写仍拒。需要 PLAZA_TEST_DSN（临时库）。

const frozenUpdate = "2020-01-01 00:00:00"

// bColumns 取 Java 也写的列（含 update_time 与 enabled 派生的 state / online_at / offline_at / deleted_at / deleted_by）的整行文本。
func bColumns(t *testing.T, s *Service, id int64) string {
	t.Helper()
	var out string
	err := s.db.NewRaw("SELECT CONCAT_WS('|', id, IFNULL(category,'∅'), title, IFNULL(description,'∅'), IFNULL(CAST(tags_json AS CHAR),'∅'), "+
		"CAST(data_json AS CHAR), sort_order, enabled, create_time, update_time, deleted, state, IFNULL(online_at,'∅'), IFNULL(offline_at,'∅'), "+
		"IFNULL(deleted_at,'∅'), IFNULL(deleted_by,'∅'), IFNULL(created_by,'∅')) FROM afv_compat_plaza_template WHERE id = ?", id).Scan(context.Background(), &out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func revOf(t *testing.T, s *Service, id int64) int {
	t.Helper()
	d, err := s.AdminGetTemplate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Revision
}

func auditCount(t *testing.T, s *Service, action string) int {
	t.Helper()
	n, err := s.db.NewSelect().Model((*model.AuditLog)(nil)).Where("action = ?", action).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestIntegrationHandoverWrites(t *testing.T) {
	full := integrationService(t)
	ctx := context.Background()
	cfg := full.cfg
	cfg.AdminWriteEnabled = false
	ho := New(full.db, nil, cfg) // 交接期服务（与 full 共用同一个库）

	// 行业 / 渠道在交接期也能建（Go 独占表）
	foot, err := ho.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "ho_foot", Name: sp("足浴"), Hint: sp("门店")})
	foot = mustID(t, foot, err)
	spa, err := ho.AdminCreateIndustry(ctx, adm, IndustryInput{Code: "ho_spa", Name: sp("SPA"), Hint: sp("养生")})
	spa = mustID(t, spa, err)
	if _, err := ho.AdminCreateChannel(ctx, adm, ChannelInput{Code: "douyin_9_16", Name: sp("抖音"), Width: intp(1080), Height: intp(1920), Ratio: sp("9:16")}); err != nil {
		t.Fatal(err)
	}

	// 「Java 写的」模板：只填 Java 的列（Go 列走默认：state=PUBLISHED、media_type=image、revision=0）
	javaRow := func(title string, enabled int) int64 {
		res, err := ho.db.ExecContext(ctx, "INSERT INTO afv_compat_plaza_template (title, category, description, tags_json, data_json, sort_order, enabled) VALUES (?, '足浴', 'd', ?, ?, 5, ?)",
			title, `{"tags":["a"]}`, `{"result":["https://x.example.com/a.webp"],"prompt":"p"}`, enabled)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := ho.db.ExecContext(ctx, "UPDATE afv_compat_plaza_template SET update_time = ? WHERE id = ?", frozenUpdate, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	j1, j2, j0 := javaRow("Java一", 1), javaRow("Java二", 1), javaRow("Java停用", 0)

	// ---- 未分配清单与计数
	sum, err := ho.AdminUnassignedSummary(ctx)
	if err != nil || sum.Total != 3 || sum.Enabled != 2 || sum.AdminWriteEnabled {
		t.Fatalf("未分配计数: %+v %v", sum, err)
	}
	pg, err := ho.AdminListTemplates(ctx, TemplateFilter{Unassigned: true})
	if err != nil || pg.Total != 3 || pg.UnassignedCount != 3 {
		t.Fatalf("未分配清单: %+v %v", pg, err)
	}
	if v, _ := ho.Templates(ctx, ListParams{Industry: "ho_foot", Page: 1, Size: 10}); v != nil && v.Total != 0 {
		t.Fatalf("未挂行业不可见: %+v", v)
	}

	// ---- B 类写：全部拒（ADMIN_WRITE_DISABLED），且不动行
	before := bColumns(t, ho, j1)
	rejected := map[string]error{}
	_, rejected["create"] = ho.AdminCreateTemplate(ctx, adm, TemplateInput{Title: "x"})
	_, rejected["update"] = ho.AdminUpdateTemplate(ctx, adm, j1, TemplateInput{Title: "x", UpdatedAt: frozenUpdate})
	_, rejected["duplicate"] = ho.AdminDuplicateTemplate(ctx, adm, j1)
	_, rejected["publish"] = ho.AdminPublishTemplate(ctx, adm, j1, PublishInput{})
	rejected["offline"] = ho.AdminOfflineTemplate(ctx, adm, j1)
	rejected["delete"] = ho.AdminDeleteTemplate(ctx, adm, j1)
	rejected["restore"] = ho.AdminRestoreTemplate(ctx, adm, j1)
	rejected["purge"] = ho.AdminPurgeTemplate(ctx, adm, j1)
	for _, a := range []string{"publish", "offline", "delete", "restore"} {
		_, rejected["batch-"+a] = ho.AdminBatch(ctx, adm, BatchInput{Action: a, IDs: []int64{j1}})
	}
	for name, err := range rejected {
		if reasonOf(err) != ReasonWriteDisabled {
			t.Errorf("B 类 %s 交接期应拒 ADMIN_WRITE_DISABLED: %v", name, err)
		}
	}
	if got := bColumns(t, ho, j1); got != before {
		t.Fatalf("被拒的 B 类写不得改行:\n%s\n%s", before, got)
	}

	// ---- A 类：单个模板挂 / 摘行业（增量）
	r0 := revOf(t, ho, j1)
	res, err := ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{Assign: []int64{foot, spa}})
	if err != nil || !res.Changed || len(res.IndustryIDs) != 2 || res.Revision != r0+1 {
		t.Fatalf("assign: %+v %v", res, err)
	}
	if got := bColumns(t, ho, j1); got != before {
		t.Fatalf("A 类挂行业改了 B 类列（含 update_time）:\n%s\n%s", before, got)
	}
	// 同样的 assign 再来一次：幂等，不 bump、不记审计
	n := auditCount(t, ho, "TEMPLATE_SET_INDUSTRIES")
	if res, err = ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{Assign: []int64{foot}}); err != nil || res.Changed || res.Revision != r0+1 {
		t.Fatalf("幂等: %+v %v", res, err)
	}
	if auditCount(t, ho, "TEMPLATE_SET_INDUSTRIES") != n {
		t.Fatal("没变化不应记审计")
	}
	// 交接期挂上行业后无需 Java 改动即对用户可见（enabled=1 ∧ 有行业）；enabled=0 的仍不可见
	if v, err := ho.Templates(ctx, ListParams{Industry: "ho_foot", Page: 1, Size: 10}); err != nil || v.Total != 1 {
		t.Fatalf("挂行业后可见: %+v %v", v, err)
	}
	if _, err := ho.AdminSetTemplateIndustries(ctx, adm, j0, TemplateIndustriesInput{Assign: []int64{foot}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := ho.Templates(ctx, ListParams{Industry: "ho_foot", Page: 1, Size: 10}); v.Total != 1 {
		t.Fatalf("enabled=0 的模板交接期仍不可见（H1）: %+v", v)
	}
	// 摘行业 + 整体替换（revision 必填、冲突 4002）
	if res, err = ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{Unassign: []int64{spa}}); err != nil || len(res.IndustryIDs) != 1 {
		t.Fatalf("unassign: %+v %v", res, err)
	}
	if _, err = ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{IndustryIDs: &[]int64{spa}}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("整体替换缺 revision: %v", err)
	}
	stale := res.Revision - 1
	if _, err = ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{IndustryIDs: &[]int64{spa}, Revision: &stale}); reasonOf(err) != ReasonConflict {
		t.Fatalf("旧 revision → CONFLICT: %v", err)
	}
	cur := res.Revision
	if res, err = ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{IndustryIDs: &[]int64{spa}, Revision: &cur}); err != nil || len(res.IndustryIDs) != 1 || res.IndustryIDs[0] != spa {
		t.Fatalf("replace: %+v %v", res, err)
	}
	if _, err = ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{Assign: []int64{999999}}); reasonOf(err) != ReasonIndustryNotFound {
		t.Fatalf("行业不存在: %v", err)
	}
	if _, err = ho.AdminSetTemplateIndustries(ctx, adm, j1, TemplateIndustriesInput{IndustryIDs: &[]int64{spa}, Assign: []int64{foot}, Revision: &cur}); codeOf(err) != xerr.ParamError {
		t.Fatalf("replace 与 assign 互斥: %v", err)
	}
	if _, err = ho.AdminSetTemplateIndustries(ctx, adm, 424242, TemplateIndustriesInput{Assign: []int64{foot}}); reasonOf(err) != ReasonTemplateNotFound {
		t.Fatalf("模板不存在: %v", err)
	}

	// ---- A 类：批量挂 / 摘 / 替换 / 置顶（只写关系表，模板行整行不变）
	b2 := bColumns(t, ho, j2)
	for _, a := range []string{"assign", "unassign", "replaceIndustries", "setOrder"} {
		br, err := ho.AdminBatch(ctx, adm, BatchInput{Action: a, IDs: []int64{j1, j2}, IndustryIDs: []int64{foot}})
		if err != nil || len(br.Failed) != 0 {
			t.Fatalf("batch %s: %+v %v", a, br, err)
		}
	}
	if got := bColumns(t, ho, j2); got != b2 {
		t.Fatalf("批量行业动作改了 B 类列:\n%s\n%s", b2, got)
	}

	// ---- A 类：行业内排序
	if err := ho.AdminSetIndustryOrder(ctx, adm, foot, []int64{j2, j1}); err != nil {
		t.Fatalf("行业内排序: %v", err)
	}
	if got := bColumns(t, ho, j2); got != b2 {
		t.Fatal("行业内排序改了模板行")
	}

	// ---- A 类：slots（交接期 revision 必填；update_time 冻结）
	cur = revOf(t, ho, j2)
	if _, err = ho.AdminUpdateTemplateSlots(ctx, adm, j2, TemplateSlotsInput{Slots: json.RawMessage(footSchema), UpdatedAt: frozenUpdate}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("交接期缺 revision: %v", err)
	}
	stale = cur - 1
	if _, err = ho.AdminUpdateTemplateSlots(ctx, adm, j2, TemplateSlotsInput{Slots: json.RawMessage(footSchema), Revision: &stale}); reasonOf(err) != ReasonConflict {
		t.Fatalf("slots 旧 revision: %v", err)
	}
	sr, err := ho.AdminUpdateTemplateSlots(ctx, adm, j2, TemplateSlotsInput{Slots: json.RawMessage(footSchema), Revision: &cur})
	if err != nil || sr.Revision != cur+1 || sr.UpdatedAt != "2020-01-01T00:00:00" {
		t.Fatalf("slots: %+v %v", sr, err)
	}
	if got := bColumns(t, ho, j2); got != b2 {
		t.Fatalf("slots 改了 B 类列（含 update_time）:\n%s\n%s", b2, got)
	}
	if _, err = ho.AdminUpdateTemplateSlots(ctx, adm, j2, TemplateSlotsInput{Slots: json.RawMessage(footSchema), Revision: &cur}); reasonOf(err) != ReasonConflict {
		t.Fatalf("重放旧 revision: %v", err)
	}

	// ---- A 类：渠道 / 媒体类型 / 需要产品图
	cur = sr.Revision
	if _, err = ho.AdminSetTemplateAttributes(ctx, adm, j2, TemplateAttributesInput{MediaType: sp("video")}); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("attributes 缺 revision: %v", err)
	}
	if _, err = ho.AdminSetTemplateAttributes(ctx, adm, j2, TemplateAttributesInput{Revision: &cur}); codeOf(err) != xerr.ParamError {
		t.Fatalf("没有要改的属性: %v", err)
	}
	if _, err = ho.AdminSetTemplateAttributes(ctx, adm, j2, TemplateAttributesInput{Revision: &cur, MediaType: sp("video")}); codeOf(err) != xerr.ParamError {
		t.Fatalf("视频需要 videoUrl: %v", err)
	}
	if _, err = ho.AdminSetTemplateAttributes(ctx, adm, j2, TemplateAttributesInput{Revision: &cur, Channels: json.RawMessage(`["ghost"]`)}); codeOf(err) != xerr.ParamError {
		t.Fatalf("渠道不存在: %v", err)
	}
	if got := bColumns(t, ho, j2); got != b2 {
		t.Fatal("被拒的 attributes 不得改行")
	}
	wr, err := ho.AdminSetTemplateAttributes(ctx, adm, j2, TemplateAttributesInput{Revision: &cur, MediaType: sp("video"), VideoURL: sp("https://v.example.com/a.mp4"),
		Channels: json.RawMessage(`["douyin_9_16"]`), NeedsProductImage: bp(false)})
	if err != nil || wr.Revision != cur+1 || wr.UpdatedAt != "2020-01-01T00:00:00" {
		t.Fatalf("attributes: %+v %v", wr, err)
	}
	if got := bColumns(t, ho, j2); got != b2 {
		t.Fatalf("attributes 改了 B 类列（含 update_time）:\n%s\n%s", b2, got)
	}
	det, _ := ho.AdminGetTemplate(ctx, j2)
	if det.MediaType != "video" || det.NeedsProductImage {
		t.Fatalf("A 类列应已写入: %+v", det)
	}
	if _, err = ho.AdminSetTemplateAttributes(ctx, adm, j2, TemplateAttributesInput{Revision: &cur, MediaType: sp("image")}); reasonOf(err) != ReasonConflict {
		t.Fatalf("attributes 旧 revision: %v", err)
	}
	cur = wr.Revision
	if _, err = ho.AdminSetTemplateAttributes(ctx, adm, j2, TemplateAttributesInput{Revision: &cur, MediaType: sp("image"), Channels: json.RawMessage(`null`)}); err != nil {
		t.Fatal(err)
	}
	if det, _ = ho.AdminGetTemplate(ctx, j2); det.MediaType != "image" || det.VideoURL != nil {
		t.Fatalf("改回 image 清 videoUrl: %+v", det)
	}

	// ---- A 类：示例素材（Go 独占表，交接期放行）
	sid, err := ho.AdminCreateSample(ctx, adm, SampleInput{ScopeType: model.SampleScopeTemplate, TemplateID: j2, URL: sp("https://s.example.com/1.webp"), Title: sp("示例")})
	if err != nil {
		t.Fatal(err)
	}
	if ls, _ := ho.AdminListSamples(ctx, model.SampleScopeTemplate, 0, j2); len(ls) != 1 || ls[0].Role != model.SampleRoleOutput {
		t.Fatalf("role 缺省 OUTPUT: %+v", ls)
	}
	if err := ho.AdminUpdateSample(ctx, adm, sid, SampleInput{Title: sp("示例2"), Role: sp("INPUT")}); err != nil {
		t.Fatal(err)
	}
	if ls, _ := ho.AdminListSamples(ctx, model.SampleScopeTemplate, 0, j2); ls[0].Role != model.SampleRoleInput || ls[0].Title != "示例2" {
		t.Fatalf("role 更新: %+v", ls)
	}
	if err := ho.AdminUpdateSample(ctx, adm, sid, SampleInput{Role: sp("X")}); codeOf(err) != xerr.ParamError {
		t.Fatalf("role 非法: %v", err)
	}
	if err := ho.AdminDeleteSample(ctx, adm, sid); err != nil {
		t.Fatal(err)
	}
	if got := bColumns(t, ho, j2); got != b2 {
		t.Fatal("示例素材写不得动模板行")
	}

	// ---- 能力探测
	if c := ho.AdminCapabilities(); c.AdminWriteEnabled || c.Writable.Status || c.Writable.Recycle || c.Writable.FullEdit ||
		!c.Writable.Slots || !c.Writable.IndustryRelation || !c.Writable.IndustryOrder || !c.Writable.Attrs || !c.Writable.Samples {
		t.Fatalf("交接期能力: %+v", c)
	}
	if c := full.AdminCapabilities(); !c.AdminWriteEnabled || !c.Writable.Status || !c.Writable.FullEdit {
		t.Fatalf("完整模式能力: %+v", c)
	}

	// ---- 全程都落审计
	for _, a := range []string{"TEMPLATE_SET_INDUSTRIES", "TEMPLATE_UPDATE_SLOTS", "TEMPLATE_UPDATE_ATTRS", "BATCH_ASSIGN", "BATCH_SETORDER", "INDUSTRY_TEMPLATE_ORDER", "SAMPLE_CREATE", "SAMPLE_UPDATE", "SAMPLE_DELETE"} {
		if auditCount(t, ho, a) == 0 {
			t.Errorf("缺审计 %s", a)
		}
	}

	// ---- 对照：完整模式（写权属在 Go）下 A 类写会推进 update_time，证明上面的「冻结」断言确实有区分力
	c := revOf(t, full, j1)
	if _, err := full.AdminUpdateTemplateSlots(ctx, adm, j1, TemplateSlotsInput{Slots: json.RawMessage(footSchema), UpdatedAt: frozenUpdate, Revision: &c}); err != nil {
		t.Fatal(err)
	}
	if got := bColumns(t, full, j1); got == before {
		t.Fatalf("对照：完整模式下 update_time 应推进 %s", fmt.Sprint(got))
	}

	// ---- 完整模式下 B 类写恢复可用
	if _, err := full.AdminPublishTemplate(ctx, adm, j1, PublishInput{}); err != nil {
		t.Fatalf("完整模式发布: %v", err)
	}
}
