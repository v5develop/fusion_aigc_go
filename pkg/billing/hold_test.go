package billing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"aigc-local-life/pkg/xerr"
)

func TestTierOf(t *testing.T) {
	for in, want := range map[string]string{
		"lite": TierLite, "seedance2.5": TierCloud, "doubao-seedance-2-0-260128": TierCloud, "": TierCloud,
	} {
		if got := TierOf(in); got != want {
			t.Errorf("TierOf(%q) = %s, want %s", in, got, want)
		}
	}
}

// off 和空服务：什么都不做，不碰库（conn 是 nil，碰了就 panic）
func TestHoldOffDoesNothing(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]*Service{"nil": nil, "off": {conf: Conf{Mode: ModeOff}}} {
		r, err := s.Hold(ctx, Charge{UserID: 1, RequestID: "x", EntryCode: "poster"})
		if r != nil || err != nil {
			t.Errorf("%s: Hold = %v, %v", name, r, err)
		}
		s.Done(ctx, 1, "x", 1)
		s.Fail(ctx, 1, "x", "r")
		if err := s.Enough(ctx, Charge{}); err != nil {
			t.Errorf("%s: Enough = %v", name, err)
		}
	}
	// shadow 下 Done / Fail 也不碰库（没冻结过）
	s := &Service{conf: Conf{Mode: ModeShadow}}
	s.Done(ctx, 1, "x", 1)
	s.Fail(ctx, 1, "x", "r")
}

func TestUserError(t *testing.T) {
	s := &Service{conf: Conf{Mode: ModeOn}}
	ctx := context.Background()
	c := Charge{UserID: 1, EntryCode: "poster"}
	cases := []struct {
		err  error
		code int
	}{
		{&NotEnough{Need: 20, Available: 5}, xerr.InsufficientPoints},
		{fmt.Errorf("w: %w", ErrNoFeature), xerr.Forbidden},
		{ErrNoOffer, xerr.ServerError},
		{ErrPricingRule, xerr.ServerError},
		{errors.New("db down"), xerr.ServerError},
	}
	for _, tc := range cases {
		code, msg, ok := xerr.Parse(s.userError(ctx, c, tc.err))
		if !ok || code != tc.code {
			t.Errorf("%v → %d %q, want %d", tc.err, code, msg, tc.code)
		}
	}
	_, msg, _ := xerr.Parse(s.userError(ctx, c, &NotEnough{Need: 20, Available: 5}))
	if msg != "点数不足，还差 15 点" {
		t.Errorf("msg = %q", msg)
	}
}

// 准入 / 冻结失败三类分开：没开通（Forbidden）、入口没配价格（ServerError + 专门文案）、库异常（ServerError 通用文案）。
// 没配价格是我们的配置问题，不能说成「暂未开通该功能」让用户以为要去买。
func TestUserErrorDistinguishesNoOfferFromNoFeature(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		err  error
		code int
		msg  string
	}{
		{fmt.Errorf("%w: ecom_image_gen", ErrNoFeature), xerr.Forbidden, "暂未开通该功能"},
		{fmt.Errorf("%w: canvas_image", ErrNoOffer), xerr.ServerError, "该功能暂未配置价格，请联系客服"},
		{fmt.Errorf("%w: canvas_image 没有启用的档位", ErrNoOffer), xerr.ServerError, "该功能暂未配置价格，请联系客服"},
		{errors.New("dial tcp: connection refused"), xerr.ServerError, "暂时无法计费，请稍后重试"},
		{xerr.New(xerr.Conflict, "原样"), xerr.Conflict, "原样"},
	}
	for _, tc := range cases {
		code, msg, ok := xerr.Parse(UserError(ctx, 7, "canvas_image", tc.err))
		if !ok || code != tc.code || msg != tc.msg {
			t.Errorf("%v → %d %q, want %d %q", tc.err, code, msg, tc.code, tc.msg)
		}
	}
	if UserError(ctx, 7, "x", nil) != nil {
		t.Error("nil 应返回 nil")
	}
}

// 准入：入口码没有启用的 offer → CheckAccess 返回 ErrNoOffer（不是 ErrNoFeature），翻成「没配价格」而非「没开通」。
func TestCheckAccessNoOfferIsNotNoFeature(t *testing.T) {
	conn, mock := mockConn(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `feature_code` FROM `afv_billing_offer`")).WithArgs("canvas_image").
		WillReturnRows(sqlmock.NewRows([]string{"feature_code"}))
	s := New(Conf{Mode: ModeOn, CheckEntitlement: true}, conn)
	err := s.CheckAccess(context.Background(), 7, "canvas_image")
	if !errors.Is(err, ErrNoOffer) || errors.Is(err, ErrNoFeature) {
		t.Fatalf("err=%v", err)
	}
	code, msg, _ := xerr.Parse(UserError(context.Background(), 7, "canvas_image", err))
	if code != xerr.ServerError || msg != MsgNoOffer {
		t.Fatalf("%d %q", code, msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
