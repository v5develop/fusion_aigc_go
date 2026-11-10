package config

import (
	"time"

	"github.com/zeromicro/go-zero/zrpc"
)

// DetectDetailsMethod / DetectDetailsTimeout 商拍「AI 找细节」要下载压缩服装图 + 一次 sidecar crop-plan（服装图 >6 张分批，
// 单次读图可达 180s），远超 RPC 默认 30s：没在 MethodTimeouts 里单独配时由 EnsureDetailsTimeout 补上（与配对预览同为 240s）。
const (
	DetectDetailsMethod  = "/studio.Studio/DetectDetails"
	DetectDetailsTimeout = 240 * time.Second
)

// EnsureDetailsTimeout 给 DetectDetails 补默认的服务端超时（已配置的不动）。
func (c *Config) EnsureDetailsTimeout() {
	for _, m := range c.MethodTimeouts {
		if m.FullMethod == DetectDetailsMethod {
			return
		}
	}
	c.MethodTimeouts = append(c.MethodTimeouts, zrpc.MethodTimeoutConf{FullMethod: DetectDetailsMethod, Timeout: DetectDetailsTimeout})
}
