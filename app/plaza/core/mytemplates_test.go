package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/xerr"
)

func sPtr(s string) *string { return &s }
func iPtr(i int) *int       { return &i }
func inputsOf(s string) *map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return &m
}

// ---- 纯逻辑（不需要库）

func TestInputKeyOf(t *testing.T) {
	for in, want := range map[string]string{
		"inputs.photo": "photo", "inputs.photo[0]": "photo", "inputs.a.b": "a", "inputs": "", "items[0].key": "",
	} {
		if got := inputKeyOf(in); got != want {
			t.Errorf("inputKeyOf(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMyTemplateFieldValidation(t *testing.T) {
	for _, bad := range []string{"", "   ", "\x00\x01", strings.Repeat("长", 41)} {
		if _, err := cleanName(bad); err == nil {
			t.Errorf("name %q 应被拒", bad)
		}
	}
	if n, err := cleanName("  足浴 · 蓝V号 · 11-13 "); err != nil || n != "足浴 · 蓝V号 · 11-13" {
		t.Fatalf("合法名: %q %v", n, err)
	}
	if n, err := cleanName(strings.Repeat("长", 40)); err != nil || n == "" {
		t.Fatal("40 字合法")
	}
	for _, v := range []int{0, -1, 5} {
		if checkVariants(v) == nil {
			t.Errorf("variants %d 应被拒", v)
		}
	}
	for _, v := range []int{1, 4} {
		if err := checkVariants(v); err != nil {
			t.Errorf("variants %d 合法: %v", v, err)
		}
	}
	if _, err := cleanMyText(strings.Repeat("字", 501)); err == nil {
		t.Fatal("text 超长应被拒")
	}
}

func TestSplitStaleInputs(t *testing.T) {
	schema, err := ParseSlotSchema([]byte(userLibSchema))
	if err != nil {
		t.Fatal(err)
	}
	// 全部有效
	valid, stale := splitStaleInputs(schema, true, map[string]json.RawMessage{"shop_name": raw(`"小店"`), "style": raw(`"a"`)})
	if len(stale) != 0 || valid["shop_name"] != "小店" || valid["style"] != "a" {
		t.Fatalf("全有效: %v %v", valid, stale)
	}
	// 选项已不存在 + 键已不存在 + 超长：这些失效，其余保留
	valid, stale = splitStaleInputs(schema, true, map[string]json.RawMessage{
		"shop_name": raw(`"小店"`), "style": raw(`"gone"`), "removed_slot": raw(`1`)})
	if !reflect.DeepEqual(stale, []string{"removed_slot", "style"}) || len(valid) != 1 || valid["shop_name"] != "小店" {
		t.Fatalf("部分失效: %v %v", valid, stale)
	}
	valid, stale = splitStaleInputs(schema, true, map[string]json.RawMessage{"shop_name": raw(`"这个店名实在是太长太长太长了"`)})
	if !reflect.DeepEqual(stale, []string{"shop_name"}) || len(valid) != 0 {
		t.Fatalf("超长: %v %v", valid, stale)
	}
	// 新增必填项没存值：不算失效，已存的照常给出
	valid, stale = splitStaleInputs(schema, true, map[string]json.RawMessage{"style": raw(`"b"`)})
	if len(stale) != 0 || valid["style"] != "b" {
		t.Fatalf("缺必填不算失效: %v %v", valid, stale)
	}
	// schema 脏：全部失效
	valid, stale = splitStaleInputs(nil, false, map[string]json.RawMessage{"a": raw(`1`), "b": raw(`2`)})
	if !reflect.DeepEqual(stale, []string{"a", "b"}) || len(valid) != 0 {
		t.Fatalf("schema 脏: %v %v", valid, stale)
	}
	// 模板已没有任何入参定义：已存的全失效
	empty, _ := ParseSlotSchema(nil)
	valid, stale = splitStaleInputs(empty, true, map[string]json.RawMessage{"shop_name": raw(`"x"`)})
	if !reflect.DeepEqual(stale, []string{"shop_name"}) || len(valid) != 0 {
		t.Fatalf("无 schema: %v %v", valid, stale)
	}
}

func TestMyTemplateListNeedsIndustryAndLogin(t *testing.T) {
	s := newTestService(stdFixture())
	if _, err := s.ListMyTemplates(context.Background(), 1, " ", ""); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("缺 industry: %v", err)
	}
	if _, err := s.ListMyTemplates(context.Background(), 0, "foot_bath", ""); codeOf(err) != xerr.Unauthorized {
		t.Fatalf("未登录: %v", err)
	}
	if _, err := s.ListFavorites(context.Background(), 1, "", ""); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("收藏缺 industry: %v", err)
	}
	if _, err := s.Nodes(context.Background(), "", ""); codeOf(err) != xerr.ParamMissing {
		t.Fatalf("节点缺 industry: %v", err)
	}
	if _, err := s.Nodes(context.Background(), "foot_bath", ""); reasonOf(err) != ReasonNotReady { // 无库
		t.Fatalf("无库: %v", err)
	}
}

// ---- 真库

func TestUserLibMyTemplatesCRUD(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	it, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{
		Name: sPtr(" 足浴春季款 "), TemplateID: 101, Industry: "foot_bath",
		Inputs: inputsOf(`{"shop_name":"小店","style":"a"}`), Variants: iPtr(3), Text: sPtr("做得温馨一点")})
	if err != nil {
		t.Fatal(err)
	}
	if it.Name != "足浴春季款" || it.Variants != 3 || it.Text != "做得温馨一点" || it.Industry != "foot_bath" ||
		!it.TemplateAvailable || it.Template == nil || it.Template.Slots == nil || it.Stale || it.Inputs["style"] != "a" {
		t.Fatalf("创建结果: %+v", it)
	}
	// 缺省 variants=1、无入参的模板（102 没有 schema）；industry 缺省取模板自己的行业
	it2, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("第二个"), TemplateID: 102})
	if err != nil || it2.Variants != 1 || it2.Industry != "foot_bath" || it2.Resolution != "1k" {
		t.Fatalf("缺省: %v %+v", err, it2)
	}
	// 清晰度：记住选择，改得动，非法被拒
	it3, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("高清款"), TemplateID: 102, Resolution: sPtr("2K")})
	if err != nil || it3.Resolution != "2k" {
		t.Fatalf("清晰度创建: %v %+v", err, it3)
	}
	if up3, err := s.UpdateMyTemplate(ctx, 1, it3.ID, MyTemplateInput{Resolution: sPtr("4k")}); err != nil || up3.Resolution != "4k" {
		t.Fatalf("清晰度修改: %v %+v", err, up3)
	}
	if _, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("x"), TemplateID: 102, Resolution: sPtr("8k")}); err == nil {
		t.Fatal("非法清晰度应被拒")
	}
	if err := s.DeleteMyTemplate(ctx, 1, it3.ID); err != nil {
		t.Logf("清理: %v", err)
	}
	l, err := s.ListMyTemplates(ctx, 1, "foot_bath", "")
	if err != nil || len(l.Items) != 2 || l.Items[0].ID != it2.ID || l.Items[1].ID != it.ID || l.Limit != MaxMyTemplates {
		t.Fatalf("列表（倒序）: %v %+v", err, l)
	}
	// 只改名：其余不动
	up, err := s.UpdateMyTemplate(ctx, 1, it.ID, MyTemplateInput{Name: sPtr("改名了")})
	if err != nil || up.Name != "改名了" || up.Variants != 3 || up.Inputs["shop_name"] != "小店" || up.Text != "做得温馨一点" {
		t.Fatalf("改名: %v %+v", err, up)
	}
	// 改入参 / 版数 / 文字
	up, err = s.UpdateMyTemplate(ctx, 1, it.ID, MyTemplateInput{Inputs: inputsOf(`{"shop_name":"新店","style":"b"}`), Variants: iPtr(2), Text: sPtr("")})
	if err != nil || up.Inputs["shop_name"] != "新店" || up.Variants != 2 || up.Text != "" {
		t.Fatalf("改设置: %v %+v", err, up)
	}
	// 改入参按 schema 校验：必填缺失 / 选项非法被拒，且不落库
	for _, bad := range []string{`{"style":"a"}`, `{"shop_name":"x","style":"zzz"}`, `{"shop_name":"x","nope":1}`} {
		_, err := s.UpdateMyTemplate(ctx, 1, it.ID, MyTemplateInput{Inputs: inputsOf(bad)})
		if err == nil || AsSlotsError(err) == nil {
			t.Fatalf("入参 %s 应被 schema 拒: %v", bad, err)
		}
	}
	if cur, _ := s.getMyItem(ctx, 1, it.ID); cur.Inputs["shop_name"] != "新店" {
		t.Fatalf("被拒的修改不能落库: %+v", cur)
	}
	// 创建时同样校验
	if _, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("x"), TemplateID: 101, Inputs: inputsOf(`{}`)}); AsSlotsError(err) == nil {
		t.Fatalf("缺必填应被拒: %v", err)
	}
	// 字段校验
	for _, in := range []MyTemplateInput{
		{Name: sPtr(""), TemplateID: 102}, {TemplateID: 102}, {Name: sPtr("x"), TemplateID: 102, Variants: iPtr(0)},
		{Name: sPtr("x"), TemplateID: 102, Variants: iPtr(9)}, {Name: sPtr("x")},
	} {
		if _, err := s.CreateMyTemplate(ctx, 1, in); err == nil {
			t.Errorf("应被拒: %+v", in)
		}
	}
	// 不可见模板（105）不能存
	if _, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("x"), TemplateID: 105}); err == nil {
		t.Fatal("不可见模板不能存")
	}
	// 删除幂等
	for i := 0; i < 2; i++ {
		if err := s.DeleteMyTemplate(ctx, 1, it.ID); err != nil {
			t.Fatal(err)
		}
	}
	if l, _ = s.ListMyTemplates(ctx, 1, "foot_bath", ""); len(l.Items) != 1 {
		t.Fatalf("删后: %+v", l)
	}
	if _, err := s.UpdateMyTemplate(ctx, 1, it.ID, MyTemplateInput{Name: sPtr("x")}); codeOf(err) != xerr.NotFound {
		t.Fatalf("改已删除的: %v", err)
	}
}

