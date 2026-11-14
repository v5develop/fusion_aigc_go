package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aigc-local-life/pkg/xerr"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestParseSlotSchemaTable(t *testing.T) {
	ok := `{"key":"shop_name","type":"text","label":"店名"}`
	cases := []struct {
		name string
		in   string
		want string // 空 = 通过；否则错误里必须含（字段路径 / 提示）
	}{
		{"空数组", `[]`, ""},
		{"null", `null`, ""},
		{"裸数组合法", `[` + ok + `]`, ""},
		{"对象 v1 合法", `{"schemaVersion":1,"items":[` + ok + `]}`, ""},
		{"旧键 slots 兼容读取", `{"schemaVersion":1,"slots":[` + ok + `]}`, ""},
		{"items 为空", `{"schemaVersion":1,"items":[]}`, ""},
		{"items 与旧键并存", `{"schemaVersion":1,"items":[` + ok + `],"slots":[` + ok + `]}`, "items"},
		{"版本不对", `{"schemaVersion":2,"items":[` + ok + `]}`, "schemaVersion"},
		{"缺版本", `{"items":[` + ok + `]}`, "schemaVersion"},
		{"不是数组也不是对象", `"x"`, "必须是对象"},
		{"未知字段", `[{"key":"a","type":"text","label":"A","maxLen":3}]`, "unknown field"},
		{"key 格式", `[{"key":"Shop","type":"text","label":"A"}]`, "items[0].key"},
		{"key 超长", `[{"key":"a123456789012345678901234567890123","type":"text","label":"A"}]`, "items[0].key"},
		{"key 重复", `[` + ok + `,` + ok + `]`, "items[1].key"},
		{"类型非法", `[{"key":"a","type":"date","label":"A"}]`, "items[0].type"},
		{"label 缺", `[{"key":"a","type":"text"}]`, "items[0].label"},
		{"image 超过 9 张", `[{"key":"a","type":"image","label":"A","maxCount":10}]`, "items[0].maxCount"},
		{"image min>max", `[{"key":"a","type":"image","label":"A","minCount":3,"maxCount":2}]`, "items[0].minCount"},
		{"image 必填 min 0", `[{"key":"a","type":"image","label":"A","required":true,"minCount":0}]`, "items[0].minCount"},
		{"image 比例格式", `[{"key":"a","type":"image","label":"A","acceptRatio":"9x16"}]`, "items[0].acceptRatio"},
		{"image 示例图地址", `[{"key":"a","type":"image","label":"A","exampleUrl":"ftp://x"}]`, "items[0].exampleUrl"},
		{"image 带 maxLength", `[{"key":"a","type":"image","label":"A","maxLength":5}]`, "items[0].maxLength"},
		{"image 不支持 default", `[{"key":"a","type":"image","label":"A","default":"x"}]`, "items[0].default"},
		{"text 带 maxDurationSec", `[{"key":"a","type":"text","label":"A","maxDurationSec":5}]`, "items[0].maxDurationSec"},
		{"video 时长越界", `[{"key":"a","type":"video","label":"A","maxDurationSec":601}]`, "items[0].maxDurationSec"},
		{"text maxLength 越界", `[{"key":"a","type":"text","label":"A","maxLength":2001}]`, "items[0].maxLength"},
		{"text layout 不是对象", `[{"key":"a","type":"text","label":"A","layout":[1]}]`, "items[0].layout"},
		{"text layout 合法且未知键原样", `[{"key":"a","type":"text","label":"A","layout":{"box":{"x":0.1,"y":0.1,"w":0.5,"h":0.1},"font":"f"}}]`, ""},
		{"text layout 缺 box", `[{"key":"a","type":"text","label":"A","layout":{"x":0.1}}]`, "items[0].layout.box"},
		{"textarea 不支持 layout", `[{"key":"a","type":"textarea","label":"A","layout":{"box":{"x":0,"y":0,"w":1,"h":1}}}]`, "items[0].layout"},
		{"number min>max", `[{"key":"a","type":"number","label":"A","min":5,"max":1}]`, "items[0].min"},
		{"number step<=0", `[{"key":"a","type":"number","label":"A","step":0}]`, "items[0].step"},
		{"number 默认值越界", `[{"key":"a","type":"number","label":"A","min":1,"max":3,"default":9}]`, "items[0].default"},
		{"price 带 options", `[{"key":"a","type":"price","label":"A","options":[{"value":"1","label":"1"}]}]`, "items[0].options"},
		{"select 无选项", `[{"key":"a","type":"select","label":"A"}]`, "items[0].options"},
		{"select 选项重复", `[{"key":"a","type":"select","label":"A","options":[{"value":"x","label":"X"},{"value":"x","label":"Y"}]}]`, "items[0].options[1].value"},
		{"select 选项缺 label", `[{"key":"a","type":"select","label":"A","options":[{"value":"x","label":""}]}]`, "items[0].options[0].label"},
		{"select 默认不在选项里", `[{"key":"a","type":"select","label":"A","options":[{"value":"x","label":"X"}],"default":"y"}]`, "items[0].default"},
		{"multiselect 默认合法", `[{"key":"a","type":"multiselect","label":"A","maxCount":2,"options":[{"value":"x","label":"X"},{"value":"y","label":"Y"}],"default":["x"]}]`, ""},
		{"multiselect maxCount 超选项数", `[{"key":"a","type":"multiselect","label":"A","maxCount":3,"options":[{"value":"x","label":"X"}]}]`, "items[0].maxCount"},
		{"switch 默认非布尔", `[{"key":"a","type":"switch","label":"A","default":"yes"}]`, "items[0].default"},
		{"color 默认格式", `[{"key":"a","type":"color","label":"A","default":"red"}]`, "items[0].default"},
		{"color 合法", `[{"key":"a","type":"color","label":"A","default":"#ff00aa"}]`, ""},
		{"visibleWhen 引用不存在", `[{"key":"a","type":"text","label":"A","visibleWhen":{"key":"zz","equals":1}}]`, "items[0].visibleWhen.key"},
		{"visibleWhen 自引用", `[{"key":"a","type":"text","label":"A","visibleWhen":{"key":"a","equals":1}}]`, "items[0].visibleWhen.key"},
		{"visibleWhen 嵌套", `[{"key":"s","type":"switch","label":"S","visibleWhen":{"key":"t","equals":true}},{"key":"t","type":"switch","label":"T"},{"key":"a","type":"text","label":"A","visibleWhen":{"key":"s","equals":true}}]`, "items[2].visibleWhen.key"},
		{"visibleWhen 引用类型不支持", `[{"key":"n","type":"number","label":"N"},{"key":"a","type":"text","label":"A","visibleWhen":{"key":"n","equals":1}}]`, "items[1].visibleWhen.key"},
		{"visibleWhen equals 非标量", `[{"key":"s","type":"switch","label":"S"},{"key":"a","type":"text","label":"A","visibleWhen":{"key":"s","equals":[1]}}]`, "items[1].visibleWhen.equals"},
	}
	for _, c := range cases {
		_, err := ParseSlotSchema([]byte(c.in))
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: 应通过: %v", c.name, err)
			}
			continue
		}
		if err == nil || codeOf(err) != xerr.ParamError || !strings.Contains(err.Error()+msgOf(err), c.want) {
			t.Errorf("%s: 应拒收且带路径 %q，得到 %v", c.name, c.want, err)
		}
	}
}

