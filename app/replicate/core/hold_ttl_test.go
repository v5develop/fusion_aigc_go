package core

import (
	"testing"
	"time"

	"aigc-local-life/pkg/billing"
)

// 续期时刻必须随预留有效期推导：既不早到把到期时间缩短，也不晚到预留单先被回收。
func TestRenewAfterFollowsHoldTTL(t *testing.T) {
	for _, ttl := range []time.Duration{60 * time.Minute, 200 * time.Minute, 300 * time.Minute} {
		c := Config{HoldTTL: ttl}.withDefaults()
		if c.RenewAfter < ttl-billing.LeaseDuration {
			t.Fatalf("HoldTTL=%v: RenewAfter=%v 早于 HoldTTL−租约，续期会缩短到期时间", ttl, c.RenewAfter)
		}
		if c.RenewAfter >= ttl {
			t.Fatalf("HoldTTL=%v: RenewAfter=%v 不早于到期，预留单会先被回收", ttl, c.RenewAfter)
		}
	}
}