func TestUserLibMyTemplatesUserIsolation(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	it, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("甲的"), TemplateID: 102})
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListMyTemplates(ctx, 2, "foot_bath", ""); len(l.Items) != 0 {
		t.Fatalf("别人看不到: %+v", l)
	}
	if _, err := s.UpdateMyTemplate(ctx, 2, it.ID, MyTemplateInput{Name: sPtr("被抢")}); codeOf(err) != xerr.NotFound {
		t.Fatalf("改别人的: %v", err)
	}
	if err := s.DeleteMyTemplate(ctx, 2, it.ID); err != nil { // 幂等成功但没有效果
		t.Fatal(err)
	}
	cur, err := s.getMyItem(ctx, 1, it.ID)
	if err != nil || cur.Name != "甲的" {
		t.Fatalf("甲的没被动: %v %+v", err, cur)
	}
	if _, err := s.getMyItem(ctx, 2, it.ID); codeOf(err) != xerr.NotFound {
		t.Fatalf("乙读不到: %v", err)
	}
}

func TestUserLibMyTemplatesIndustryStrict(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	// 104 属于 spa：以 spa 创建
	a, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("spa 的"), TemplateID: 104, Industry: "spa"})
	if err != nil {
		t.Fatal(err)
	}
	// 以不属于模板的行业创建：拒
	if _, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("x"), TemplateID: 101, Industry: "spa"}); err == nil {
		t.Fatal("模板不属于所选行业应被拒")
	}
	if _, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("x"), TemplateID: 101, Industry: "dead"}); reasonOf(err) != ReasonIndustryNotFound {
		t.Fatalf("停用行业: %v", err)
	}
	// 103 在二级 bluev：以二级创建，一级和二级里都看得到
	b, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("二级的"), TemplateID: 103, Industry: "foot_bath", SubIndustry: "foot_bath_bluev"})
	if err != nil || b.Industry != "foot_bath_bluev" {
		t.Fatalf("二级: %v %+v", err, b)
	}
	l, _ := s.ListMyTemplates(ctx, 1, "foot_bath", "")
	if len(l.Items) != 1 || l.Items[0].ID != b.ID {
		t.Fatalf("foot_bath 只含自己的: %+v", l)
	}
	if l, _ = s.ListMyTemplates(ctx, 1, "spa", ""); len(l.Items) != 1 || l.Items[0].ID != a.ID {
		t.Fatalf("spa: %+v", l)
	}
	if l, _ = s.ListMyTemplates(ctx, 1, "foot_bath", "foot_bath_bluev"); len(l.Items) != 1 {
		t.Fatalf("二级: %+v", l)
	}
	if _, err := s.ListMyTemplates(ctx, 1, "", ""); codeOf(err) != xerr.ParamMissing {
		t.Fatal(err)
	}
}

