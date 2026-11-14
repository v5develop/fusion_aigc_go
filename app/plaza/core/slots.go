package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 可替换位 / 入参 schema v1（slots_json）。契约说明：docs/api/plaza-slot-schema-v1.md。
//
// 线上与存储形态：{"schemaVersion":1,"items":[…]}（与两端前端一致）。读取兼容旧键 {"schemaVersion":1,"slots":[…]}
// 与裸数组（视为 v1），保存时统一规整成 items 对象并补 schemaVersion。校验错误路径相对这个对象：items[n].field。
// 浏览列表不下发；原生模板详情下发；做同款提交前由 ValidateInputs 按同一份 schema 校验入参。

// SlotSchemaVersion 当前 schema 版本。
const SlotSchemaVersion = 1

// 入参类型。
const (
	SlotImage       = "image"
	SlotVideo       = "video"
	SlotText        = "text"
	SlotTextarea    = "textarea"
	SlotPrice       = "price"
	SlotNumber      = "number"
	SlotSelect      = "select"
	SlotMultiselect = "multiselect"
	SlotSwitch      = "switch"
	SlotColor       = "color"
)

var slotTypeSet = map[string]bool{SlotImage: true, SlotVideo: true, SlotText: true, SlotTextarea: true, SlotPrice: true,
	SlotNumber: true, SlotSelect: true, SlotMultiselect: true, SlotSwitch: true, SlotColor: true}

const (
	maxSlots        = 20
	maxSlotOptions  = 50
	maxMediaCount   = 9
	maxTextLength   = 2000
	maxVideoSeconds = 600
	// maxReportedErrs 一次拒收最多返回 / 记录的错误条数。
	maxReportedErrs = 20
	// maxInputKeys 做同款入参 key 数上限（先于逐项校验）：schema 最多 maxSlots 项，留一倍余量。
	maxInputKeys = maxSlots * 2
)

