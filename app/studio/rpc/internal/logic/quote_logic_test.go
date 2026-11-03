package logic

import (
	"context"
	"errors"
	"testing"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/billing"
)

// fakeQuoter 按分辨率出单价，记下收到的计费请求。
type fakeQuoter struct {
	mode   string
	prices map[string]int64 // 1K / 2K / 4K → 点
	err    error
	got    []billing.Charge
}

func (f *fakeQuoter) Mode() string { return f.mode }

func (f *fakeQuoter) Quote(_ context.Context, c billing.Charge) (*billing.Quote, error) {
	f.got = append(f.got, c)
	if f.err != nil {
		return nil, f.err
	}
	return &billing.Quote{PricePoints: f.prices[c.Resolution], PriceUnit: "PER_IMAGE"}, nil
}

var quoteGroups = map[string]string{"通用": "img.gpt-image-2", "Seedream": "img.seedream-5"}

func TestQuoteStudio_UnitTimesTasks(t *testing.T) {
	q := &fakeQuoter{mode: billing.ModeOn, prices: map[string]int64{"1K": 10, "2K": 15, "4K": 20}}
	r := quoteStudio(context.Background(), q, "studio_tryon", model.KindTryon,
		&studio.QuoteReq{UserId: 7, Resolution: "2K", TaskTotal: 3}, quoteGroups)
	if !r.Available || r.UnitPoints != 15 || r.Points != 45 || !r.Charged || r.Resolution != "2k" {
		t.Fatalf("报价不对：%+v", r)
	}
	c := q.got[0]
	if c.EntryCode != "studio_tryon" || c.Count != 1 || c.Resolution != "2K" || c.UserID != 7 || c.RequestID != "" {
		t.Fatalf("计费请求须与提交 charge 同形：%+v", c)
	}
}

func TestQuoteStudio_SeedreamNormalizedLikeSubmit(t *testing.T) {
	q := &fakeQuoter{mode: billing.ModeOn, prices: map[string]int64{"1K": 10, "2K": 15, "4K": 20}}
	r := quoteStudio(context.Background(), q, "studio_imagegen", model.KindImageGen,
		&studio.QuoteReq{Model: "Seedream", Resolution: "4k", TaskTotal: 2}, quoteGroups)
	if r.Resolution != "2k" || r.Points != 30 || len(r.Warnings) != 1 {
		t.Fatalf("Seedream 4K 应按 2K 计价并带告警：%+v", r)
	}
}

func TestQuoteStudio_OffAndNoOffer(t *testing.T) {
	off := quoteStudio(context.Background(), &fakeQuoter{mode: billing.ModeOff}, "studio_pose", model.KindPose,
		&studio.QuoteReq{TaskTotal: 2}, quoteGroups)
	if !off.Available || off.Points != 0 || off.Charged || off.BillingMode != billing.ModeOff {
		t.Fatalf("不计费应回 0 点：%+v", off)
	}
	no := quoteStudio(context.Background(), &fakeQuoter{mode: billing.ModeOn, err: billing.ErrNoOffer}, "studio_pose",
		model.KindPose, &studio.QuoteReq{TaskTotal: 2}, quoteGroups)
	if no.Available || no.Points != 0 || no.Reason == "" {
		t.Fatalf("没配价应 available=false：%+v", no)
	}
	bad := quoteStudio(context.Background(), &fakeQuoter{mode: billing.ModeOn, err: errors.New("x")}, "studio_pose",
		model.KindPose, &studio.QuoteReq{Model: "不存在"}, quoteGroups)
	if bad.Available || bad.Reason == "" {
		t.Fatalf("不支持的模型应 available=false：%+v", bad)
	}
}