func TestUserLibMyTemplatesSchemaDriftAndOffline(t *testing.T) {
	s, src := userLibService(t)
	ctx := context.Background()
	it, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("存的"), TemplateID: 101, Inputs: inputsOf(`{"shop_name":"小店","style":"a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// 运营把 style 的选项 a 删了
	src.tpls[0].SlotsJSON = json.RawMessage(`{"schemaVersion":1,"items":[` +
		`{"key":"shop_name","type":"text","label":"店名","required":true,"maxLength":10},` +
		`{"key":"style","type":"select","label":"风格","options":[{"value":"b","label":"B"}]}]}`)
	s.Invalidate(ctx)
	l, _ := s.ListMyTemplates(ctx, 1, "foot_bath", "")
	got := l.Items[0]
	if !got.Stale || got.StaleHint != StaleHint || !reflect.DeepEqual(got.StaleKeys, []string{"style"}) ||
		got.Inputs["shop_name"] != "小店" || len(got.Inputs) != 1 || !got.TemplateAvailable {
		t.Fatalf("部分失效: %+v", got)
	}
	// 库里的原值不被读取改写
	var raw string
	_ = s.db.NewRaw("SELECT CAST(inputs_json AS CHAR) FROM afv_plaza_my_template WHERE id = ?", it.ID).Scan(ctx, &raw)
	if !strings.Contains(raw, `"a"`) {
		t.Fatalf("读取不改库: %s", raw)
	}
	// 模板整体下线：记录保留，不可恢复，可改名删除
	src.tpls[0].State = model.StateOffline
	s.Invalidate(ctx)
	l, _ = s.ListMyTemplates(ctx, 1, "foot_bath", "")
	if len(l.Items) != 1 || l.Items[0].TemplateAvailable || l.Items[0].Template != nil || l.Items[0].Stale {
		t.Fatalf("下线: %+v", l.Items)
	}
	if up, err := s.UpdateMyTemplate(ctx, 1, it.ID, MyTemplateInput{Name: sPtr("改名")}); err != nil || up.Name != "改名" {
		t.Fatalf("下线后可改名: %v", err)
	}
	if _, err := s.UpdateMyTemplate(ctx, 1, it.ID, MyTemplateInput{Inputs: inputsOf(`{"shop_name":"x"}`)}); err == nil {
		t.Fatal("下线后不能改设置")
	}
}

func TestUserLibMyTemplatesLimit(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_my_template (user_id, name, template_id, industry_id, variants, text) "+
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?) SELECT 1, 'x', 102, 1, 1, '' FROM n", MaxMyTemplates-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("第100个"), TemplateID: 102}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("第101个"), TemplateID: 102}); reasonOf(err) != ReasonMyTemplateLimit {
		t.Fatalf("应满: %v", err)
	}
	if _, err := s.CreateMyTemplate(ctx, 2, MyTemplateInput{Name: sPtr("别人"), TemplateID: 102}); err != nil {
		t.Fatalf("别人不受影响: %v", err)
	}
}
