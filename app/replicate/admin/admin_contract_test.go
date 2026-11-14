package admin

import (
	"os"
	"strings"
	"testing"

	"aigc-local-life/pkg/billing"
)

// 契约：流水归属按前缀精确匹配，依赖 pkg/billing 写流水的键前缀与 RefundConfirmed 的描述前缀（见 pkg/billing/ledger_keys.go）。
func TestLedgerAttributionContract(t *testing.T) {
	if billing.RefundKeyPrefix != "refunded:" || billing.PostRefundKeyPrefix != "post-refund:" {
		t.Fatal("billing 键前缀变了")
	}
	src, err := os.ReadFile("../../../pkg/billing/refund_confirmed.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `Description: "`+confirmedDescPrefix+`" + reason`) {
		t.Fatalf("RefundConfirmed 的流水描述前缀不再是 %q，confirmedDescPrefix 要同步", confirmedDescPrefix)
	}
	if !strings.HasPrefix(heldReasonPrefix(12), "后台人工退点[a12]：") || strings.HasPrefix(heldReasonPrefix(12), heldReasonPrefix(1)) {
		t.Fatal("动作前缀必须能区分 a1 与 a12")
	}
}
