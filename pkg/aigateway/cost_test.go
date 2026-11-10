package aigateway

import "testing"

func reported(amount string) *Cost {
	return &Cost{Amount: amount, Currency: "USD", AmountCNY: "0.1", Source: CostSourceReported, Final: true}
}

func TestReportedUSD(t *testing.T) {
	ok1 := &Call{ID: "a", Status: StatusSucceeded, Cost: reported("0.014536")}
	ok2 := &Call{ID: "b", Status: StatusSucceeded, Cost: reported("0.02")}
	if v, ok := ReportedUSD([]*Call{ok1, ok2}); !ok || v.FloatString(6) != "0.034536" {
		t.Fatalf("两个调用求和: %v %v", v, ok)
	}
	// 只认 source=reported && final
	for _, c := range []*Cost{
		{Amount: "0.01", Currency: "USD", Source: CostSourceConfigured, Final: true},
		{Amount: "0.01", Currency: "USD", Source: CostSourceReported, Final: false},
		{Amount: "0.01", Currency: "CNY", Source: CostSourceReported, Final: true},
		{Amount: "x", Currency: "USD", Source: CostSourceReported, Final: true},
	} {
		if _, ok := ReportedUSD([]*Call{{Status: StatusSucceeded, Cost: c}}); ok {
			t.Errorf("不应可用: %+v", c)
		}
	}
	// 调用级没有实报值：按扣费尝试求和（多次尝试）；NOT_CHARGED 不计
	multi := &Call{Status: StatusSucceeded, Attempts: []Attempt{
		{Seq: 1, Charged: ChargedNo},
		{Seq: 2, Charged: ChargedYes, Cost: reported("0.01")},
		{Seq: 3, Charged: ChargedYes, Cost: reported("0.005")},
	}}
	if v, ok := ReportedUSD([]*Call{multi}); !ok || v.FloatString(3) != "0.015" {
		t.Fatalf("尝试求和: %v %v", v, ok)
	}
	// 有一个扣费尝试没实报 → 整组不可用
	multi.Attempts[2].Cost = &Cost{Amount: "0.005", Currency: "USD", Source: CostSourceEstimated}
	if _, ok := ReportedUSD([]*Call{multi}); ok {
		t.Fatal("缺实报的尝试")
	}
	// 失败且一次都没扣费 → 0；在途 / nil / 空 → 不可用
	if v, ok := ReportedUSD([]*Call{ok1, {Status: StatusFailed, Attempts: []Attempt{{Charged: ChargedNo}}}}); !ok || v.FloatString(6) != "0.014536" {
		t.Fatalf("失败未扣费记 0: %v %v", v, ok)
	}
	for _, cs := range [][]*Call{nil, {nil}, {{Status: StatusRunning}}, {{Status: StatusSucceeded}}} {
		if _, ok := ReportedUSD(cs); ok {
			t.Errorf("不应可用: %+v", cs)
		}
	}
	var nilCost *Cost
	if nilCost.Authoritative() {
		t.Fatal("nil")
	}
}
