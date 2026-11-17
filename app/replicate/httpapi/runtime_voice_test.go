package httpapi

import (
	"testing"

	"aigc-local-life/app/replicate/core"
)

// 换声音：开关与每日额度的运行时覆盖（数据库 > yaml）；非法值忽略；进签名。
func TestApplyParamsVoiceOverride(t *testing.T) {
	base := core.DefaultConfig()
	if p := applyParams(base, true, nil); p.Cfg.VoiceOff || p.Cfg.VoiceDailyQuota != 10 {
		t.Fatalf("没配时用 yaml 默认（10 条 / 天）：%+v", p.Cfg)
	}
	p := applyParams(base, true, map[string]string{KeyVoiceEnabled: "false", KeyVoiceDailyQuota: "30"})
	if !p.Cfg.VoiceOff || p.Cfg.VoiceDailyQuota != 30 {
		t.Fatalf("库里的值应覆盖 yaml：%+v", p.Cfg)
	}
	for _, bad := range []map[string]string{{KeyVoiceEnabled: "maybe"}, {KeyVoiceDailyQuota: "0"}, {KeyVoiceDailyQuota: "x"}, {KeyVoiceDailyQuota: "10001"}} {
		if q := applyParams(base, true, bad); q.Cfg.VoiceOff || q.Cfg.VoiceDailyQuota != 10 {
			t.Fatalf("非法值应忽略：%v → %+v", bad, q.Cfg)
		}
	}
	s0 := signature(applyParams(base, true, nil))
	if signature(applyParams(base, true, map[string]string{KeyVoiceDailyQuota: "9"})) == s0 ||
		signature(applyParams(base, true, map[string]string{KeyVoiceEnabled: "false"})) == s0 {
		t.Fatal("换声音参数应进签名")
	}
}
