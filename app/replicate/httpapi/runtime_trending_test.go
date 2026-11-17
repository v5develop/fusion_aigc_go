package httpapi

import (
	"testing"

	"aigc-local-life/app/replicate/core"
)

// 灵感推荐：k-匿名阈值的运行时覆盖（数据库 > yaml）；非法值忽略；进签名（改了要重建内核）。
func TestApplyParamsTrendingMinUsersOverride(t *testing.T) {
	base := core.DefaultConfig()
	base.TrendingMinUsers = 20
	if p := applyParams(base, true, nil); p.Cfg.TrendingMinUsers != 20 {
		t.Fatalf("没配时用 yaml（20）：%+v", p.Cfg)
	}
	if p := applyParams(base, true, map[string]string{KeyTrendingMinUsers: "50"}); p.Cfg.TrendingMinUsers != 50 {
		t.Fatalf("库里的值应覆盖 yaml：%+v", p.Cfg)
	}
	for _, bad := range []string{"0", "-1", "x", "100001", ""} {
		if p := applyParams(base, true, map[string]string{KeyTrendingMinUsers: bad}); p.Cfg.TrendingMinUsers != 20 {
			t.Fatalf("非法值 %q 应忽略：%+v", bad, p.Cfg)
		}
	}
	if signature(applyParams(base, true, map[string]string{KeyTrendingMinUsers: "30"})) == signature(applyParams(base, true, nil)) {
		t.Fatal("阈值应进签名")
	}
}
