package httpapi

import (
	"testing"

	"aigc-local-life/app/replicate/core"
)

// 定妆图开关 / 每日额度：库里有且合法 → 覆盖 yaml；缺省或非法 → 用 yaml（数据库 > 文件配置）。
func TestApplyParamsPreviewOverride(t *testing.T) {
	base := core.DefaultConfig()
	base.PreviewOff, base.PreviewDailyQuota = false, 3
	if p := applyParams(base, true, nil); p.Cfg.PreviewOff || p.Cfg.PreviewDailyQuota != 3 {
		t.Fatalf("没配时用 yaml：%+v", p.Cfg)
	}
	p := applyParams(base, true, map[string]string{KeyPreviewEnabled: "false", KeyPreviewDailyQuota: "10"})
	if !p.Cfg.PreviewOff || p.Cfg.PreviewDailyQuota != 10 {
		t.Fatalf("库里的值应覆盖 yaml：%+v", p.Cfg)
	}
	// yaml 关着、库里开 → 以库为准
	off := base
	off.PreviewOff = true
	if q := applyParams(off, true, map[string]string{KeyPreviewEnabled: "true"}); q.Cfg.PreviewOff {
		t.Fatal("库里 true 应覆盖 yaml 的关闭")
	}
	for _, bad := range []map[string]string{
		{KeyPreviewEnabled: "maybe"}, {KeyPreviewDailyQuota: "0"}, {KeyPreviewDailyQuota: "-1"}, {KeyPreviewDailyQuota: "abc"}, {KeyPreviewDailyQuota: "1001"},
	} {
		if q := applyParams(base, true, bad); q.Cfg.PreviewOff || q.Cfg.PreviewDailyQuota != 3 {
			t.Fatalf("非法值应忽略并用 yaml：%v → %+v", bad, q.Cfg)
		}
	}
	s0 := signature(applyParams(base, true, nil))
	if signature(applyParams(base, true, map[string]string{KeyPreviewEnabled: "false"})) == s0 ||
		signature(applyParams(base, true, map[string]string{KeyPreviewDailyQuota: "9"})) == s0 {
		t.Fatal("定妆图参数应进签名（变了要重建内核）")
	}
}

// 坏片判定：开关与两个比例阈值的运行时覆盖。
func TestApplyParamsBadOutputOverride(t *testing.T) {
	base := core.DefaultConfig()
	if base.BadOutputOff || base.BadOutputBlackRatio != 0.6 || base.BadOutputFreezeRatio != 0.8 {
		t.Fatalf("默认：开、黑屏 0.6、定格 0.8：%+v", base)
	}
	p := applyParams(base, true, map[string]string{KeyBadOutputEnabled: "off", KeyBadOutputBlackRatio: "0.5", KeyBadOutputFreezeRatio: "1"})
	if !p.Cfg.BadOutputOff || p.Cfg.BadOutputBlackRatio != 0.5 || p.Cfg.BadOutputFreezeRatio != 1 {
		t.Fatalf("库里的值应覆盖：%+v", p.Cfg)
	}
	for _, bad := range []map[string]string{
		{KeyBadOutputBlackRatio: "0"}, {KeyBadOutputBlackRatio: "1.5"}, {KeyBadOutputFreezeRatio: "-0.1"}, {KeyBadOutputFreezeRatio: "x"},
	} {
		if q := applyParams(base, true, bad); q.Cfg.BadOutputBlackRatio != 0.6 || q.Cfg.BadOutputFreezeRatio != 0.8 {
			t.Fatalf("非法值应忽略并用默认：%v → %+v", bad, q.Cfg)
		}
	}
	if signature(applyParams(base, true, map[string]string{KeyBadOutputBlackRatio: "0.7"})) == signature(applyParams(base, true, nil)) {
		t.Fatal("阈值应进签名")
	}
}
