package httpapi

import (
	"testing"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/rules/analysisfix"
)

func TestApplyParamsConfirmThresholdAndAnalysisRules(t *testing.T) {
	base := core.DefaultConfig()
	if d := applyParams(base, true, nil); d.Cfg.ConfirmThresholdPoints != 0 || analysisfix.VersionOf(d.Cfg.AnalysisRules) != analysisfix.Version {
		t.Fatalf("默认：阈值 0（不启用）、规则默认版本：%+v", d.Cfg)
	}
	p := applyParams(base, true, map[string]string{
		KeyConfirmThreshold: "500",
		KeyAnalysisRules:    `{"disabled":["LINE_DUPLICATE"],"durationTolerance":0.2}`,
	})
	if p.Cfg.ConfirmThresholdPoints != 500 {
		t.Fatalf("阈值应生效：%d", p.Cfg.ConfirmThresholdPoints)
	}
	if v := analysisfix.VersionOf(p.Cfg.AnalysisRules); v == analysisfix.Version || len(p.Cfg.AnalysisRules.Disabled) != 1 {
		t.Fatalf("规则配置应生效并改变版本：%s %+v", v, p.Cfg.AnalysisRules)
	}
	for _, bad := range []map[string]string{
		{KeyConfirmThreshold: "-1"}, {KeyConfirmThreshold: "abc"}, {KeyConfirmThreshold: "100000001"},
		{KeyAnalysisRules: `{"disabled":["NOPE"]}`}, {KeyAnalysisRules: `not json`},
	} {
		q := applyParams(base, true, bad)
		if q.Cfg.ConfirmThresholdPoints != 0 || analysisfix.VersionOf(q.Cfg.AnalysisRules) != analysisfix.Version {
			t.Fatalf("非法值应忽略并用默认：%v → %+v", bad, q.Cfg)
		}
	}
	// 运行参数变了要重建内核：阈值 / 规则版本都进签名
	s0 := signature(applyParams(base, true, nil))
	if signature(applyParams(base, true, map[string]string{KeyConfirmThreshold: "1"})) == s0 ||
		signature(applyParams(base, true, map[string]string{KeyAnalysisRules: `{"disabled":["LINE_DUPLICATE"]}`})) == s0 {
		t.Fatal("阈值与规则配置应进签名")
	}
}
