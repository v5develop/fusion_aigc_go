package main

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
)

type fakeBiller struct {
	resv     map[string]*billing.ReservationBrief // key: requestID
	mode     map[string]string                    // key: reservation no
	consumed map[string]int64
	before   map[string]int64
	refunds  map[string]int64 // RefundOvercharge 调用：no → target
}

func (f *fakeBiller) LoadReservation(_ context.Context, _ int64, req string) (*billing.ReservationBrief, error) {
	if req == "boom" {
		return nil, errors.New("db down")
	}
	return f.resv[req], nil
}
func (f *fakeBiller) CarrierOf(_ context.Context, b *billing.ReservationBrief) (string, func(*big.Rat) int64) {
	rate, markup := big.NewRat(72, 10), big.NewRat(13, 10)
	return f.mode[b.No], func(usd *big.Rat) int64 { return billing.ActualPoints(b.PricePoints, usd, rate, markup) }
}
func (f *fakeBiller) ConsumedPoints(_ context.Context, b *billing.ReservationBrief) (int64, int64, error) {
	return f.consumed[b.No], f.before[b.No], nil
}
func (f *fakeBiller) RefundOvercharge(_ context.Context, no string, target int64, _ string) (int64, error) {
	f.refunds[no] = target
	return f.consumed[no] - target, nil
}

type fakeGateway struct{ calls map[string]aigateway.Call }

