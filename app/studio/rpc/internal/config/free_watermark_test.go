package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// 免费水印默认关；分层 / 展示水印默认值与 Java 一致（true）；没写 FreeWatermark 段时同样取这些默认。
func TestFreeWatermarkDefaults(t *testing.T) {
	var c Config
	if err := conf.Load("../../../../../deploy/etc/studio-rpc.yaml", &c); err != nil {
		t.Fatal(err)
	}
	fw := c.FreeWatermark
	if fw.Enabled || fw.Active() || !fw.UserTierDefault || !fw.DisplayWatermarkDefault || fw.SignedTTLSeconds != 600 || fw.PromoteMinutes != 10 {
		t.Fatalf("默认值不对：%+v", fw)
	}
	var bare Config
	if err := conf.LoadFromYamlBytes([]byte("Name: x\nListenOn: 0.0.0.0:1\nDataSource: x\nBilling: {}\nStorage: {Mode: local}\nAsset: {}\n"), &bare); err != nil {
		t.Fatal(err)
	}
	if b := bare.FreeWatermark; b.Enabled || !b.UserTierDefault || !b.DisplayWatermarkDefault || b.PromoteMinutes != 10 {
		t.Fatalf("缺省段默认值不对：%+v", b)
	}
}
