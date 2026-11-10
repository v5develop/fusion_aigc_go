package quote

import (
	"context"
	"testing"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/pkg/ctxdata"

	"google.golang.org/grpc"
)

type fakeImage struct {
	imagegen.ImageGen
	got *imagegen.QuoteReq
}

func (f *fakeImage) Quote(_ context.Context, in *imagegen.QuoteReq, _ ...grpc.CallOption) (*imagegen.QuoteResp, error) {
	f.got = in
	return &imagegen.QuoteResp{Available: true, BillingMode: "on", Charged: true, Points: 120, UnitPoints: 8,
		PriceUnit: "PER_SECOND", Quantity: 15, Exact: true, Stage: "submit", Balance: 100, BalanceKnown: true}, nil
}

type fakeNote struct {
	note.Note
	got *note.QuoteReq
}

func (f *fakeNote) Quote(_ context.Context, in *note.QuoteReq, _ ...grpc.CallOption) (*note.QuoteResp, error) {
	f.got = in
	return &note.QuoteResp{Available: true, Points: 40, Stage: "generate", Exact: true, Sufficient: true}, nil
}

type fakeHuman struct {
	digitalhuman.DigitalHuman
	got *digitalhuman.QuoteReq
}

func (f *fakeHuman) Quote(_ context.Context, in *digitalhuman.QuoteReq, _ ...grpc.CallOption) (*digitalhuman.QuoteResp, error) {
	f.got = in
	return &digitalhuman.QuoteResp{Available: true, Points: 32, PointsMax: 240, Exact: false, Stage: "submit", Sufficient: true, SufficientMax: false}, nil
}

type fakeCopy struct {
	copywriting.Copywriting
	got *copywriting.QuoteReq
}

func (f *fakeCopy) Quote(_ context.Context, in *copywriting.QuoteReq, _ ...grpc.CallOption) (*copywriting.QuoteResp, error) {
	f.got = in
	return &copywriting.QuoteResp{Available: true, Points: 2, Exact: true, Stage: "submit"}, nil
}

func TestQuoteDispatchesToBillingService(t *testing.T) {
	img, nt, dh, cw := &fakeImage{}, &fakeNote{}, &fakeHuman{}, &fakeCopy{}
	s := &svc.ServiceContext{ImageGenRpc: img, NoteRpc: nt, DigitalHumanRpc: dh, CopywritingRpc: cw}
	ctx := ctxdata.WithUserID(context.Background(), 42)
	l := NewQuoteLogic(ctx, s)

	out, err := l.Quote(&types.LocalLifeQuoteReq{Tool: "voiceover", Model: "lite", Duration: 15})
	if err != nil {
		t.Fatal(err)
	}
	if img.got.UserId != 42 || img.got.Kind != "voiceover" || img.got.Model != "lite" || img.got.Duration != 15 {
		t.Fatalf("转发参数不对: %+v", img.got)
	}
	if !out.Available || !out.Charged || out.Points != 120 || out.UnitPoints != 8 || out.Quantity != 15 ||
		!out.BalanceKnown || out.Balance != 100 || out.Sufficient || out.FreeQuota != 0 || out.Stage != "submit" {
		t.Fatalf("字段映射不对: %+v", out)
	}

	if _, err := l.Quote(&types.LocalLifeQuoteReq{Tool: "videoRemake", Id: 7, Model: "lite"}); err != nil || img.got.Id != 7 || img.got.Kind != "videoRemake" {
		t.Fatalf("复刻带记录 id: %+v %v", img.got, err)
	}
	if out, _ := l.Quote(&types.LocalLifeQuoteReq{Tool: "imageTextNote", ImageCount: 4}); nt.got.ImageCount != 4 || nt.got.UserId != 42 || out.Points != 40 || out.Stage != "generate" {
		t.Fatalf("图文笔记: %+v %+v", nt.got, out)
	}
	if out, _ := l.Quote(&types.LocalLifeQuoteReq{Tool: "imageTextNote", Id: 9}); nt.got.Id != 9 || !out.SufficientMax {
		t.Fatalf("图文笔记配图阶段带 id: %+v %+v", nt.got, out)
	}
	if out, _ := l.Quote(&types.LocalLifeQuoteReq{Tool: "digitalHuman", Model: "lite"}); dh.got.Model != "lite" || out.Exact || out.PointsMax != 240 ||
		!out.Sufficient || out.SufficientMax {
		t.Fatalf("数字人: %+v %+v", dh.got, out)
	}
	if out, _ := l.Quote(&types.LocalLifeQuoteReq{Tool: "copywriting"}); cw.got.UserId != 42 || out.Points != 2 {
		t.Fatalf("文案: %+v %+v", cw.got, out)
	}
	if _, err := l.Quote(&types.LocalLifeQuoteReq{Tool: "studio"}); err == nil {
		t.Fatal("未知工具应报参数错误")
	}
}
