package logic

import (
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/copywriting/model"
)

// 字数按设计稿便签的口径：汉字算 2，其余算 1（店铺名称 20 字符 = 10 个汉字）。
func TestCharLen(t *testing.T) {
	cases := map[string]int{"": 0, "abc": 3, "咖啡店": 6, "a咖啡": 5, "一二三四五六七八九十": 20, "，。": 4}
	for in, want := range cases {
		if got := charLen(in); got != want {
			t.Errorf("charLen(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestProfileText(t *testing.T) {
	p := &model.AfvShopProfile{Name: "西湖边网红咖啡店"}
	p.OpenDate = nullStr(time.Now().AddDate(-1, -2, 0).Format("2006-01-02"))
	p.SelfTitle = nullStr("店长小林")
	got := profileText(p)
	if !strings.Contains(got, "店铺名称：西湖边网红咖啡店") || !strings.Contains(got, "如何称呼自己：店长小林") {
		t.Fatalf("缺字段: %s", got)
	}
	if !strings.Contains(got, "已开店 1 年 2 个月") {
		t.Fatalf("开店时间没换算: %s", got)
	}
	// 空字段跳过，不留空行
	if strings.Contains(got, "店铺地址") || strings.Contains(got, "\n\n") {
		t.Fatalf("空字段没跳过: %q", got)
	}
}

func TestOpenDateText(t *testing.T) {
	now := time.Now()
	cases := []struct{ in, want string }{
		{"", ""},
		{"3 年", "3 年"}, // 解析不了就原样用
		{now.AddDate(0, -3, 0).Format("2006-01-02"), "已开店 3 个月"},
		{now.AddDate(-2, 0, 0).Format("2006-01-02"), "已开店 2 年"},
		{now.AddDate(1, 0, 0).Format("2006-01-02"), ""}, // 未来日期不换算
	}
	for _, c := range cases {
		got := openDateText(c.in)
		if c.want == "" {
			if got != c.in {
				t.Errorf("openDateText(%q) = %q, want 原样", c.in, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("openDateText(%q) = %q, want 含 %q", c.in, got, c.want)
		}
	}
}
