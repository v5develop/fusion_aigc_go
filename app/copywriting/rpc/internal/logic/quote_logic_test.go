package logic

import (
	"context"
	"testing"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/billing"
)

func TestCopywritingQuote(t *testing.T) {
	s := &svc.ServiceContext{}
	s.Config.BillingEntryCode = "aiCopywriting"
	out, err := NewQuoteLogic(context.Background(), s).Quote(&pb.QuoteReq{UserId: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Available || out.Charged || out.Points != 0 || out.BillingMode != billing.ModeOff || out.Stage != "submit" {
		t.Fatalf("未配计费 = off: %+v", out)
	}
	if c := copywritingCharge("aiCopywriting", 3, ""); c.EntryCode != "aiCopywriting" || c.Count != 1 || c.Kind != billing.KindQuick {
		t.Fatalf("charge %+v", c)
	}
}
