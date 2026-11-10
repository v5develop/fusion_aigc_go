package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type fakeEstimator struct {
	mode    string
	quote   *Quote
	err     error
	balance int64
	balErr  error
	got     Charge
	// noFeature 模拟没开通权益；entErr 模拟查权益失败
	noFeature bool
	entErr    error
	entCalls  int
}

func (f *fakeEstimator) Entitled(context.Context, int64, string) (bool, error) {
	f.entCalls++
	return !f.noFeature, f.entErr
}

func (f *fakeEstimator) Mode() string { return f.mode }
func (f *fakeEstimator) Quote(_ context.Context, c Charge) (*Quote, error) {
	f.got = c
	return f.quote, f.err
}
func (f *fakeEstimator) Balance(context.Context, int64) (*Account, error) {
	if f.balErr != nil {
		return nil, f.balErr
	}
	return &Account{Balance: f.balance}, nil
}

func videoQuote(points, unit int64, secs int) *Quote {
	return &Quote{PricePoints: points, PriceUnit: unitPerSecond, Quantity: secs, Lines: []QuoteLine{
		{ComponentCode: "parse", CostOnly: true},
		{ComponentCode: "gen", UnitPoints: unit, Unit: unitPerSecond, Quantity: secs, Points: points},
	}}
}

func TestEstimateOnPassesChargeThroughAndChecksBalance(t *testing.T) {
	f := &fakeEstimator{mode: ModeOn, quote: videoQuote(120, 8, 15), balance: 100}
	c := Charge{UserID: 7, EntryCode: "voiceover", Kind: KindVideo, Tier: TierCloud, Count: 1, Seconds: 15}
	e := estimate(context.Background(), f, c)
	if f.got != c {
		t.Fatalf("报价必须用调用方给的同一个 Charge: %+v", f.got)
	}
	if !e.Available || !e.Charged || e.Points != 120 || e.UnitPoints != 8 || e.Quantity != 15 || e.PriceUnit != unitPerSecond {
		t.Fatalf("报价字段不对: %+v", e)
	}
	if !e.BalanceKnown || e.Balance != 100 || e.Sufficient {
		t.Fatalf("余额 100 < 120 应不足: %+v", e)
	}
	f.balance = 120
	if e := estimate(context.Background(), f, c); !e.Sufficient {
		t.Fatalf("余额正好够（与冻结同口径 balance >= price）应足够: %+v", e)
	}
}

func TestEstimateOffDoesNotPrice(t *testing.T) {
	f := &fakeEstimator{mode: ModeOff, err: errors.New("不该被调用")}
	e := estimate(context.Background(), f, Charge{UserID: 1, EntryCode: "poster"})
	if !e.Available || e.Charged || e.Points != 0 || !e.Sufficient {
		t.Fatalf("off：提交冻结 0 点: %+v", e)
	}
	if f.got.EntryCode != "" {
		t.Fatal("off 不应算价")
	}
}

func TestEstimateShadowPricesButNotCharged(t *testing.T) {
	f := &fakeEstimator{mode: ModeShadow, quote: &Quote{PricePoints: 30, PriceUnit: unitPerImage, Quantity: 3,
		Lines: []QuoteLine{{UnitPoints: 10, Points: 30}}}, balance: 0}
	e := estimate(context.Background(), f, Charge{UserID: 1, EntryCode: "poster", Count: 3})
	if !e.Available || e.Charged || e.Points != 30 || e.UnitPoints != 10 || !e.Sufficient {
		t.Fatalf("shadow：照常算价但不扣、不判余额: %+v", e)
	}
}

func TestEstimateErrorsBecomeReasons(t *testing.T) {
	f := &fakeEstimator{mode: ModeOn, err: fmt.Errorf("%w: poster", ErrNoOffer)}
	e := estimate(context.Background(), f, Charge{UserID: 1, EntryCode: "poster"})
	if e.Available || e.Points != 0 || e.Reason != "该功能暂未配置价格，请联系客服" {
		t.Fatalf("没配价格: %+v", e)
	}
	f.err = ErrPricingRule
	if e := estimate(context.Background(), f, Charge{UserID: 1, EntryCode: "poster"}); e.Available || e.Reason != "暂时无法报价，请稍后重试" {
		t.Fatalf("其他错误: %+v", e)
	}
	// shadow：Hold 对未配价格只记日志、不拦、冻结 0 点 → 报价也不当拦截原因
	f = &fakeEstimator{mode: ModeShadow, err: fmt.Errorf("%w: poster", ErrNoOffer)}
	if e := estimate(context.Background(), f, Charge{UserID: 1, EntryCode: "poster"}); !e.Available || e.Reason != "" || e.Charged || e.Points != 0 || !e.Sufficient {
		t.Fatalf("shadow 未配价格不该拦: %+v", e)
	}
}

func TestEstimateBalanceFailureKeepsPrice(t *testing.T) {
	f := &fakeEstimator{mode: ModeOn, quote: videoQuote(60, 4, 15), balErr: errors.New("db down")}
	e := estimate(context.Background(), f, Charge{UserID: 1, EntryCode: "digitalHuman"})
	if !e.Available || e.Points != 60 || e.BalanceKnown || !e.Sufficient {
		t.Fatalf("余额查不到不影响报价，也不判不足: %+v", e)
	}
}

func TestEstimateNilService(t *testing.T) {
	var s *Service
	if e := s.Estimate(context.Background(), Charge{}); !e.Available || e.Mode != ModeOff {
		t.Fatalf("nil 服务按 off: %+v", e)
	}
}

func TestEstimateEntitlement(t *testing.T) {
	f := &fakeEstimator{mode: ModeOn, quote: videoQuote(60, 4, 15), balance: 100, noFeature: true}
	e := estimate(context.Background(), f, Charge{UserID: 7, EntryCode: "voiceover"})
	if e.Available || e.Reason != "未开通该功能" || e.Points != 0 {
		t.Fatalf("没开通应不可报: %+v", e)
	}
	f = &fakeEstimator{mode: ModeOn, quote: videoQuote(60, 4, 15), entErr: errors.New("db down")}
	if e = estimate(context.Background(), f, Charge{UserID: 7}); e.Available || e.Reason != "暂时无法报价，请稍后重试" {
		t.Fatalf("查权益失败: %+v", e)
	}
	// shadow：提交不会因没开通被拦（Hold 吞错），报价不查权益、照常报价
	f = &fakeEstimator{mode: ModeShadow, quote: videoQuote(60, 4, 15), noFeature: true}
	if e = estimate(context.Background(), f, Charge{UserID: 7}); !e.Available || e.Reason != "" || e.Charged || e.Points != 60 || f.entCalls != 0 {
		t.Fatalf("shadow 不该报未开通: %+v calls=%d", e, f.entCalls)
	}
	// off 与 Freeze 一样不查权益
	f = &fakeEstimator{mode: ModeOff, noFeature: true}
	if e = estimate(context.Background(), f, Charge{UserID: 7}); !e.Available || f.entCalls != 0 {
		t.Fatalf("off 不查权益: %+v calls=%d", e, f.entCalls)
	}
}

func TestServiceEntitledRespectsSwitch(t *testing.T) {
	s := &Service{conf: Conf{CheckEntitlement: false}}
	if ok, err := s.Entitled(context.Background(), 1, "x"); !ok || err != nil {
		t.Fatalf("开关关闭恒放行: %v %v", ok, err)
	}
}
