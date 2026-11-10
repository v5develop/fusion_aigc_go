package logic

import (
	"context"
	"testing"

	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/billing"
)

// 最低价的计费请求与提交前余额快拦（Enough）同一个：最短时长、按档次
func TestMinHumanCharge(t *testing.T) {
	c := minHumanCharge(5, modelLite)
	want := billing.Charge{UserID: 5, EntryCode: "digitalHuman", Kind: billing.KindVideo, Tier: billing.TierLite, Count: 1, Seconds: minVideoSeconds}
	if c != want {
		t.Fatalf("got %+v want %+v", c, want)
	}
	if minHumanCharge(5, modelCloud).Tier != billing.TierCloud {
		t.Fatal("云端档")
	}
}

func TestDigitalHumanQuote(t *testing.T) {
	l := NewQuoteLogic(context.Background(), &svc.ServiceContext{})
	out, err := l.Quote(&pb.QuoteReq{UserId: 1, Model: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Available || out.Reason != "出片模型不可用，请重新选择" {
		t.Fatalf("模型不合法: %+v", out)
	}
	out, _ = l.Quote(&pb.QuoteReq{UserId: 1})
	if !out.Available || out.Charged || out.Points != 0 || !out.Exact || out.BillingMode != billing.ModeOff {
		t.Fatalf("未配计费 = off：0 点且是准数: %+v", out)
	}
}

func TestSufficientMax(t *testing.T) {
	cases := []struct {
		r    *pb.QuoteResp
		want bool
	}{
		{&pb.QuoteResp{Charged: true, BalanceKnown: true, Balance: 100, Points: 30, PointsMax: 240, Sufficient: true}, false},
		{&pb.QuoteResp{Charged: true, BalanceKnown: true, Balance: 300, Points: 30, PointsMax: 240, Sufficient: true}, true},
		{&pb.QuoteResp{Charged: true, BalanceKnown: false, PointsMax: 240, Sufficient: true}, true},             // 余额未知
		{&pb.QuoteResp{Charged: false, BalanceKnown: true, Balance: 1, PointsMax: 240, Sufficient: true}, true}, // 不扣
		{&pb.QuoteResp{Exact: true, Charged: true, BalanceKnown: true, Balance: 1, Points: 5, Sufficient: false}, false},
	}
	for i, c := range cases {
		if got := sufficientMax(c.r); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}
