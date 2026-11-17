package httpapi

import (
	"testing"

	"aigc-local-life/app/replicate/core"
)

// 发布文案：开关与每日额度的运行时覆盖（数据库 > yaml）；非法值忽略；进签名。
func TestApplyParamsCopyOverride(t *testing.T) {
	base := core.DefaultConfig()
	base.CopyOff, base.CopyDailyQuota = false, 20
	if p := applyParams(base, true, nil); p.Cfg.CopyOff || p.Cfg.CopyDailyQuota != 20 {
		t.Fatalf("没配时用 yaml：%+v", p.Cfg)
	}
	p := applyParams(base, true, map[string]string{KeyCopyEnabled: "false", KeyCopyDailyQuota: "50"})
	if !p.Cfg.CopyOff || p.Cfg.CopyDailyQuota != 50 {
		t.Fatalf("库里的值应覆盖 yaml：%+v", p.Cfg)
	}
	for _, bad := range []map[string]string{{KeyCopyEnabled: "maybe"}, {KeyCopyDailyQuota: "0"}, {KeyCopyDailyQuota: "abc"}, {KeyCopyDailyQuota: "10001"}} {
		if q := applyParams(base, true, bad); q.Cfg.CopyOff || q.Cfg.CopyDailyQuota != 20 {
			t.Fatalf("非法值应忽略并用 yaml：%v → %+v", bad, q.Cfg)
		}
	}
	s0 := signature(applyParams(base, true, nil))
	if signature(applyParams(base, true, map[string]string{KeyCopyDailyQuota: "9"})) == s0 ||
		signature(applyParams(base, true, map[string]string{KeyCopyEnabled: "false"})) == s0 {
		t.Fatal("文案参数应进签名（变了要重建内核）")
	}
}
