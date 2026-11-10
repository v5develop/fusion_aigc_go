package logic

import (
	"context"
	"testing"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/billing"
)

func quoteSvc() *svc.ServiceContext {
	s := &svc.ServiceContext{}
	s.Config.Limits.MaxImages = 4
	s.Config.ExploreShop.DefaultModel, s.Config.ExploreShop.DefaultDuration = "seedance2.5", 15
	s.Config.ShortDrama.DefaultModel, s.Config.ShortDrama.DefaultDuration = "lite", 30
	return s
}

// 报价构造的计费请求必须和提交（insertAndSubmit → imageCharge / GenerateVideo → videoCharge）逐字段一致，
// 只差请求号——这是「报价 = 冻结」的前提。
func TestQuoteChargeMatchesSubmitCharge(t *testing.T) {
	resetGenRows(t)
	setGenRows(nil) // 选项表空：全部走代码默认值
	ctx, s := context.Background(), quoteSvc()

	cases := []struct {
		name  string
		in    *pb.QuoteReq
		want  billing.Charge
		stage string
	}{
		{"海报按张", &pb.QuoteReq{UserId: 9, Kind: model.KindPoster, ImageCount: 3},
			billing.Charge{UserID: 9, EntryCode: "poster", Kind: billing.KindQuick, Count: 3}, stageSubmit},
		{"长图不传张数按 1", &pb.QuoteReq{UserId: 9, Kind: model.KindLongImage},
			billing.Charge{UserID: 9, EntryCode: "longImage", Kind: billing.KindQuick, Count: 1}, stageSubmit},
		{"动作迁移 lite 档、时长交给档位默认", &pb.QuoteReq{UserId: 9, Kind: model.KindActionTransfer, Model: "lite"},
			billing.Charge{UserID: 9, EntryCode: "actionTransfer", Kind: billing.KindVideo, Tier: billing.TierLite, Count: 1}, stageSubmit},
		{"口播默认档 + 默认时长", &pb.QuoteReq{UserId: 9, Kind: model.KindVoiceover},
			billing.Charge{UserID: 9, EntryCode: "voiceover", Kind: billing.KindVideo, Tier: billing.TierCloud, Count: 1, Seconds: 15}, stageSubmit},
		{"口播 lite 30 秒", &pb.QuoteReq{UserId: 9, Kind: model.KindVoiceover, Model: "lite", Duration: 30},
			billing.Charge{UserID: 9, EntryCode: "voiceover", Kind: billing.KindVideo, Tier: billing.TierLite, Count: 1, Seconds: 30}, stageSubmit},
		{"探店默认", &pb.QuoteReq{UserId: 9, Kind: model.KindExploreShop},
			billing.Charge{UserID: 9, EntryCode: "exploreShop", Kind: billing.KindVideo, Tier: billing.TierCloud, Count: 1, Seconds: 15}, stageGenerate},
		{"短剧走自己的默认档与时长", &pb.QuoteReq{UserId: 9, Kind: model.KindShortDrama},
			billing.Charge{UserID: 9, EntryCode: "shortDrama", Kind: billing.KindVideo, Tier: billing.TierLite, Count: 1, Seconds: 30}, stageGenerate},
		{"复刻未解析：按最短计费时长 4 秒报最低价", &pb.QuoteReq{UserId: 9, Kind: model.KindVideoRemake, Model: "lite"},
			billing.Charge{UserID: 9, EntryCode: "videoRemake", Kind: billing.KindVideo, Tier: billing.TierLite, Count: 1, Seconds: minRemakeSeconds}, stageGenerate},
	}
	for _, c := range cases {
		q, err := quoteChargeOf(ctx, s, c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if q.charge != c.want || q.stage != c.stage {
			t.Errorf("%s: got %+v / %s, want %+v / %s", c.name, q.charge, q.stage, c.want, c.stage)
		}
	}

	// 与提交路径用同一个构造函数：口播记录直接喂 imageCharge 得到的请求（去掉请求号）应相同
	row := &model.LocalLifeCreation{UserId: 9, Kind: model.KindVoiceover, Model: "lite", Duration: 30, ImageCount: 1}
	if q, _ := quoteChargeOf(ctx, s, &pb.QuoteReq{UserId: 9, Kind: model.KindVoiceover, Model: "lite", Duration: 30}); q.charge != imageCharge(row) {
		t.Fatalf("报价与 imageCharge 不一致: %+v vs %+v", q.charge, imageCharge(row))
	}
}

func TestQuoteChargeRejectsWhatSubmitRejects(t *testing.T) {
	resetGenRows(t)
	setGenRows(nil)
	ctx, s := context.Background(), quoteSvc()
	bad := []*pb.QuoteReq{
		{UserId: 1, Kind: model.KindPoster, ImageCount: 5},      // 超过 MaxImages
		{UserId: 1, Kind: model.KindActionTransfer},             // 没选模型
		{UserId: 1, Kind: model.KindVoiceover, Duration: 20},    // 不在时长选项里
		{UserId: 1, Kind: model.KindExploreShop, Model: "nope"}, // 不在模型选项里
		{UserId: 1, Kind: model.KindVideoRemake, Model: "nope"}, // 复刻档位不在选项里
		{UserId: 1, Kind: "unknown"},
	}
	for _, in := range bad {
		if _, err := quoteChargeOf(ctx, s, in); err == nil {
			t.Errorf("应拒绝: %+v", in)
		}
	}
}

// 参数不合法：接口不报错，回 available=false + 给用户看的原因
func TestQuoteInvalidParamsBecomeReason(t *testing.T) {
	resetGenRows(t)
	setGenRows(nil)
	out, err := NewQuoteLogic(context.Background(), quoteSvc()).Quote(&pb.QuoteReq{UserId: 1, Kind: model.KindVoiceover, Duration: 20})
	if err != nil {
		t.Fatal(err)
	}
	if out.Available || out.Points != 0 || out.Reason != "视频时长无效" || out.Stage != stageSubmit {
		t.Fatalf("got %+v", out)
	}
}

// 没配计费（Billing=nil，等同 off）：可用、0 点、不扣
func TestQuoteBillingOff(t *testing.T) {
	resetGenRows(t)
	setGenRows(nil)
	out, err := NewQuoteLogic(context.Background(), quoteSvc()).Quote(&pb.QuoteReq{UserId: 1, Kind: model.KindPoster, ImageCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Available || out.Charged || out.Points != 0 || out.BillingMode != billing.ModeOff || !out.Exact || !out.Sufficient {
		t.Fatalf("got %+v", out)
	}
}