func msgOf(err error) string { _, m, _ := xerr.Parse(err); return m }

func TestTooManySlotsAndNormalization(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 21; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"key":"k` + string(rune('a'+i)) + `","type":"text","label":"L"}`)
	}
	sb.WriteString("]")
	if _, err := ParseSlotSchema([]byte(sb.String())); err == nil || !strings.Contains(msgOf(err), "items：") {
		t.Fatalf("超过 20 个: %v", err)
	}
	sc, err := ParseSlotSchema([]byte(`[
	 {"key":"b","type":"image","label":"B","order":2,"required":true},
	 {"key":"a","type":"video","label":"A","order":1},
	 {"key":"c","type":"textarea","label":"C","order":3},
	 {"key":"p","type":"price","label":"P","order":4}]`))
	if err != nil {
		t.Fatal(err)
	}
	if sc.SchemaVersion != 1 || sc.Items[0].Key != "a" || sc.Items[1].Key != "b" {
		t.Fatalf("按 order 稳定排序并补 schemaVersion=1: %+v", sc)
	}
	if *sc.Items[1].MinCount != 1 || *sc.Items[1].MaxCount != 1 || *sc.Items[0].MaxDurationSec != 60 || *sc.Items[2].MaxLength != 500 || *sc.Items[3].Min != 0 {
		t.Fatalf("默认值补齐: %+v", sc.Items)
	}
	b := string(sc.Marshal())
	if !strings.HasPrefix(b, `{"schemaVersion":1,"items":[`) {
		t.Fatal(b)
	}
	// 存下来的形态能再解析且结果一致
	again, err := ParseSlotSchema([]byte(b))
	if err != nil || string(again.Marshal()) != b {
		t.Fatalf("round trip: %v", err)
	}
	if (*SlotSchema)(nil).Marshal() != nil {
		t.Fatal("nil schema 存 NULL")
	}
}

// 两个范例（与 docs/api/plaza-slot-schema-v1.md 一致）。
const footSchema = `{"schemaVersion":1,"items":[
 {"key":"storefront","type":"image","label":"门头照片","required":true,"order":1,"maxCount":1,"guidance":"正对门头，白天拍，别逆光","acceptRatio":"4:3"},
 {"key":"shop_name","type":"text","label":"店名","required":true,"order":2,"maxLength":12,"placeholder":"例如：悦足堂"}]}`

const apparelSchema = `{"schemaVersion":1,"items":[
 {"key":"products","type":"image","label":"商品图","required":true,"order":1,"minCount":1,"maxCount":3,"group":"商品"},
 {"key":"price","type":"price","label":"到手价","required":true,"order":2,"min":0.01,"max":99999,"step":0.01,"unit":"元","group":"商品"},
 {"key":"style","type":"select","label":"风格","order":3,"default":"fresh","options":[{"value":"fresh","label":"清新"},{"value":"luxury","label":"轻奢"},{"value":"street","label":"街头"}]},
 {"key":"tags","type":"multiselect","label":"卖点","order":4,"maxCount":2,"options":[{"value":"new","label":"新品"},{"value":"sale","label":"限时"},{"value":"hot","label":"爆款"}]},
 {"key":"show_price","type":"switch","label":"海报上显示价格","order":5,"default":true},
 {"key":"price_color","type":"color","label":"价格颜色","order":6,"default":"#ff3b30","visibleWhen":{"key":"show_price","equals":true}}]}`

func mustSchema(t *testing.T, s string) *SlotSchema {
	t.Helper()
	sc, err := ParseSlotSchema([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func in(kv ...string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = raw(kv[i+1])
	}
	return m
}

func TestValidateInputsFootBath(t *testing.T) {
	sc := mustSchema(t, footSchema)
	got, err := ValidateInputs(sc, in("storefront", `"https://x.com/a.jpg"`, "shop_name", `"  悦足堂 "`))
	if err != nil {
		t.Fatal(err)
	}
	if got["shop_name"] != "悦足堂" || len(got["storefront"].([]string)) != 1 {
		t.Fatalf("单个地址规整成数组、文字去空白: %v", got)
	}
	cases := []struct {
		name string
		in   map[string]json.RawMessage
		want string
	}{
		{"缺必填图", in("shop_name", `"店"`), "inputs.storefront"},
		{"缺必填文字", in("storefront", `["https://x.com/a.jpg"]`), "inputs.shop_name"},
		{"图片多于 maxCount", in("storefront", `["https://x.com/a.jpg","https://x.com/b.jpg"]`, "shop_name", `"店"`), "inputs.storefront"},
		{"非 http 地址", in("storefront", `["javascript:alert(1)"]`, "shop_name", `"店"`), "inputs.storefront"},
		{"店名超长", in("storefront", `["https://x.com/a.jpg"]`, "shop_name", `"一二三四五六七八九十一二三"`), "inputs.shop_name"},
		{"文字给了数字", in("storefront", `["https://x.com/a.jpg"]`, "shop_name", `5`), "inputs.shop_name"},
		{"未知入参", in("storefront", `["https://x.com/a.jpg"]`, "shop_name", `"店"`, "evil", `1`), "inputs.evil"},
	}
	for _, c := range cases {
		if _, err := ValidateInputs(sc, c.in); err == nil || !strings.Contains(msgOf(err), c.want) || codeOf(err) != xerr.ParamError {
			t.Errorf("%s: 应拒收并带 %s: %v", c.name, c.want, err)
		}
	}
}

func TestValidateInputsApparel(t *testing.T) {
	sc := mustSchema(t, apparelSchema)
	base := []string{"products", `["https://x.com/1.jpg","https://x.com/2.jpg"]`, "price", `199.5`}
	got, err := ValidateInputs(sc, in(base...))
	if err != nil {
		t.Fatal(err)
	}
	if got["style"] != "fresh" || got["show_price"] != true || got["price_color"] != "#FF3B30" || got["price"] != 199.5 {
		t.Fatalf("缺省取 default，条件显示项（show_price=true 默认）可见: %v", got)
	}
	// visibleWhen：关闭价格显示后，价格颜色不可见 → 即使传了非法值也忽略，且不出现在结果里
	got, err = ValidateInputs(sc, in(append(base, "show_price", `false`, "price_color", `"not-a-color"`)...))
	if err != nil {
		t.Fatalf("不可见项忽略: %v", err)
	}
	if _, has := got["price_color"]; has {
		t.Fatalf("不可见项不应出现: %v", got)
	}
	// 可见时非法颜色被拒
	if _, err := ValidateInputs(sc, in(append(base, "price_color", `"red"`)...)); err == nil || !strings.Contains(msgOf(err), "inputs.price_color") {
		t.Fatalf("可见时校验: %v", err)
	}
	cases := []struct {
		name string
		kv   []string
		want string
	}{
		{"商品图 0 张", []string{"products", `[]`, "price", `10`}, "inputs.products"},
		{"商品图 4 张", []string{"products", `["https://a.com/1","https://a.com/2","https://a.com/3","https://a.com/4"]`, "price", `10`}, "inputs.products"},
		{"价格缺", []string{"products", `["https://a.com/1"]`}, "inputs.price"},
		{"价格低于下限", []string{"products", `["https://a.com/1"]`, "price", `0`}, "inputs.price"},
		{"价格超上限", []string{"products", `["https://a.com/1"]`, "price", `100000`}, "inputs.price"},
		{"价格步长", []string{"products", `["https://a.com/1"]`, "price", `10.005`}, "inputs.price"},
		{"价格是字符串", []string{"products", `["https://a.com/1"]`, "price", `"10"`}, "inputs.price"},
		{"风格不在选项", []string{"products", `["https://a.com/1"]`, "price", `10`, "style", `"gothic"`}, "inputs.style"},
		{"卖点超过 maxCount", []string{"products", `["https://a.com/1"]`, "price", `10`, "tags", `["new","sale","hot"]`}, "inputs.tags"},
		{"卖点重复", []string{"products", `["https://a.com/1"]`, "price", `10`, "tags", `["new","new"]`}, "inputs.tags"},
		{"卖点不在选项", []string{"products", `["https://a.com/1"]`, "price", `10`, "tags", `["zzz"]`}, "inputs.tags"},
	}
	for _, c := range cases {
		if _, err := ValidateInputs(sc, in(c.kv...)); err == nil || !strings.Contains(msgOf(err), c.want) {
			t.Errorf("%s: 应拒收并带 %s: %v", c.name, c.want, err)
		}
	}
	// 多处错误一次说清
	if _, err := ValidateInputs(sc, in()); err == nil || !strings.Contains(msgOf(err), "inputs.products") || !strings.Contains(msgOf(err), "inputs.price") {
		t.Fatalf("多处错误一并返回: %v", err)
	}
}

func TestValidateInputsNoSchemaAndHiddenRequired(t *testing.T) {
	if got, err := ValidateInputs(nil, nil); err != nil || len(got) != 0 {
		t.Fatalf("没有入参定义允许空入参: %v %v", got, err)
	}
	if _, err := ValidateInputs(nil, in("a", `1`)); err == nil {
		t.Fatal("没有入参定义时不接受任何入参")
	}
	sc := mustSchema(t, `[{"key":"s","type":"switch","label":"S"},
	  {"key":"must","type":"text","label":"必填文字","required":true,"visibleWhen":{"key":"s","equals":true}}]`)
	if _, err := ValidateInputs(sc, in()); err != nil {
		t.Fatalf("不可见的必填项不要求: %v", err)
	}
	if _, err := ValidateInputs(sc, in("s", `true`)); err == nil || !strings.Contains(msgOf(err), "inputs.must") {
		t.Fatalf("可见后必填: %v", err)
	}
	// switch 显式 false 是有效值，不算「未填」
	sc = mustSchema(t, `[{"key":"s","type":"switch","label":"S","required":true}]`)
	if got, err := ValidateInputs(sc, in("s", `false`)); err != nil || got["s"] != false {
		t.Fatalf("switch=false 合法: %v %v", got, err)
	}
}

func TestDetailIncludesSlotsListDoesNot(t *testing.T) {
	src := stdFixture()
	src.tpls[3].SlotsJSON = mustSchema(t, footSchema).Marshal() // 104 在 spa
	s := newTestService(src)
	d, err := s.TemplateByID(ctx0, 104)
	if err != nil || d.Slots == nil || len(d.Slots.Items) != 2 {
		t.Fatalf("详情下发 slots: %+v %v", d, err)
	}
	pg, err := s.Templates(ctx0, ListParams{Industry: "spa", Page: 1, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pg)
	if strings.Contains(string(b), "slots") || strings.Contains(string(b), "storefront") {
		t.Fatalf("浏览列表不下发 slots: %s", b)
	}
	out, err := s.PrepareInputs(ctx0, 104, in("storefront", `["https://x.com/a.jpg"]`, "shop_name", `"店"`))
	if err != nil || out["shop_name"] != "店" {
		t.Fatalf("%v %v", out, err)
	}
	if _, err := s.PrepareInputs(ctx0, 104, in("shop_name", `"店"`)); err == nil {
		t.Fatal("缺必填应拒收")
	}
	if _, err := s.PrepareInputs(ctx0, 999, in()); codeOf(err) != xerr.NotFound {
		t.Fatalf("不可见模板: %v", err)
	}
	// 库里脏的 slots_json 不让详情打不开
	src.tpls[3].SlotsJSON = []byte(`{"schemaVersion":9}`)
	if d, err = newTestService(src).TemplateByID(ctx0, 104); err != nil || d.Slots != nil {
		t.Fatalf("脏数据降级为无入参: %+v %v", d, err)
	}
}

// 与管理端前端同口径的五条保存校验（错误路径 items[n].field，reason=SLOTS_INVALID）。
func TestSlotSchemaFrontendParity(t *testing.T) {
	opts := `"options":[{"value":"x","label":"X"},{"value":"y","label":"Y"}]`
	cases := []struct {
		name string
		in   string
		want string // 空 = 通过；否则为第一处错误的路径（SlotsError.Field）
	}{
		// 1) required 的 image / video 位 minCount ≥ 1
		{"必填图片 minCount=0", `[{"key":"a","type":"image","label":"A","required":true,"minCount":0}]`, "items[0].minCount"},
		{"必填视频 minCount=0", `[{"key":"a","type":"video","label":"A","required":true,"minCount":0,"maxCount":2}]`, "items[0].minCount"},
		{"必填图片缺省 minCount 补 1", `[{"key":"a","type":"image","label":"A","required":true}]`, ""},
		{"非必填图片 minCount=0", `[{"key":"a","type":"image","label":"A","minCount":0}]`, ""},
		// 2) acceptRatio 形如 正整数:正整数
		{"比例 9:16", `[{"key":"a","type":"image","label":"A","acceptRatio":"9:16"}]`, ""},
		{"比例 1080:1920", `[{"key":"a","type":"video","label":"A","acceptRatio":"1080:1920"}]`, ""},
		{"比例含 0", `[{"key":"a","type":"image","label":"A","acceptRatio":"0:16"}]`, "items[0].acceptRatio"},
		{"比例小数", `[{"key":"a","type":"image","label":"A","acceptRatio":"1.5:1"}]`, "items[0].acceptRatio"},
		{"比例前导 0", `[{"key":"a","type":"image","label":"A","acceptRatio":"09:16"}]`, "items[0].acceptRatio"},
		{"比例带空格", `[{"key":"a","type":"image","label":"A","acceptRatio":"9 : 16"}]`, "items[0].acceptRatio"},
		// 3) color 默认值 #RRGGBB
		{"颜色三位", `[{"key":"a","type":"color","label":"A","default":"#fff"}]`, "items[0].default"},
		{"颜色无井号", `[{"key":"a","type":"color","label":"A","default":"ff3b30"}]`, "items[0].default"},
		{"颜色合法", `[{"key":"a","type":"color","label":"A","default":"#FF3B30"}]`, ""},
		// 4) default 与 type 匹配
		{"select 默认不在选项", `[{"key":"a","type":"select","label":"A",` + opts + `,"default":"z"}]`, "items[0].default"},
		{"select 默认是数组", `[{"key":"a","type":"select","label":"A",` + opts + `,"default":["x"]}]`, "items[0].default"},
		{"multiselect 默认非子集", `[{"key":"a","type":"multiselect","label":"A",` + opts + `,"default":["x","z"]}]`, "items[0].default"},
		{"multiselect 默认是字符串", `[{"key":"a","type":"multiselect","label":"A",` + opts + `,"default":"x"}]`, "items[0].default"},
		{"multiselect 默认子集", `[{"key":"a","type":"multiselect","label":"A",` + opts + `,"default":["y"]}]`, ""},
		{"number 默认低于 min", `[{"key":"a","type":"number","label":"A","min":1,"max":5,"default":0}]`, "items[0].default"},
		{"number 默认高于 max", `[{"key":"a","type":"number","label":"A","min":1,"max":5,"default":6}]`, "items[0].default"},
		{"number 默认是字符串", `[{"key":"a","type":"number","label":"A","default":"3"}]`, "items[0].default"},
		{"price 默认负数（min 缺省 0）", `[{"key":"a","type":"price","label":"A","default":-1}]`, "items[0].default"},
		{"price 默认在范围内", `[{"key":"a","type":"price","label":"A","min":1,"max":99,"default":9.9}]`, ""},
		{"switch 默认是字符串", `[{"key":"a","type":"switch","label":"A","default":"true"}]`, "items[0].default"},
		{"switch 默认是数字", `[{"key":"a","type":"switch","label":"A","default":1}]`, "items[0].default"},
		{"switch 默认 false", `[{"key":"a","type":"switch","label":"A","default":false}]`, ""},
		// 5) visibleWhen.equals 必填；key 必须指向本模板内另一个已存在的 key
		{"equals 缺", `[{"key":"s","type":"switch","label":"S"},{"key":"a","type":"text","label":"A","visibleWhen":{"key":"s"}}]`, "items[1].visibleWhen.equals"},
		{"equals 为 null", `[{"key":"s","type":"switch","label":"S"},{"key":"a","type":"text","label":"A","visibleWhen":{"key":"s","equals":null}}]`, "items[1].visibleWhen.equals"},
		{"key 缺", `[{"key":"s","type":"switch","label":"S"},{"key":"a","type":"text","label":"A","visibleWhen":{"equals":true}}]`, "items[1].visibleWhen.key"},
		{"key 不存在", `[{"key":"s","type":"switch","label":"S"},{"key":"a","type":"text","label":"A","visibleWhen":{"key":"zz","equals":true}}]`, "items[1].visibleWhen.key"},
		{"key 指向自己", `[{"key":"a","type":"switch","label":"A","visibleWhen":{"key":"a","equals":true}}]`, "items[0].visibleWhen.key"},
		{"引用在后面的项", `[{"key":"a","type":"text","label":"A","visibleWhen":{"key":"s","equals":true}},{"key":"s","type":"switch","label":"S"}]`, ""},
	}
	for _, c := range cases {
		_, err := ParseSlotSchema([]byte(c.in))
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: 应通过: %v", c.name, err)
			}
			continue
		}
		se := AsSlotsError(err)
		if se == nil || se.Reason != ReasonSlotsInvalid || se.Field() != c.want || codeOf(err) != xerr.ParamError {
			t.Errorf("%s: 应拒收且 field=%q，得到 %#v (%v)", c.name, c.want, se, err)
		}
	}
}

func TestSlotsErrorEnvelope(t *testing.T) {
	_, err := ParseSlotSchema([]byte(`{"schemaVersion":1,"items":[{"key":"a","type":"select","label":"A","options":[{"value":"x","label":"X"},{"value":"x","label":"Y"}]},{"key":"B","type":"text","label":"B"}]}`))
	se := AsSlotsError(err)
	if se == nil {
		t.Fatalf("应是 SlotsError: %v", err)
	}
	if xerr.ReasonOf(err) != ReasonSlotsInvalid {
		t.Fatalf("reason: %s", xerr.ReasonOf(err))
	}
	d := se.Data()
	if d["field"] != "items[1].key" && d["field"] != "items[0].options[1].value" {
		t.Fatalf("field: %v", d["field"])
	}
	if fs := d["errors"].([]SlotFieldError); len(fs) != 2 {
		t.Fatalf("全部错误: %+v", fs)
	}
	// 做同款入参的错误仍是 PARAM_INVALID
	_, err = ValidateInputs(mustSchema(t, footSchema), in())
	if xerr.ReasonOf(err) != "PARAM_INVALID" || AsSlotsError(err).Field() != "inputs.storefront" {
		t.Fatalf("做同款入参: %v %s", err, xerr.ReasonOf(err))
	}
}

// L1：visibleWhen.equals 的类型须与被引用项一致。
func TestVisibleWhenEqualsMatchesRefType(t *testing.T) {
	sel := `{"key":"m","type":"select","label":"M","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}`
	cases := []struct {
		name, items, wantRule string
	}{
		{"switch 配字符串", `[{"key":"s","type":"switch","label":"S"},{"key":"x","type":"text","label":"X","visibleWhen":{"key":"s","equals":"true"}}]`, "visibleWhen.equals_type"},
		{"switch 配数字", `[{"key":"s","type":"switch","label":"S"},{"key":"x","type":"text","label":"X","visibleWhen":{"key":"s","equals":1}}]`, "visibleWhen.equals_type"},
		{"select 配数字", `[` + sel + `,{"key":"x","type":"text","label":"X","visibleWhen":{"key":"m","equals":1}}]`, "visibleWhen.equals_type"},
		{"select 配非法选项", `[` + sel + `,{"key":"x","type":"text","label":"X","visibleWhen":{"key":"m","equals":"z"}}]`, "visibleWhen.equals_option"},
		{"text 配布尔", `[{"key":"t","type":"text","label":"T"},{"key":"x","type":"text","label":"X","visibleWhen":{"key":"t","equals":false}}]`, "visibleWhen.equals_type"},
		{"switch 配布尔 ok", `[{"key":"s","type":"switch","label":"S"},{"key":"x","type":"text","label":"X","visibleWhen":{"key":"s","equals":true}}]`, ""},
		{"select 合法选项 ok", `[` + sel + `,{"key":"x","type":"text","label":"X","visibleWhen":{"key":"m","equals":"b"}}]`, ""},
		{"text 配字符串 ok", `[{"key":"t","type":"text","label":"T"},{"key":"x","type":"text","label":"X","visibleWhen":{"key":"t","equals":"hi"}}]`, ""},
	}
	for _, c := range cases {
		_, err := ParseSlotSchema([]byte(`{"schemaVersion":1,"items":` + c.items + `}`))
		se := AsSlotsError(err)
		if c.wantRule == "" {
			if err != nil {
				t.Errorf("%s: 不应拒收: %v", c.name, err)
			}
			continue
		}
		if se == nil || se.Fields[0].Rule != c.wantRule || se.Field() != "items[1].visibleWhen.equals" {
			t.Errorf("%s: want rule %s, got %+v", c.name, c.wantRule, se)
		}
	}
	// number / price 分支（预留）同口径
	if r, _ := equalsMatchesRef(&Slot{Type: SlotNumber}, []byte(`"1"`)); r == "" {
		t.Error("number 配字符串应拒")
	}
	if r, _ := equalsMatchesRef(&Slot{Type: SlotPrice}, []byte(`9.9`)); r != "" {
		t.Error("price 配数字应通过")
	}
}

// L3：入参 key 数先限（≤ maxSlots×2），错误最多返回 20 条。
func TestValidateInputsLimits(t *testing.T) {
	in := map[string]json.RawMessage{}
	for i := 0; i <= maxInputKeys; i++ {
		in[fmt.Sprintf("k%03d", i)] = json.RawMessage(`"x"`)
	}
	_, err := ValidateInputs(nil, in)
	se := AsSlotsError(err)
	if se == nil || len(se.Fields) != 1 || se.Fields[0].Rule != "inputs.too_many" {
		t.Fatalf("超过 key 上限应一次拒收: %+v", se)
	}
	in = map[string]json.RawMessage{}
	for i := 0; i < maxInputKeys; i++ { // 未到上限：逐个报 unknown，但最多 20 条
		in[fmt.Sprintf("k%03d", i)] = json.RawMessage(`"x"`)
	}
	se = AsSlotsError(func() error { _, e := ValidateInputs(nil, in); return e }())
	if se == nil || len(se.Fields) != maxReportedErrs || !strings.Contains(se.Msg, fmt.Sprintf("共 %d 处", maxInputKeys)) {
		t.Fatalf("错误最多 20 条且提示总数: %d %s", len(se.Fields), se.Msg)
	}
}

// L8：必填项传 null 报必填缺失（switch 的 null 不再被当成 false）。
func TestRequiredNullIsMissing(t *testing.T) {
	schema, err := ParseSlotSchema([]byte(`{"schemaVersion":1,"items":[{"key":"on","type":"switch","label":"开关","required":true},{"key":"name","type":"text","label":"店名","required":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ValidateInputs(schema, map[string]json.RawMessage{"on": json.RawMessage(`null`), "name": json.RawMessage(`null`)})
	se := AsSlotsError(err)
	if se == nil || len(se.Fields) != 2 || se.Fields[0].Rule != "required" || se.Fields[1].Rule != "required" {
		t.Fatalf("null 应报必填: %+v", se)
	}
	out, err := ValidateInputs(schema, map[string]json.RawMessage{"on": json.RawMessage(`false`), "name": json.RawMessage(`"x"`)})
	if err != nil || out["on"] != false {
		t.Fatalf("显式 false 是有效取值: %v %v", out, err)
	}
}
