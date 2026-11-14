package core

import (
	"encoding/json"
	"strings"
	"testing"
)

const goodRid = "abc123-asset-aaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestParseLayout(t *testing.T) {
	ok := `{"box":{"x":0.1,"y":0.8,"w":0.8,"h":0.1},"size":0.05,"color":"#FFAA00","align":"center","valign":"middle",
		"stroke":{"color":"#000000","width":0.004},"maxLines":2,"lineHeight":1.2,"fontRid":"` + goodRid + `","defaultText":"某某足浴","extra":1}`
	if l, errs := ParseLayout(json.RawMessage(ok), "items[0].layout"); len(errs) != 0 || l == nil {
		t.Fatalf("合法 layout 被拒: %v", errs)
	}
	cases := []struct{ name, raw, path, rule string }{
		{"不是对象", `[1]`, "items[0].layout", "layout.not_object"},
		{"字段类型错", `{"box":"x"}`, "items[0].layout", "layout.field_type"},
		{"缺 box", `{"size":0.1}`, "items[0].layout.box", "layout.box_missing"},
		{"x 越界", `{"box":{"x":1.2,"y":0,"w":0.1,"h":0.1}}`, "items[0].layout.box.x", "layout.box_range"},
		{"w 为 0", `{"box":{"x":0,"y":0,"w":0,"h":0.1}}`, "items[0].layout.box.w", "layout.box_range"},
		{"x+w 超出", `{"box":{"x":0.6,"y":0,"w":0.5,"h":0.1}}`, "items[0].layout.box.w", "layout.box_overflow"},
		{"y+h 超出", `{"box":{"x":0,"y":0.95,"w":0.5,"h":0.1}}`, "items[0].layout.box.h", "layout.box_overflow"},
		{"字号过大", `{"box":{"x":0,"y":0,"w":1,"h":1},"size":0.9}`, "items[0].layout.size", "layout.size_range"},
		{"字号为负", `{"box":{"x":0,"y":0,"w":1,"h":1},"size":-0.1}`, "items[0].layout.size", "layout.size_range"},
		{"颜色格式", `{"box":{"x":0,"y":0,"w":1,"h":1},"color":"red"}`, "items[0].layout.color", "layout.color_format"},
		{"颜色缺 #", `{"box":{"x":0,"y":0,"w":1,"h":1},"color":"FFFFFF"}`, "items[0].layout.color", "layout.color_format"},
		{"描边色", `{"box":{"x":0,"y":0,"w":1,"h":1},"stroke":{"color":"zz","width":0.01}}`, "items[0].layout.stroke.color", "layout.color_format"},
		{"描边过粗", `{"box":{"x":0,"y":0,"w":1,"h":1},"stroke":{"width":0.5}}`, "items[0].layout.stroke.width", "layout.stroke_range"},
		{"对齐", `{"box":{"x":0,"y":0,"w":1,"h":1},"align":"justify"}`, "items[0].layout.align", "layout.align_enum"},
		{"垂直对齐", `{"box":{"x":0,"y":0,"w":1,"h":1},"valign":"mid"}`, "items[0].layout.valign", "layout.valign_enum"},
		{"最大行数", `{"box":{"x":0,"y":0,"w":1,"h":1},"maxLines":11}`, "items[0].layout.maxLines", "layout.max_lines_range"},
		{"行高", `{"box":{"x":0,"y":0,"w":1,"h":1},"lineHeight":5}`, "items[0].layout.lineHeight", "layout.line_height_range"},
		{"字体 rid", `{"box":{"x":0,"y":0,"w":1,"h":1},"fontRid":"font-1"}`, "items[0].layout.fontRid", "layout.font_rid_format"},
		{"默认文字过长", `{"box":{"x":0,"y":0,"w":1,"h":1},"defaultText":"` + strings.Repeat("字", 101) + `"}`, "items[0].layout.defaultText", "layout.default_text_len"},
	}
	for _, c := range cases {
		_, errs := ParseLayout(json.RawMessage(c.raw), "items[0].layout")
		if len(errs) == 0 {
			t.Errorf("%s: 应被拒", c.name)
			continue
		}
		if errs[0].path != c.path || errs[0].rule != c.rule {
			t.Errorf("%s: got %s/%s want %s/%s", c.name, errs[0].path, errs[0].rule, c.path, c.rule)
		}
	}
}

func TestLayoutToSlot(t *testing.T) {
	l, errs := ParseLayout(json.RawMessage(`{"box":{"x":0.1,"y":0.2,"w":0.5,"h":0.1},"size":0.05,"stroke":{"color":"#000","width":0.004},"maxLines":2}`), "l")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	sl := l.ToSlot("店名", 2000, goodRid)
	if sl.Size != 100 || sl.Stroke == nil || sl.Stroke.Width != 8 || sl.FontRid != goodRid || sl.MaxLines != 2 || sl.Box.W != 0.5 {
		t.Fatalf("换算不对: %+v stroke=%+v", sl, sl.Stroke)
	}
	if sl2 := l.ToSlot("x", 0, ""); sl2.Size != 0 || sl2.Stroke != nil {
		t.Fatal("未知画面高度时不换算像素")
	}
}