func (g *fakeGateway) BatchGet(_ context.Context, ids []string) ([]aigateway.Call, error) {
	var out []aigateway.Call
	for _, id := range ids {
		if c, ok := g.calls[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func rep(usd string) *aigateway.Cost {
	return &aigateway.Cost{Amount: usd, Currency: "USD", Source: aigateway.CostSourceReported, Final: true}
}

func fixtures() (*fakeBiller, *fakeGateway, []candidate) {
	brief := func(no, status string, price int64) *billing.ReservationBrief {
		return &billing.ReservationBrief{No: no, UserID: 7, Status: status, PricePoints: price, GoCreated: true}
	}
	b := &fakeBiller{
		resv: map[string]*billing.ReservationBrief{
			"r-refund": brief("BR1", "CONFIRMED", 836),
			"r-multi":  brief("BR2", "PARTIALLY_REFUNDED", 1672),
			"r-fixed":  brief("BR3", "CONFIRMED", 20),
			"r-nocost": brief("BR4", "CONFIRMED", 836),
			"r-done":   brief("BR5", "CONFIRMED", 836),
			"r-fair":   brief("BR6", "CONFIRMED", 836),
			"r-held":   brief("BR7", "HELD", 836),
		},
		mode:     map[string]string{"BR1": billing.SettleActualCost, "BR2": billing.SettleActualCost, "BR3": billing.SettleFixed, "BR4": billing.SettleActualCost, "BR5": billing.SettleActualCost, "BR6": billing.SettleActualCost},
		consumed: map[string]int64{"BR1": 836, "BR2": 1672, "BR4": 836, "BR5": 836, "BR6": 14},
		before:   map[string]int64{"BR5": 822},
		refunds:  map[string]int64{},
	}
	g := &fakeGateway{calls: map[string]aigateway.Call{
		"c1": {ID: "c1", Status: aigateway.StatusSucceeded, Cost: rep("0.014536")},
		"c2": {ID: "c2", Status: aigateway.StatusSucceeded, Cost: rep("0.014536")},
		"c3": {ID: "c3", Status: aigateway.StatusSucceeded, Cost: rep("0.02")},
		"c4": {ID: "c4", Status: aigateway.StatusSucceeded, Cost: &aigateway.Cost{Amount: "0.01", Currency: "USD", Source: aigateway.CostSourceConfigured, Final: true}},
		"c6": {ID: "c6", Status: aigateway.StatusSucceeded, Cost: rep("0.014536")},
	}}
	cands := []candidate{
		{Source: "canvas", RefID: 1, UserID: 7, RequestID: "r-refund", CallIDs: []string{"c1"}},
		{Source: "canvas", RefID: 2, UserID: 7, RequestID: "r-multi", CallIDs: []string{"c2", "c3"}}, // 一个 run 两个调用：成本求和
		{Source: "studio", RefID: 3, UserID: 7, RequestID: "r-fixed", CallIDs: []string{"c1"}},
		{Source: "studio", RefID: 4, UserID: 7, RequestID: "r-nocost", CallIDs: []string{"c4"}},
		{Source: "studio", RefID: 5, UserID: 7, RequestID: "r-done", CallIDs: []string{"c1"}},
		{Source: "studio", RefID: 6, UserID: 7, RequestID: "r-fair", CallIDs: []string{"c6"}},
		{Source: "studio", RefID: 7, UserID: 7, RequestID: "r-held", CallIDs: []string{"c1"}},
		{Source: "studio", RefID: 8, UserID: 7, RequestID: "r-missing", CallIDs: []string{"c1"}},
		{Source: "canvas", RefID: 9, UserID: 7, RequestID: "boom", CallIDs: []string{"c1"}},
	}
	return b, g, cands
}

// dry-run：只算不写，汇总各类结论与将退点数。
func TestRunDryRun(t *testing.T) {
	b, g, cands := fixtures()
	var out bytes.Buffer
	s := run(context.Background(), b, g, cands, false, &out)
	printSummary(&out, s, false)
	if len(b.refunds) != 0 {
		t.Fatalf("dry-run 不能退: %v", b.refunds)
	}
	want := map[string]int{outRefund: 2, outFixed: 1, outNoCost: 1, outAlready: 1, outFair: 1, outNotConfirmed: 1, outNoReservation: 1, outError: 1}
	for k, v := range want {
		if s.ByOutcome[k] != v {
			t.Errorf("%s=%d want %d\n%s", k, s.ByOutcome[k], v, out.String())
		}
	}
	// BR1：836 - 14 = 822；BR2：两个调用 $0.034536 → ceil(32.33) = 33，1672 - 33 = 1639
	if s.RefundPoints != 822+1639 {
		t.Fatalf("将退合计 %d\n%s", s.RefundPoints, out.String())
	}
	for _, sub := range []string{"DRY-RUN", "将退=822", "将退=1639", "先在 ai-gateway 跑历史调用成本回填脚本"} {
		if !strings.Contains(out.String(), sub) {
			t.Errorf("输出缺 %q\n%s", sub, out.String())
		}
	}
}

// apply：只对该退的调 RefundOvercharge，目标 = 公平点数。
func TestRunApply(t *testing.T) {
	b, g, cands := fixtures()
	var out bytes.Buffer
	s := run(context.Background(), b, g, cands, true, &out)
	if len(b.refunds) != 2 || b.refunds["BR1"] != 14 || b.refunds["BR2"] != 33 {
		t.Fatalf("补退调用: %v", b.refunds)
	}
	if s.RefundPoints != 822+1639 {
		t.Fatalf("实退合计 %d", s.RefundPoints)
	}
}

// 只碰 Go 冻结的单；多条记录共用一张预留单时合成一笔、调用号取并集（成本覆盖全部调用，不重复计退）。
func TestRunScopeAndMerge(t *testing.T) {
	b, g, _ := fixtures()
	b.resv["r-java"] = &billing.ReservationBrief{No: "BRJ", UserID: 7, Status: "CONFIRMED", PricePoints: 836}
	b.mode["BRJ"] = billing.SettleActualCost
	b.consumed["BRJ"] = 836
	cands := []candidate{
		{Source: "studio", RefID: 1, UserID: 7, RequestID: "r-java", CallIDs: []string{"c1"}},
		{Source: "canvas", RefID: 2, UserID: 7, RequestID: "r-multi", CallIDs: []string{"c2"}},
		{Source: "canvas", RefID: 3, UserID: 7, RequestID: "r-multi", CallIDs: []string{"c3", "c2"}},
	}
	var out bytes.Buffer
	s := run(context.Background(), b, g, cands, true, &out)
	if s.Scanned != 2 || s.ByOutcome[outNotGo] != 1 || s.ByOutcome[outRefund] != 1 {
		t.Fatalf("结论: %+v\n%s", s.ByOutcome, out.String())
	}
	if _, ok := b.refunds["BRJ"]; ok {
		t.Fatal("Java 的单不能补退")
	}
	// 合并后 c2 + c3 = $0.034536 → 33 点（只用第一条记录的 c2 会算成 14、多退）
	if b.refunds["BR2"] != 33 {
		t.Fatalf("合并调用后的目标点数: %v", b.refunds)
	}
}
