package billing

import (
	"os"
	"strings"
	"testing"
)

// 契约：导出的键前缀与实际写流水的字面量一致（只读对账方据此按前缀精确匹配，改这里必须同步改写入处）。
func TestLedgerKeyPrefixesMatchWriters(t *testing.T) {
	if RefundKeyPrefix != "refunded:" || PostRefundKeyPrefix != "post-refund:" {
		t.Fatalf("前缀变了：%q %q", RefundKeyPrefix, PostRefundKeyPrefix)
	}
	ledger, err := os.ReadFile("ledger.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ledger), `"refunded:"+no`) {
		t.Fatal("ledger.go 的 refund 不再用 \"refunded:\"+单号 写流水，RefundKeyPrefix 要同步")
	}
	rc, err := os.ReadFile("refund_confirmed.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rc), `IdempotencyKey: "post-refund:" + no`) || !strings.Contains(string(rc), `Description: "人工处理退回：" + reason`) {
		t.Fatal("refund_confirmed.go 的流水键 / 描述变了，PostRefundKeyPrefix 与对账方的描述前缀要同步")
	}
}
