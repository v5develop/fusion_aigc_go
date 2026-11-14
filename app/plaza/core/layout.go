package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"aigc-local-life/pkg/mediasvc"
)

// 文字位 layout v1（DESIGN 17.3，P1）：模板里 type=text 的入参可带 layout，出图后按它把用户填的文字叠到成品图上。
// 取值都是相对画面的比例（画面尺寸在叠字时才知道）：
//
//	box{x,y,w,h}  0~1，w、h>0，x+w≤1、y+h≤1（必填）
//	size          字号，相对画面高度 (0,0.5]；缺省 / 0 = 按框高自适应
//	color         #RGB / #RRGGBB / #RRGGBBAA，缺省白
//	align         left|center|right，缺省 center；valign top|middle|bottom，缺省 middle
//	stroke        {color?, width}，width 相对画面高度 [0,0.05]
//	maxLines      1~10；lineHeight 0.8~3
//	fontRid       媒体服务字体 rid；缺省用 Plaza.Overlay.DefaultFontRid
//	defaultText   样例 / 默认文字 ≤100 字
//
// 本规格之外的键原样保存、不报错（add-only）。

const (
	layoutMaxSize       = 0.5
	layoutMaxStroke     = 0.05
	layoutMaxLines      = 10
	layoutMinLineHeight = 0.8
	layoutMaxLineHeight = 3.0
	layoutMaxDefaultLen = 100
)

var layoutColorRe = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// TextLayout layout v1 的解析结果。
type TextLayout struct {
	Box struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	} `json:"box"`
	Size   float64 `json:"size,omitempty"`
	Color  string  `json:"color,omitempty"`
	Align  string  `json:"align,omitempty"`
	Valign string  `json:"valign,omitempty"`
	Stroke *struct {
		Color string  `json:"color,omitempty"`
		Width float64 `json:"width"`
	} `json:"stroke,omitempty"`
	MaxLines    int     `json:"maxLines,omitempty"`
	LineHeight  float64 `json:"lineHeight,omitempty"`
	FontRid     string  `json:"fontRid,omitempty"`
	DefaultText string  `json:"defaultText,omitempty"`
}

// ParseLayout 解析并校验 layout；p 是字段路径前缀（如 items[0].layout），错误按路径列出。
func ParseLayout(raw json.RawMessage, p string) (*TextLayout, []slotErr) {
	var errs []slotErr
	add := func(path, rule, msg string) { errs = append(errs, slotErr{p + path, rule, msg}) }
	dec := json.NewDecoder(bytes.NewReader(raw))
	var l TextLayout
	if err := dec.Decode(&l); err != nil {
		var o map[string]json.RawMessage
		if json.Unmarshal(raw, &o) != nil {
			return nil, []slotErr{{p, "layout.not_object", "layout 必须是对象"}}
		}
		// 是对象但某个字段类型不对：逐项指出
		return nil, []slotErr{{p, "layout.field_type", "layout 字段类型不对（box 要对象，size / lineHeight 要数字，maxLines 要整数，其余要字符串）：" + err.Error()}}
	}
	var present map[string]json.RawMessage
	_ = json.Unmarshal(raw, &present)
	if len(present["box"]) == 0 || string(bytes.TrimSpace(present["box"])) == "null" {
		add(".box", "layout.box_missing", "box 必填")
		return &l, errs
	}
	b := l.Box
	for _, v := range []float64{b.X, b.Y, b.W, b.H} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			add(".box", "layout.box_range", "box 不是合法数字")
			return &l, errs
		}
	}
	const eps = 1e-6
	switch {
	case b.X < 0 || b.X > 1:
		add(".box.x", "layout.box_range", "box.x 需在 0~1")
	case b.Y < 0 || b.Y > 1:
		add(".box.y", "layout.box_range", "box.y 需在 0~1")
	case b.W <= 0 || b.W > 1:
		add(".box.w", "layout.box_range", "box.w 需在 (0,1]")
	case b.H <= 0 || b.H > 1:
		add(".box.h", "layout.box_range", "box.h 需在 (0,1]")
	case b.X+b.W > 1+eps:
		add(".box.w", "layout.box_overflow", "box.x + box.w 不能超过 1")
	case b.Y+b.H > 1+eps:
		add(".box.h", "layout.box_overflow", "box.y + box.h 不能超过 1")
	}
	if l.Size < 0 || l.Size > layoutMaxSize || math.IsNaN(l.Size) {
		add(".size", "layout.size_range", fmt.Sprintf("size（相对画面高度）需在 0~%g，0 或不填表示按框自适应", layoutMaxSize))
	}
	if l.Color != "" && !layoutColorRe.MatchString(l.Color) {
		add(".color", "layout.color_format", "color 需是 #RGB / #RRGGBB / #RRGGBBAA")
	}
	if l.Align != "" && l.Align != "left" && l.Align != "center" && l.Align != "right" {
		add(".align", "layout.align_enum", "align 只能是 left / center / right")
	}
	if l.Valign != "" && l.Valign != "top" && l.Valign != "middle" && l.Valign != "bottom" {
		add(".valign", "layout.valign_enum", "valign 只能是 top / middle / bottom")
	}
	if s := l.Stroke; s != nil {
		if s.Width < 0 || s.Width > layoutMaxStroke || math.IsNaN(s.Width) {
			add(".stroke.width", "layout.stroke_range", fmt.Sprintf("stroke.width（相对画面高度）需在 0~%g", layoutMaxStroke))
		}
		if s.Color != "" && !layoutColorRe.MatchString(s.Color) {
			add(".stroke.color", "layout.color_format", "stroke.color 需是 #RGB / #RRGGBB / #RRGGBBAA")
		}
	}
	if l.MaxLines != 0 && (l.MaxLines < 1 || l.MaxLines > layoutMaxLines) {
		add(".maxLines", "layout.max_lines_range", fmt.Sprintf("maxLines 需在 1~%d", layoutMaxLines))
	}
	if l.LineHeight != 0 && (l.LineHeight < layoutMinLineHeight || l.LineHeight > layoutMaxLineHeight || math.IsNaN(l.LineHeight)) {
		add(".lineHeight", "layout.line_height_range", fmt.Sprintf("lineHeight 需在 %g~%g", layoutMinLineHeight, layoutMaxLineHeight))
	}
	if l.FontRid != "" && !mediasvc.ValidRID(l.FontRid) {
		add(".fontRid", "layout.font_rid_format", "fontRid 格式不合法（形如 xxxxxx-asset-<26 位小写字母数字>）")
	}
	if utf8.RuneCountInString(l.DefaultText) > layoutMaxDefaultLen || strings.ContainsRune(l.DefaultText, 0) {
		add(".defaultText", "layout.default_text_len", fmt.Sprintf("defaultText 不超过 %d 字", layoutMaxDefaultLen))
	}
	return &l, errs
}

