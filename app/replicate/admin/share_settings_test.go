package admin

import (
	"context"
	"testing"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"
)

type staticRewardSrc struct{ v RewardSettings }

func (s *staticRewardSrc) Load(context.Context) RewardSettings { return s.v }

func TestRewardSettingsValidate(t *testing.T) {
	ok := []RewardSettings{{true, 1, 0}, {false, 1000, 100}, {true, 30, 3}}
	for _, v := range ok {
		if err := v.Validate(); err != nil {
			t.Errorf("%+v 应合法：%v", v, err)
		}
	}
	bad := []RewardSettings{{true, 0, 3}, {true, 1001, 3}, {true, -5, 3}, {true, 30, -1}, {true, 30, 101}}
	for _, v := range bad {
		if err := v.Validate(); err == nil {
			t.Errorf("%+v 应非法", v)
		}
	}
}

// 没装配来源 = 内置默认 true / 30 / 3，行里带 rewardPoints
func TestShareRowDefaultsWithoutSource(t *testing.T) {
	s, f, _ := shareFixture(t)
	f.shares[1].Status = model.ShareApproved
	l, _ := s.ListShares(ctx, ShareListReq{})
	if r := l.Items[0]; r.RewardPoints != 30 || r.RewardLimit != 3 || !r.RewardAllowed {
		t.Fatalf("默认值：%+v", r)
	}
}

func TestShareRowFollowsSettings(t *testing.T) {
	s, f, _ := shareFixture(t)
	f.shares[1].Status = model.ShareApproved
	src := &staticRewardSrc{RewardSettings{Enabled: true, Points: 50, MonthlyLimit: 5}}
	s.SetRewardSettings(src)
	l, _ := s.ListShares(ctx, ShareListReq{})
	if r := l.Items[0]; r.RewardPoints != 50 || r.RewardLimit != 5 || !r.RewardAllowed {
		t.Fatalf("应取配置值：%+v", r)
	}
	src.v.Enabled = false // 关闭：rewardAllowed 置 false，点数 / 上限照常回显
	l, _ = s.ListShares(ctx, ShareListReq{})
	if r := l.Items[0]; r.RewardAllowed || r.RewardPoints != 50 {
		t.Fatalf("关闭后不允许发放：%+v", r)
	}
	g, err := s.GetShare(ctx, 1)
	if err != nil || g.RewardAllowed || g.RewardPoints != 50 {
		t.Fatalf("详情同口径：%+v %v", g, err)
	}
}

func TestClaimRewardDisabled(t *testing.T) {
	s, f, _ := shareFixture(t)
	f.shares[1].Status = model.ShareApproved
	src := &staticRewardSrc{RewardSettings{Enabled: false, Points: 30, MonthlyLimit: 3}}
	s.SetRewardSettings(src)
	if _, err := s.ClaimReward(ctx, 1, 9); xerr.ReasonOf(err) != ReasonRewardDisabled || f.shares[1].RewardStatus != model.RewardNone {
		t.Fatalf("关闭时占位应 409 REWARD_DISABLED 且不落状态：%v", err)
	}
	if _, err := s.ClaimReward(ctx, 99, 9); xerr.ReasonOf(err) == ReasonRewardDisabled || err == nil {
		t.Fatalf("找不到仍报找不到：%v", err)
	}
	// 占位之后关闭：重试占位 / 标记都不受开关影响（钱可能已发出）
	src.v.Enabled = true
	if _, err := s.ClaimReward(ctx, 1, 9); err != nil {
		t.Fatal(err)
	}
	src.v.Enabled = false
	src.v.Points = 77 // 点数快照也不随设置变
	if c, err := s.ClaimReward(ctx, 1, 9); err != nil || c.RewardPoints != 30 {
		t.Fatalf("发放中重试占位不看开关、沿用快照：%+v %v", c, err)
	}
	if r, err := s.MarkRewardGranted(ctx, 1, 9); err != nil || r.RewardStatus != model.RewardGranted || r.RewardPoints != 30 {
		t.Fatalf("关闭后标记发放中的仍成功：%+v %v", r, err)
	}
	if r, err := s.MarkRewardGranted(ctx, 1, 9); err != nil || !r.RewardReplay {
		t.Fatalf("已发放的重放不受开关影响：%+v %v", r, err)
	}
}

func TestClaimRewardLimitFromSettings(t *testing.T) {
	s, f, _ := shareFixture(t)
	for i := int64(1); i <= 6; i++ {
		f.shares[i] = &model.Share{ID: i, UserID: 12345678, JobID: 30 + i, PlanID: 21, Status: model.ShareApproved, RewardStatus: model.RewardNone}
	}
	src := &staticRewardSrc{RewardSettings{Enabled: true, Points: 30, MonthlyLimit: 1}}
	s.SetRewardSettings(src)
	if _, err := s.ClaimReward(ctx, 1, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimReward(ctx, 2, 9); xerr.ReasonOf(err) != ReasonRewardMonthlyLimit {
		t.Fatalf("上限 1：第 2 次应超限：%v", err)
	}
	src.v.MonthlyLimit = 2 // 运营调高后立即生效
	if _, err := s.ClaimReward(ctx, 2, 9); err != nil {
		t.Fatalf("上限调到 2 应可发：%v", err)
	}
	src.v.MonthlyLimit = 0 // 0 = 不限
	for i := int64(3); i <= 6; i++ {
		if _, err := s.ClaimReward(ctx, i, 9); err != nil {
			t.Fatalf("不限时第 %d 条应可发：%v", i, err)
		}
	}
	l, _ := s.ListShares(ctx, ShareListReq{})
	for _, it := range l.Items {
		if it.RewardLimit != 0 || it.MonthRewardCount != 6 {
			t.Fatalf("行：%+v", it)
		}
	}
	f.shares[7] = &model.Share{ID: 7, UserID: 12345678, JobID: 37, PlanID: 21, Status: model.ShareApproved, RewardStatus: model.RewardNone}
	if g, _ := s.GetShare(ctx, 7); !g.RewardAllowed {
		t.Fatalf("不限时 rewardAllowed 恒为真（只要开着）：%+v", g)
	}
}