var (
	slotKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	ratioRe   = regexp.MustCompile(`^[1-9]\d{0,4}:[1-9]\d{0,4}$`) // 正整数:正整数（与管理端前端同口径；每边最多 5 位防溢出）
	colorRe   = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

// SlotOption select / multiselect 的选项。
type SlotOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// VisibleWhen 简单条件显示（单层）：被引用的入参取值等于 Equals 时才显示。
type VisibleWhen struct {
	Key    string          `json:"key"`
	Equals json.RawMessage `json:"equals"`
}

// Slot 一个入参。
type Slot struct {
	Key         string          `json:"key"`
	Type        string          `json:"type"`
	Label       string          `json:"label"`
	Help        string          `json:"help,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
	Required    bool            `json:"required"`
	Default     json.RawMessage `json:"default,omitempty"`
	Order       int             `json:"order"`
	Group       string          `json:"group,omitempty"`
	// image / video
	MinCount       *int   `json:"minCount,omitempty"`
	MaxCount       *int   `json:"maxCount,omitempty"`
	Guidance       string `json:"guidance,omitempty"`
	ExampleURL     string `json:"exampleUrl,omitempty"`
	AcceptRatio    string `json:"acceptRatio,omitempty"`
	MaxDurationSec *int   `json:"maxDurationSec,omitempty"`
	// text / textarea
	MaxLength *int            `json:"maxLength,omitempty"`
	Layout    json.RawMessage `json:"layout,omitempty"` // P1 文字叠加，原样保存
	// number / price
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
	Step *float64 `json:"step,omitempty"`
	Unit string   `json:"unit,omitempty"`
	// select / multiselect
	Options     []SlotOption `json:"options,omitempty"`
	VisibleWhen *VisibleWhen `json:"visibleWhen,omitempty"`
}

// SlotSchema 一个模板的入参 schema。
type SlotSchema struct {
	SchemaVersion int    `json:"schemaVersion"`
	Items         []Slot `json:"items"`
}

// ReasonSlotsInvalid 入参 schema 校验没通过（1003）；data.field 为第一处错误的路径（如 items[2].options[1].value）。
const ReasonSlotsInvalid = "SLOTS_INVALID"

// slotErr 带字段路径与命中判据的校验错误。
type slotErr struct{ path, rule, msg string }

func (e slotErr) String() string { return e.path + "：" + e.msg }

// SlotFieldError 一处校验错误（对外）：路径、判据、原因。
type SlotFieldError struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// SlotsError 入参 schema / 做同款入参校验失败：业务码 1003，reason 见 Reason（schema 为 SLOTS_INVALID，
// 做同款入参为 PARAM_INVALID）。handler 用 errors.As 取出后在 data 里下发 field（第一处）与 errors（全部）。
type SlotsError struct {
	Reason string
	Msg    string
	Fields []SlotFieldError
}

func (e *SlotsError) Error() string { return e.Msg }

// Unwrap 让 xerr.Parse / xerr.ReasonOf 认得它（默认信封仍是 code=1003 + reason + msg）。
func (e *SlotsError) Unwrap() error { return xerr.NewReason(xerr.ParamError, e.Reason, e.Msg) }

// Field 第一处错误的路径。
func (e *SlotsError) Field() string {
	if len(e.Fields) == 0 {
		return ""
	}
	return e.Fields[0].Field
}

// Data 响应 data：{field, errors}。
func (e *SlotsError) Data() map[string]any {
	return map[string]any{"field": e.Field(), "errors": e.Fields}
}

// AsSlotsError 取出入参校验错误（不是则 nil）。
func AsSlotsError(err error) *SlotsError {
	var se *SlotsError
	if errors.As(err, &se) {
		return se
	}
	return nil
}

// reject 把校验失败转成参数错误（*SlotsError），并按「拒收日志要写清楚命中哪条判据」打日志。
func reject(scope, reason string, errs []slotErr) error {
	if len(errs) == 0 {
		return nil
	}
	total := len(errs)
	if total > maxReportedErrs { // 错误最多返回 / 记录 20 条：恶意大入参不能把日志和响应撑爆
		logx.Infof("plaza: %s拒收 共 %d 处错误，只返回 / 记录前 %d 处", scope, total, maxReportedErrs)
		errs = errs[:maxReportedErrs]
	}
	parts := make([]string, 0, len(errs))
	fields := make([]SlotFieldError, 0, len(errs))
	for _, e := range errs {
		logx.Infof("plaza: %s拒收 判据=%s 路径=%s 原因=%s", scope, e.rule, e.path, e.msg)
		parts = append(parts, e.String())
		fields = append(fields, SlotFieldError{Field: e.path, Rule: e.rule, Message: e.msg})
	}
	if len(parts) > 5 {
		parts = append(parts[:5], fmt.Sprintf("……共 %d 处", total))
	}
	if reason == "" {
		reason = xerr.Reason(xerr.ParamError)
	}
	return &SlotsError{Reason: reason, Msg: strings.Join(parts, "；"), Fields: fields}
}

// applicable 各类型允许出现的类型专属字段（其余类型专属字段出现即拒收，避免前端拿到含义不明的配置）。
var applicable = map[string]map[string]bool{
	"minCount": {SlotImage: true, SlotVideo: true}, "maxCount": {SlotImage: true, SlotVideo: true, SlotMultiselect: true},
	"guidance": {SlotImage: true, SlotVideo: true}, "exampleUrl": {SlotImage: true, SlotVideo: true},
	"acceptRatio": {SlotImage: true, SlotVideo: true}, "maxDurationSec": {SlotVideo: true},
	"maxLength": {SlotText: true, SlotTextarea: true}, "layout": {SlotText: true, SlotTextarea: true},
	"min": {SlotNumber: true, SlotPrice: true}, "max": {SlotNumber: true, SlotPrice: true},
	"step": {SlotNumber: true, SlotPrice: true}, "unit": {SlotNumber: true, SlotPrice: true},
	"options": {SlotSelect: true, SlotMultiselect: true},
}

func (s *Slot) present(field string) bool {
	switch field {
	case "minCount":
		return s.MinCount != nil
	case "maxCount":
		return s.MaxCount != nil
	case "guidance":
		return s.Guidance != ""
	case "exampleUrl":
		return s.ExampleURL != ""
	case "acceptRatio":
		return s.AcceptRatio != ""
	case "maxDurationSec":
		return s.MaxDurationSec != nil
	case "maxLength":
		return s.MaxLength != nil
	case "layout":
		return len(s.Layout) > 0
	case "min":
		return s.Min != nil
	case "max":
		return s.Max != nil
	case "step":
		return s.Step != nil
	case "unit":
		return s.Unit != ""
	case "options":
		return len(s.Options) > 0
	}
	return false
}

// ParseSlotSchema 解析并严格校验后台提交的入参 schema（{schemaVersion,items}；兼容旧键 slots 与裸数组），返回规整后的 schema
// （补默认值、按 order 稳定排序）。空 / null / [] / items 为空返回 (nil, nil)，表示没有可替换位。
// 失败返回 *SlotsError（reason=SLOTS_INVALID，路径形如 items[2].options[1].value）。
func ParseSlotSchema(raw []byte) (*SlotSchema, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" || string(t) == "[]" {
		return nil, nil
	}
	bad := func(path, rule, msg string) error {
		return reject("入参 schema ", ReasonSlotsInvalid, []slotErr{{path, rule, msg}})
	}
	var slots []Slot
	switch t[0] {
	case '[':
		if err := strictDecode(t, &slots); err != nil {
			return nil, bad("items", "slots.json", "不是合法的入参数组："+err.Error())
		}
	case '{':
		var obj struct {
			SchemaVersion int             `json:"schemaVersion"`
			Items         json.RawMessage `json:"items"`
			Slots         json.RawMessage `json:"slots"` // 旧键（上线前的存储形态），只读兼容
		}
		if err := strictDecode(t, &obj); err != nil {
			return nil, bad("schemaVersion", "slots.json", "不是合法的入参 schema："+err.Error())
		}
		if obj.SchemaVersion != SlotSchemaVersion {
			return nil, bad("schemaVersion", "slots.version", fmt.Sprintf("schemaVersion 必须是 %d", SlotSchemaVersion))
		}
		list := obj.Items
		if isNullJSON(list) {
			list = obj.Slots
		} else if !isNullJSON(obj.Slots) {
			return nil, bad("items", "slots.both_keys", "items 与旧键 slots 不能同时出现")
		}
		if isNullJSON(list) {
			return nil, nil
		}
		if err := strictDecode(list, &slots); err != nil {
			return nil, bad("items", "slots.json", "items 必须是入参数组："+err.Error())
		}
	default:
		return nil, bad("items", "slots.json", "必须是对象 {schemaVersion, items}")
	}
	if len(slots) == 0 {
		return nil, nil
	}
	if errs := validateSlots2(slots); len(errs) > 0 {
		return nil, reject("入参 schema ", ReasonSlotsInvalid, errs)
	}
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].Order < slots[j].Order })
	return &SlotSchema{SchemaVersion: SlotSchemaVersion, Items: slots}, nil
}

func isNullJSON(r json.RawMessage) bool {
	t := bytes.TrimSpace(r)
	return len(t) == 0 || string(t) == "null"
}

// strictDecode 未知字段即拒收（拼错字段名不能被悄悄吞掉）。
func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("多余内容")
	}
	return nil
}

// Marshal 存库形态。
func (s *SlotSchema) Marshal() json.RawMessage {
	if s == nil {
		return nil
	}
	b, _ := json.Marshal(s)
	return b
}

func ip(i int) *int { return &i }

func validateSlots2(slots []Slot) []slotErr {
	var errs []slotErr
	add := func(path, rule, msg string) { errs = append(errs, slotErr{path, rule, msg}) }
	if len(slots) > maxSlots {
		add("items", "slots.too_many", fmt.Sprintf("最多 %d 个入参", maxSlots))
	}
	byKey := map[string]*Slot{}
	for i := range slots {
		p := fmt.Sprintf("items[%d]", i)
		sl := &slots[i]
		if !slotKeyRe.MatchString(sl.Key) {
			add(p+".key", "key.format", "key 必须是小写字母开头的字母、数字、下划线，最长 32")
		} else if byKey[sl.Key] != nil {
			add(p+".key", "key.duplicate", "key 在模板内必须唯一："+sl.Key)
		} else {
			byKey[sl.Key] = sl
		}
	}
	for i := range slots {
		p := fmt.Sprintf("items[%d]", i)
		sl := &slots[i]
		if !slotTypeSet[sl.Type] {
			add(p+".type", "type.invalid", "type 不合法："+sl.Type)
			continue
		}
		sl.Label = strings.TrimSpace(sl.Label)
		if sl.Label == "" || runeLen(sl.Label) > 32 {
			add(p+".label", "label.invalid", "label 必填，最多 32 个字")
		}
		if runeLen(sl.Help) > 200 {
			add(p+".help", "help.too_long", "help 最多 200 个字")
		}
		if runeLen(sl.Placeholder) > 100 {
			add(p+".placeholder", "placeholder.too_long", "placeholder 最多 100 个字")
		}
		if runeLen(sl.Group) > 16 {
			add(p+".group", "group.too_long", "group 最多 16 个字")
		}
		if sl.Order < 0 || sl.Order > 9999 {
			add(p+".order", "order.range", "order 需在 0~9999")
		}
		for field, types := range applicable {
			if sl.present(field) && !types[sl.Type] {
				add(p+"."+field, "field.not_applicable", fmt.Sprintf("%s 类型不支持 %s", sl.Type, field))
			}
		}
		switch sl.Type {
		case SlotImage, SlotVideo:
			errs = append(errs, validateMedia(p, sl)...)
		case SlotText, SlotTextarea:
			def := 100
			if sl.Type == SlotTextarea {
				def = 500
			}
			if sl.MaxLength == nil {
				sl.MaxLength = ip(def)
			}
			if *sl.MaxLength < 1 || *sl.MaxLength > maxTextLength {
				add(p+".maxLength", "maxLength.range", fmt.Sprintf("maxLength 需在 1~%d", maxTextLength))
			}
			if len(sl.Layout) > 0 {
				if t := bytes.TrimSpace(sl.Layout); string(t) == "null" {
					sl.Layout = nil // null 等于没有
				} else if sl.Type != SlotText {
					add(p+".layout", "layout.type", "文字叠加只支持 text 类型的入参（textarea 不叠）")
				} else if _, les := ParseLayout(sl.Layout, p+".layout"); len(les) > 0 {
					errs = append(errs, les...)
				}
			}
		case SlotNumber, SlotPrice:
			errs = append(errs, validateNumber(p, sl)...)
		case SlotSelect, SlotMultiselect:
			errs = append(errs, validateOptions(p, sl)...)
		}
		errs = append(errs, validateDefault(p, sl)...)
		if vw := sl.VisibleWhen; vw != nil {
			ref := byKey[vw.Key]
			switch {
			case strings.TrimSpace(vw.Key) == "":
				add(p+".visibleWhen.key", "visibleWhen.key_missing", "visibleWhen.key 必填（指向本模板内另一个入参的 key）")
			case ref == nil:
				add(p+".visibleWhen.key", "visibleWhen.unknown_key", "引用的入参不存在："+vw.Key)
			case ref == sl:
				add(p+".visibleWhen.key", "visibleWhen.self", "不能引用自己")
			case ref.VisibleWhen != nil:
				add(p+".visibleWhen.key", "visibleWhen.nested", "只支持单层条件，被引用的入参自己不能再有 visibleWhen")
			case !visibleRefTypes[ref.Type]:
				add(p+".visibleWhen.key", "visibleWhen.ref_type", "只能引用 select / switch / text 类入参")
			}
			if t := bytes.TrimSpace(vw.Equals); len(t) == 0 || string(t) == "null" {
				add(p+".visibleWhen.equals", "visibleWhen.equals_missing", "visibleWhen.equals 必填")
			} else if !isScalar(vw.Equals) {
				add(p+".visibleWhen.equals", "visibleWhen.equals", "equals 必须是字符串、数字或布尔值")
			} else if ref != nil && ref != sl && visibleRefTypes[ref.Type] {
				if rule, msg := equalsMatchesRef(ref, t); rule != "" {
					add(p+".visibleWhen.equals", rule, msg)
				}
			}
		}
	}
	return errs
}

// visibleRefTypes visibleWhen 可以引用的入参类型（契约 plaza-slot-schema-v1：select / switch / text）。
// equalsMatchesRef 里 number / price 的分支是为将来放开数字引用预留的同口径校验，目前走不到。
var visibleRefTypes = map[string]bool{SlotSelect: true, SlotSwitch: true, SlotText: true}

// equalsMatchesRef visibleWhen.equals 的类型须与被引用项一致（否则条件永远不成立，该项永远不显示）：
// switch → 布尔；select → 合法选项值；text → 字符串；number / price → 数字。返回命中的判据与原因，一致时 rule 为空。
func equalsMatchesRef(ref *Slot, eq []byte) (rule, msg string) {
	switch ref.Type {
	case SlotSwitch:
		if string(eq) != "true" && string(eq) != "false" {
			return "visibleWhen.equals_type", "引用的是开关（switch），equals 必须是 true / false"
		}
	case SlotSelect:
		var v string
		if eq[0] != '"' || json.Unmarshal(eq, &v) != nil {
			return "visibleWhen.equals_type", "引用的是单选（select），equals 必须是选项值字符串"
		}
		if !hasOption(ref, v) {
			return "visibleWhen.equals_option", "equals 不是被引用单选的可选项：" + v
		}
	case SlotText:
		if eq[0] != '"' {
			return "visibleWhen.equals_type", "引用的是文字（text），equals 必须是字符串"
		}
	case SlotNumber, SlotPrice:
		var f float64
		if eq[0] == '"' || json.Unmarshal(eq, &f) != nil {
			return "visibleWhen.equals_type", "引用的是数字（" + ref.Type + "），equals 必须是数字"
		}
	}
	return "", ""
}

func isScalar(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return false
	}
	return t[0] != '{' && t[0] != '['
}

func validateMedia(p string, sl *Slot) []slotErr {
	var errs []slotErr
	add := func(f, rule, msg string) { errs = append(errs, slotErr{p + "." + f, rule, msg}) }
	if sl.MaxCount == nil {
		sl.MaxCount = ip(1)
	}
	if sl.MinCount == nil {
		if sl.Required {
			sl.MinCount = ip(1)
		} else {
			sl.MinCount = ip(0)
		}
	}
	if *sl.MinCount < 0 || *sl.MaxCount < 1 || *sl.MaxCount > maxMediaCount {
		add("maxCount", "count.range", fmt.Sprintf("minCount ≥ 0，maxCount 需在 1~%d", maxMediaCount))
	}
	if *sl.MinCount > *sl.MaxCount {
		add("minCount", "count.min_gt_max", "minCount 不能大于 maxCount")
	}
	if sl.Required && *sl.MinCount < 1 {
		add("minCount", "count.required_zero", "必填的入参 minCount 至少为 1")
	}
	if runeLen(sl.Guidance) > 200 {
		add("guidance", "guidance.too_long", "guidance 最多 200 个字")
	}
	if sl.ExampleURL != "" && !validHTTPURL(sl.ExampleURL) {
		add("exampleUrl", "exampleUrl.invalid", "exampleUrl 必须是 http(s) 地址")
	}
	if sl.AcceptRatio != "" && !ratioRe.MatchString(sl.AcceptRatio) {
		add("acceptRatio", "acceptRatio.format", "acceptRatio 形如 正整数:正整数（如 9:16）")
	}
	if sl.Type == SlotVideo {
		if sl.MaxDurationSec == nil {
			sl.MaxDurationSec = ip(60)
		}
		if *sl.MaxDurationSec < 1 || *sl.MaxDurationSec > maxVideoSeconds {
			add("maxDurationSec", "duration.range", fmt.Sprintf("maxDurationSec 需在 1~%d", maxVideoSeconds))
		}
	}
	return errs
}

func validateNumber(p string, sl *Slot) []slotErr {
	var errs []slotErr
	add := func(f, rule, msg string) { errs = append(errs, slotErr{p + "." + f, rule, msg}) }
	for name, v := range map[string]*float64{"min": sl.Min, "max": sl.Max, "step": sl.Step} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			add(name, "number.invalid", name+" 不是有效数字")
		}
	}
	if sl.Min != nil && sl.Max != nil && *sl.Min > *sl.Max {
		add("min", "number.min_gt_max", "min 不能大于 max")
	}
	if sl.Step != nil && *sl.Step <= 0 {
		add("step", "number.step", "step 必须大于 0")
	}
	if runeLen(sl.Unit) > 8 {
		add("unit", "unit.too_long", "unit 最多 8 个字")
	}
	if sl.Type == SlotPrice && sl.Min == nil {
		sl.Min = fp(0)
	}
	return errs
}

func fp(f float64) *float64 { return &f }

func validateOptions(p string, sl *Slot) []slotErr {
	var errs []slotErr
	add := func(path, rule, msg string) { errs = append(errs, slotErr{path, rule, msg}) }
	if len(sl.Options) == 0 {
		add(p+".options", "options.empty", "select / multiselect 必须有 options")
		return errs
	}
	if len(sl.Options) > maxSlotOptions {
		add(p+".options", "options.too_many", fmt.Sprintf("options 最多 %d 个", maxSlotOptions))
	}
	seen := map[string]bool{}
	for i, o := range sl.Options {
		op := fmt.Sprintf("%s.options[%d]", p, i)
		if strings.TrimSpace(o.Value) == "" || runeLen(o.Value) > 64 {
			add(op+".value", "option.value", "value 必填，最多 64 个字符")
		} else if seen[o.Value] {
			add(op+".value", "option.duplicate", "value 重复："+o.Value)
		}
		seen[o.Value] = true
		if strings.TrimSpace(o.Label) == "" || runeLen(o.Label) > 32 {
			add(op+".label", "option.label", "label 必填，最多 32 个字")
		}
	}
	if sl.Type == SlotMultiselect {
		if sl.MaxCount != nil && (*sl.MaxCount < 1 || *sl.MaxCount > len(sl.Options)) {
			add(p+".maxCount", "count.range", "multiselect 的 maxCount 需在 1~选项数")
		}
	}
	return errs
}

// validateDefault 默认值按类型校验（复用提交校验的取值规则）。
func validateDefault(p string, sl *Slot) []slotErr {
	if len(sl.Default) == 0 || string(bytes.TrimSpace(sl.Default)) == "null" {
		return nil
	}
	if sl.Type == SlotImage || sl.Type == SlotVideo {
		return []slotErr{{p + ".default", "default.not_allowed", "图片 / 视频入参不支持 default"}}
	}
	if _, e := checkValue(sl, sl.Default); e != nil {
		return []slotErr{{p + ".default", "default." + e.rule, "默认值不合法：" + e.msg}}
	}
	return nil
}

// ---------------------------------------------------------------- 提交校验

type valErr struct{ rule, msg string }

// checkValue 校验一个取值是否符合入参定义，返回规整后的值。
func checkValue(sl *Slot, raw json.RawMessage) (any, *valErr) {
	t := bytes.TrimSpace(raw)
	switch sl.Type {
	case SlotImage, SlotVideo:
		var urls []string
		if len(t) > 0 && t[0] == '"' {
			var one string
			if json.Unmarshal(t, &one) != nil {
				return nil, &valErr{"type", "必须是地址或地址数组"}
			}
			urls = []string{one}
		} else if json.Unmarshal(t, &urls) != nil {
			return nil, &valErr{"type", "必须是地址或地址数组"}
		}
		mn, mx := 0, 1
		if sl.MinCount != nil {
			mn = *sl.MinCount
		}
		if sl.MaxCount != nil {
			mx = *sl.MaxCount
		}
		if len(urls) == 0 {
			return []string{}, nil
		}
		if len(urls) < mn {
			return nil, &valErr{"count.min", fmt.Sprintf("至少 %d 个", mn)}
		}
		if len(urls) > mx {
			return nil, &valErr{"count.max", fmt.Sprintf("最多 %d 个", mx)}
		}
		for _, u := range urls {
			if !validHTTPURL(strings.TrimSpace(u)) {
				return nil, &valErr{"url", "必须是 http(s) 地址"}
			}
		}
		return urls, nil
	case SlotText, SlotTextarea:
		var s string
		if json.Unmarshal(t, &s) != nil {
			return nil, &valErr{"type", "必须是文字"}
		}
		s = strings.TrimSpace(s)
		max := 100
		if sl.MaxLength != nil {
			max = *sl.MaxLength
		}
		if runeLen(s) > max {
			return nil, &valErr{"maxLength", fmt.Sprintf("最多 %d 个字", max)}
		}
		return s, nil
	case SlotNumber, SlotPrice:
		var f float64
		if len(t) == 0 || t[0] == '"' || json.Unmarshal(t, &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, &valErr{"type", "必须是数字"}
		}
		if sl.Min != nil && f < *sl.Min {
			return nil, &valErr{"min", fmt.Sprintf("不能小于 %v", *sl.Min)}
		}
		if sl.Max != nil && f > *sl.Max {
			return nil, &valErr{"max", fmt.Sprintf("不能大于 %v", *sl.Max)}
		}
		if sl.Step != nil {
			base := 0.0
			if sl.Min != nil {
				base = *sl.Min
			}
			q := (f - base) / *sl.Step
			if math.Abs(q-math.Round(q)) > 1e-6 {
				return nil, &valErr{"step", fmt.Sprintf("必须是 %v 的整数倍", *sl.Step)}
			}
		}
		return f, nil
	case SlotSelect:
		var s string
		if json.Unmarshal(t, &s) != nil {
			return nil, &valErr{"type", "必须是选项值"}
		}
		if !hasOption(sl, s) {
			return nil, &valErr{"option", "不是可选项：" + s}
		}
		return s, nil
	case SlotMultiselect:
		var vs []string
		if json.Unmarshal(t, &vs) != nil {
			return nil, &valErr{"type", "必须是选项值数组"}
		}
		seen := map[string]bool{}
		for _, v := range vs {
			if !hasOption(sl, v) {
				return nil, &valErr{"option", "不是可选项：" + v}
			}
			if seen[v] {
				return nil, &valErr{"option.duplicate", "选项重复：" + v}
			}
			seen[v] = true
		}
		if sl.MaxCount != nil && len(vs) > *sl.MaxCount {
			return nil, &valErr{"count.max", fmt.Sprintf("最多选 %d 个", *sl.MaxCount)}
		}
		if vs == nil {
			vs = []string{}
		}
		return vs, nil
	case SlotSwitch:
		var b bool
		if json.Unmarshal(t, &b) != nil {
			return nil, &valErr{"type", "必须是 true / false"}
		}
		return b, nil
	case SlotColor:
		var s string
		if json.Unmarshal(t, &s) != nil || !colorRe.MatchString(s) {
			return nil, &valErr{"color", "必须是 #RRGGBB"}
		}
		return strings.ToUpper(s), nil
	}
	return nil, &valErr{"type", "未知类型"}
}

func hasOption(sl *Slot, v string) bool {
	for _, o := range sl.Options {
		if o.Value == v {
			return true
		}
	}
	return false
}

// isEmptyValue 未填：缺省、null、空串、空数组。
func isEmptyValue(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null" || string(t) == `""` || string(t) == "[]"
}

// visible 按 visibleWhen 判断入参此刻是否显示：引用项的取值（已填，否则默认值）等于 equals。
func visible(sl *Slot, current map[string]json.RawMessage, bySlot map[string]*Slot) bool {
	if sl.VisibleWhen == nil {
		return true
	}
	val := current[sl.VisibleWhen.Key]
	if isEmptyValue(val) {
		if ref := bySlot[sl.VisibleWhen.Key]; ref != nil {
			val = ref.Default
		}
	}
	return scalarEqual(val, sl.VisibleWhen.Equals)
}

func scalarEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	if fx, ok := x.(float64); ok {
		fy, ok2 := y.(float64)
		return ok2 && fx == fy
	}
	return x == y
}

// ValidateInputs 做同款提交时按模板的 schema 校验入参（不信前端）：
// 未知 key 拒收；visibleWhen 当前不可见的项忽略（丢弃其值、不要求必填）；必填、数量、长度、范围、步长、选项合法；
// 缺省值取 default。返回规整后的入参（key → 值），拒收时错误带字段路径（inputs.<key>），并写日志说明判据。
// schema 为 nil（模板没有入参定义）时只允许空入参。
func ValidateInputs(schema *SlotSchema, inputs map[string]json.RawMessage) (map[string]any, error) {
	if len(inputs) > maxInputKeys {
		return nil, reject("做同款入参", "", []slotErr{{"inputs", "inputs.too_many",
			fmt.Sprintf("入参最多 %d 个键（收到 %d 个）", maxInputKeys, len(inputs))}})
	}
	var errs []slotErr
	bySlot := map[string]*Slot{}
	if schema != nil {
		for i := range schema.Items {
			bySlot[schema.Items[i].Key] = &schema.Items[i]
		}
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if bySlot[k] == nil {
			errs = append(errs, slotErr{"inputs." + k, "input.unknown", "模板没有这个入参"})
		}
	}
	out := map[string]any{}
	if schema != nil {
		for i := range schema.Items {
			sl := &schema.Items[i]
			path := "inputs." + sl.Key
			if !visible(sl, inputs, bySlot) {
				continue // 不可见项忽略
			}
			raw := inputs[sl.Key]
			// 未填：缺省 / null 一律算没填（switch 的 null 以前会被当成 false 放过必填）；非 switch 另含空串 / 空数组
			missing := isEmptyValue(raw)
			if sl.Type == SlotSwitch {
				missing = isNullJSON(raw)
			}
			if missing && len(sl.Default) > 0 && string(sl.Default) != "null" {
				raw, missing = sl.Default, false
			}
			if missing {
				if sl.Required {
					errs = append(errs, slotErr{path, "required", sl.Label + "必填"})
				}
				continue
			}
			v, e := checkValue(sl, raw)
			if e != nil {
				errs = append(errs, slotErr{path, e.rule, sl.Label + e.msg})
				continue
			}
			if (sl.Type == SlotImage || sl.Type == SlotVideo) && sl.Required {
				if mn := intOrDefault(sl.MinCount, 1); len(v.([]string)) < mn {
					errs = append(errs, slotErr{path, "count.min", fmt.Sprintf("%s至少 %d 个", sl.Label, mn)})
					continue
				}
			}
			out[sl.Key] = v
		}
	}
	if err := reject("做同款入参", "", errs); err != nil {
		return nil, err
	}
	return out, nil
}

func intOrDefault(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}