// ToSlot 把 layout + 文字 + 画面高度（像素）换成媒体服务的文字槽位。fallbackFont 在 layout 没给字体时使用。
func (l *TextLayout) ToSlot(text string, imgH int, fallbackFont string) mediasvc.TextSlot {
	sl := mediasvc.TextSlot{
		Text:       text,
		Box:        mediasvc.TextBox{X: l.Box.X, Y: l.Box.Y, W: l.Box.W, H: l.Box.H},
		FontRid:    l.FontRid,
		Color:      l.Color,
		Align:      l.Align,
		Valign:     l.Valign,
		MaxLines:   l.MaxLines,
		LineHeight: l.LineHeight,
	}
	if sl.FontRid == "" {
		sl.FontRid = fallbackFont
	}
	if l.Size > 0 && imgH > 0 {
		sl.Size = clampInt(int(math.Round(l.Size*float64(imgH))), 8, 1024)
	}
	if s := l.Stroke; s != nil && s.Width > 0 && imgH > 0 {
		w := math.Round(s.Width * float64(imgH))
		if w < 1 {
			w = 1
		}
		if w > 24 {
			w = 24
		}
		sl.Stroke = &mediasvc.TextStroke{Color: s.Color, Width: w}
	}
	return sl
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// noTextPromptRe 提示词里「画面中不要出现任何文字」类约束（text_overlay=1 的模板需要它，否则模型自己画的字会和叠上去的字重叠）。
var noTextPromptRe = regexp.MustCompile(`(?i)(不要|不能|不得|禁止|避免|无|没有|不含|不带|不出现|勿|without|no)[^，。,.;；\n]{0,16}(文字|文本|字样|字幕|汉字|text|letters|caption)`)

// overlayPromptWarnings text_overlay=1 的模板提示词没有「不要出现任何文字」类约束时给后台提示（不拦截保存）。
func overlayPromptWarnings(textOverlay int8, dataJSON []byte) []string {
	if textOverlay != 1 || noTextPromptRe.MatchString(topString(dataJSON, "prompt")) {
		return nil
	}
	return []string{"已开启「文字走后期叠加」，但提示词里没有「画面中不要出现任何文字」之类的约束：模型自己画的字会和叠上去的字重叠，请在提示词里补上"}
}
