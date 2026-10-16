package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"aigc-life/pkg/xerr"
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
