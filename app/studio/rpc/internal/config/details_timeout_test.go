package config

import (
	"testing"
	"time"

	"github.com/zeromicro/go-zero/zrpc"
)

func TestEnsureDetailsTimeout(t *testing.T) {
	var c Config
	c.EnsureDetailsTimeout()
	c.EnsureDetailsTimeout()
	if len(c.MethodTimeouts) != 1 || c.MethodTimeouts[0].FullMethod != DetectDetailsMethod || c.MethodTimeouts[0].Timeout != 240*time.Second {
		t.Fatalf("MethodTimeouts = %+v", c.MethodTimeouts)
	}
	c = Config{}
	c.MethodTimeouts = []zrpc.MethodTimeoutConf{{FullMethod: DetectDetailsMethod, Timeout: time.Minute}}
	c.EnsureDetailsTimeout()
	if len(c.MethodTimeouts) != 1 || c.MethodTimeouts[0].Timeout != time.Minute {
		t.Fatalf("已配置的应保留：%+v", c.MethodTimeouts)
	}
}
