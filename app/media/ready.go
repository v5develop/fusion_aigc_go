package media

import (
	"context"
	"errors"
	"strings"
	"time"

	"aigc-local-life/pkg/health"
)

// readyProbeKey 就绪探测用的对象键：格式合法（idx/<26 位 base32>.json）但不会真实存在，
// HEAD 它得到「不存在」即说明对象存储可达、凭据有效；其它错误（超时、403、5xx）都算不可用。
var readyProbeKey = "idx/" + strings.Repeat("a", 26) + ".json"

// newReady 构造 /readyz：对象存储可达，且（幂等存储用 Redis 时）Redis 能 PING。结果缓存几秒，探针再密也不放大对存储的压力。
func (s *Server) newReady() *health.Ready {
	checks := []health.Check{{Name: "object_store", Fn: func(ctx context.Context) error {
		_, err := s.objects.Head(ctx, storePrivate, readyProbeKey)
		if err == nil || errors.Is(err, errObjectNotFound) {
			return nil
		}
		return err
	}}}
	if p, ok := s.idem.(interface{ ping(context.Context) error }); ok {
		checks = append(checks, health.Check{Name: "idempotency_redis", Fn: p.ping})
	}
	return health.NewReady(health.Options{Timeout: 3 * time.Second, TTL: 3 * time.Second}, checks...)
}
