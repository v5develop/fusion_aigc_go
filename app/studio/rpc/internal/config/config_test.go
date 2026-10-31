package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// 开发配置必须能加载（mkconf 以它为模板生成服务器配置），关键默认值与前缀不能丢。
func TestLoadDevConfig(t *testing.T) {
	var c Config
	if err := conf.Load("../../../../../deploy/etc/studio-rpc.yaml", &c); err != nil {
		t.Fatal(err)
	}
	if c.ListenOn != "0.0.0.0:9205" {
		t.Fatalf("端口应为 9205：%s", c.ListenOn)
	}
	if c.TaskIDPrefix.Tryon != "tyo" || c.TaskIDPrefix.Pose != "pse" || c.TaskIDPrefix.ImageGen != "simg" {
		t.Fatalf("前缀不对：%+v", c.TaskIDPrefix)
	}
	if c.Reverse.BaseURL != "http://127.0.0.1:8090" || c.Timeouts.Generate != 300 || c.AiGateway.LongPollWait != 20 {
		t.Fatalf("默认值不对：%+v %+v %+v", c.Reverse, c.Timeouts, c.AiGateway)
	}
	if c.ModelGroupMap()["Banana"] != "img.nano-banana-2.1" {
		t.Fatalf("模型映射不对：%v", c.ModelGroupMap())
	}
}
