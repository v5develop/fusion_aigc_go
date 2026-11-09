package config

import (
	"time"

	"github.com/zeromicro/go-zero/zrpc"
)

// SegmentMaskMethod / SegmentMaskTimeout 区域蒙版要等一次分割调用（网关同步 / 异步抠图 + 下载 + 转存，rpc 侧总时限 120s），
// 远超 RPC 默认 30s：没在 MethodTimeouts 里单独配时由 EnsureMaskTimeout 补上。gateway-api 侧同样按方法放宽调用超时（135s）。
const (
	SegmentMaskMethod  = "/studio.Studio/SegmentMask"
	SegmentMaskTimeout = 130 * time.Second
)

// EnsureMaskTimeout 给 SegmentMask 补默认的服务端超时（已配置的不动）。
func (c *Config) EnsureMaskTimeout() {
	for _, m := range c.MethodTimeouts {
		if m.FullMethod == SegmentMaskMethod {
			return
		}
	}
	c.MethodTimeouts = append(c.MethodTimeouts, zrpc.MethodTimeoutConf{FullMethod: SegmentMaskMethod, Timeout: SegmentMaskTimeout})
}
