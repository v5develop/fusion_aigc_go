package billing

import (
	"context"
	"testing"
	"time"
)

// 计费关闭或 shadow 时不落库：Settle 与 Renew 直接返回 nil，conn 为 nil 也不能 panic。
func TestSettleAndRenewNoopWhenNotBilling(t *testing.T) {
	for _, mode := range []string{"off", "shadow"} {
		s := &Service{conf: Conf{Mode: mode}}
		if err := s.Settle(context.Background(), 1, "req", 1); err != nil {
			t.Fatalf("mode=%s Settle: %v", mode, err)
		}
		if err := s.Renew(context.Background(), 1, "req"); err != nil {
			t.Fatalf("mode=%s Renew: %v", mode, err)
		}
	}
}

// 租约与文案是对外约定：租约过短会在排队时误释放，文案要和用户端一致。
func TestLeaseAndDeliverMessage(t *testing.T) {
	if LeaseDuration < 5*time.Minute {
		t.Fatalf("租约太短: %s", LeaseDuration)
	}
	if DeliverBlockedReason != "点数已失效，请重新提交" {
		t.Fatalf("文案变了: %s", DeliverBlockedReason)
	}
}
