package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"
)

// fakeShares 内存 ShareStore（语义对齐 repo/share.go；嵌入 Store 只为满足 New 的签名，只覆盖投稿管理用到的 GetJob / AdminGetPlan）。
type fakeShares struct {
	Store
	id       int64
	shares   map[int64]*model.Share
	jobs     map[int64]*model.Job
	plans    map[int64]*model.Plan
	analyses map[int64]*model.Analysis
	samples  []*model.Sample
	sysAn    []*model.Analysis
}

func newFakeShares() *fakeShares {
	return &fakeShares{shares: map[int64]*model.Share{}, jobs: map[int64]*model.Job{}, plans: map[int64]*model.Plan{}, analyses: map[int64]*model.Analysis{}}
}

func (f *fakeShares) GetJob(_ context.Context, id int64) (*model.Job, error) {
	return cp(f.jobs[id]), nil
}
func (f *fakeShares) AdminGetPlan(_ context.Context, id int64) (*model.Plan, error) {
	return cp(f.plans[id]), nil
}
func (f *fakeShares) GetAnalysis(_ context.Context, id int64) (*model.Analysis, error) {
	return cp(f.analyses[id]), nil
}
func (f *fakeShares) GetShare(_ context.Context, id int64) (*model.Share, error) {
	return cp(f.shares[id]), nil
}
func (f *fakeShares) AdminListShares(_ context.Context, fl repo.ShareFilter, _, _ int) ([]*model.Share, int64, error) {
	var out []*model.Share
	for id := int64(len(f.shares)); id >= 1; id-- {
		if s := f.shares[id]; s != nil && (fl.Status == "" || s.Status == fl.Status) && (fl.UserID == 0 || s.UserID == fl.UserID) {
			out = append(out, cp(s))
		}
	}
	return out, int64(len(out)), nil
}
func (f *fakeShares) RewardCounts(_ context.Context, ids []int64, from, to time.Time) (map[int64]int, error) {
	out := map[int64]int{}
	for _, s := range f.shares {
		at := s.RewardClaimedAt
		if at == nil {
			at = s.RewardAt
		}
		if (s.RewardStatus == model.RewardGranted || s.RewardStatus == model.RewardGranting) && at != nil && !at.Before(from) && at.Before(to) {
			for _, id := range ids {
				if id == s.UserID {
					out[id]++
				}
			}
		}
	}
	return out, nil
}
func (f *fakeShares) ApproveShare(_ context.Context, id, adminID int64, a *model.Analysis, smp *model.Sample) (*model.Share, bool, error) {
	s := f.shares[id]
	if s == nil {
		return nil, false, nil
	}
	if s.Status != model.SharePending {
		return cp(s), false, nil
	}
	a.ID = int64(100 + len(f.sysAn))
	f.sysAn = append(f.sysAn, cp(a))
	smp.AnalysisID = &a.ID
	smp.ID = int64(500 + len(f.samples))
	f.samples = append(f.samples, cp(smp))
	s.Status, s.SampleID, s.ReviewedBy = model.ShareApproved, &smp.ID, adminID
	return cp(s), true, nil
}
func (f *fakeShares) RejectShare(_ context.Context, id, adminID int64, reason string) (bool, error) {
	if s := f.shares[id]; s != nil && s.Status == model.SharePending {
		s.Status, s.RejectReason, s.ReviewedBy = model.ShareRejected, reason, adminID
		return true, nil
	}
	return false, nil
}
func (f *fakeShares) ClaimShareReward(ctx context.Context, id int64, p repo.ClaimParams) (*model.Share, repo.ClaimOutcome, error) {
	s := f.shares[id]
	if s == nil {
		return nil, repo.ClaimNotFound, nil
	}
	switch {
	case s.RewardStatus == model.RewardGranted:
		return cp(s), repo.ClaimGranted, nil
	case s.RewardStatus == model.RewardGranting:
		return cp(s), repo.ClaimReplay, nil
	case s.Status != model.ShareApproved:
		return cp(s), repo.ClaimNotApproved, nil
	case !p.Enabled:
		return cp(s), repo.ClaimDisabled, nil
	}
	n, _ := f.RewardCounts(ctx, []int64{s.UserID}, p.From, p.To)
	if p.Limit > 0 && n[s.UserID] >= p.Limit {
		return cp(s), repo.ClaimLimitHit, nil
	}
	now := p.Now
	s.RewardStatus, s.RewardPoints, s.RewardClaimedBy, s.RewardClaimedAt = model.RewardGranting, p.Points, p.AdminID, &now
	return cp(s), repo.ClaimOK, nil
}
func (f *fakeShares) MarkShareRewardGranted(_ context.Context, id int64, now time.Time) (*model.Share, repo.MarkOutcome, error) {
	s := f.shares[id]
	switch {
	case s == nil:
		return nil, repo.MarkNotFound, nil
	case s.RewardStatus == model.RewardGranting:
		s.RewardStatus, s.RewardAt = model.RewardGranted, &now
		return cp(s), repo.MarkDone, nil
	case s.RewardStatus == model.RewardGranted:
		return cp(s), repo.MarkAlready, nil
	}
	return cp(s), repo.MarkNotClaimed, nil
}
func (f *fakeShares) ReleaseShareReward(_ context.Context, id int64) (*model.Share, repo.ReleaseOutcome, error) {
	s := f.shares[id]
	switch {
	case s == nil:
		return nil, repo.ReleaseNotFound, nil
	case s.RewardStatus == model.RewardGranting:
		s.RewardStatus, s.RewardPoints, s.RewardClaimedBy, s.RewardClaimedAt = model.RewardNone, 0, 0, nil
		return cp(s), repo.ReleaseDone, nil
	case s.RewardStatus == model.RewardGranted:
		return cp(s), repo.ReleaseGranted, nil
	}
	return cp(s), repo.ReleaseNoop, nil
}

type fakeShareMedia struct {
	copies  [][2]string // rid, idem
	failRid string
}

func (m *fakeShareMedia) CopyToLibrary(_ context.Context, rid, purpose, idem string) (mediasvc.Resource, error) {
	if purpose != mediasvc.PurposeAssetLibrary {
		return mediasvc.Resource{}, errors.New("purpose 不对")
	}
	if rid == m.failRid {
		return mediasvc.Resource{}, errors.New("copy 403")
	}
	m.copies = append(m.copies, [2]string{rid, idem})
	return mediasvc.Resource{RID: "lib-asset-" + rid}, nil
}

var now0 = time.Date(2026, 11, 17, 12, 0, 0, 0, cstLoc)

// shareFixture 一条已交付完整成片的待审核投稿（用户 12345678，方案换模特 + 保留原声，备注里有用户私有内容）。
func shareFixture(t *testing.T) (*Service, *fakeShares, *fakeShareMedia) {
	t.Helper()
	f := newFakeShares()
	src := "user-src-rid"
	f.analyses[11] = &model.Analysis{ID: 11, UserID: 12345678, Status: model.AnalysisSucceeded, SourceRid: &src, SourceURL: "https://x/v.mp4",
		Result: json.RawMessage(`{"durationS":10,"summary":"一双白色运动鞋的种草口播"}`), State: json.RawMessage(`{"motion":[1,2]}`)}
	ann, _ := json.Marshal(core.Annotations{Dims: map[string]core.DimEdit{"model": {Mode: core.ModeReplace, Note: "私有备注小红", Refs: []int{1}}}, Audio: core.AudioKeep,
		Texts: []core.TextOverlay{{Text: "私有店名"}}})
	f.plans[21] = &model.Plan{ID: 21, PlanNo: "VRP21", UserID: 12345678, AnalysisID: 11, Annotations: ann}
	doc, _ := json.Marshal(core.ResultDoc{Status: core.ResultReady, Kind: core.ResultFull, VideoRid: "abc-work-final", CoverRid: "abc-work-cover"})
	f.jobs[31] = &model.Job{ID: 31, UserID: 12345678, PlanID: 21, Status: model.JobSucceeded, ResultDocument: doc}
	f.id = 1
	f.shares[1] = &model.Share{ID: 1, UserID: 12345678, JobID: 31, PlanID: 21, Status: model.SharePending, PortraitConsent: true, Industry: "food", RewardStatus: model.RewardNone,
		CreatedAt: now0, UpdatedAt: now0}
	s := New(f, nil, nil)
	s.now = func() time.Time { return now0 }
	m := &fakeShareMedia{}
	s.SetShareMedia(m)
	return s, f, m
}

func TestMaskUserIDAndMonthBounds(t *testing.T) {
	for in, want := range map[int64]string{12345678: "12****78", 12345: "12****45", 1234: "1***", 7: "7"} {
		if got := maskUserID(in); got != want {
			t.Errorf("mask(%d)=%q want %q", in, got, want)
		}
	}
	from, to := monthBounds(time.Date(2026, 12, 31, 23, 59, 0, 0, cstLoc))
	if !from.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, cstLoc)) || !to.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, cstLoc)) {
		t.Fatalf("自然月（北京时间）：%v ~ %v", from, to)
	}
	// UTC 的 11-30 20:00 = 北京时间 12-01 04:00，属于 12 月
	from, _ = monthBounds(time.Date(2026, 11, 30, 20, 0, 0, 0, time.UTC))
	if from.Month() != time.December {
		t.Fatalf("按北京时间划月：%v", from)
	}
}

func TestListSharesRowShape(t *testing.T) {
	s, f, _ := shareFixture(t)
	l, err := s.ListShares(ctx, ShareListReq{Status: "pending"})
	if err != nil || l.Total != 1 || len(l.Items) != 1 {
		t.Fatalf("%+v err=%v", l, err)
	}
	r := l.Items[0]
	if r.UserIDMasked != "12****78" || r.UserID != 12345678 || r.Status != "pending" || r.StatusLabel != "待审核" || r.PlanNo != "VRP21" ||
		r.VideoRid != "abc-work-final" || r.CoverRid != "abc-work-cover" || r.Change.Label != "换模特 + 保留原声" || !r.PortraitConsent ||
		r.RewardKey != "replicate-share-reward:1" || r.RewardLimit != 3 || r.RewardAllowed {
		t.Fatalf("行：%+v", r)
	}
	b, _ := json.Marshal(r)
	for _, bad := range []string{"私有备注", "私有店名", "refs"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("列表不应含用户内容 %q：%s", bad, b)
		}
	}
	if l, _ := s.ListShares(ctx, ShareListReq{Status: "approved"}); l.Total != 0 || len(l.Items) != 0 {
		t.Fatal("状态筛选")
	}
	if _, err := s.ListShares(ctx, ShareListReq{Status: "weird"}); err == nil {
		t.Fatal("非法 status 应拒绝")
	}
	if l, _ := s.ListShares(ctx, ShareListReq{UserID: 9}); l.Total != 0 {
		t.Fatal("用户筛选")
	}
	_ = f
	if _, err := New(nil, nil, nil).ListShares(ctx, ShareListReq{}); err == nil {
		t.Fatal("没装配投稿库应 7003")
	}
}

func TestApproveShareBuildsSampleFromSharedAnalysis(t *testing.T) {
	s, f, m := shareFixture(t)
	row, err := s.ApproveShare(ctx, 1, 9, "")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != model.ShareApproved || row.SampleID == 0 || len(f.samples) != 1 || len(f.sysAn) != 1 {
		t.Fatalf("通过：%+v samples=%d analyses=%d", row, len(f.samples), len(f.sysAn))
	}
	smp, an := f.samples[0], f.sysAn[0]
	if !smp.Enabled || smp.Industry != "food" || smp.SourceRid != "user-src-rid" || *smp.ResultRid != "lib-asset-abc-work-final" || *smp.CoverRid != "lib-asset-abc-work-cover" ||
		smp.CreatedBy != 9 || smp.Title != "一双白色运动鞋的种草口播" || *smp.AnalysisID != an.ID {
		t.Fatalf("示例：%+v", smp)
	}
	if an.UserID != model.SystemUserID || an.Status != model.AnalysisSucceeded || string(an.State) != `{"motion":[1,2]}` {
		t.Fatalf("共享分析应是系统身份整行复制：%+v", an)
	}
	for _, bad := range []string{"私有备注", "私有店名", "refs", "texts"} {
		if strings.Contains(string(smp.Annotations), bad) {
			t.Fatalf("示例改法不应含用户内容 %q：%s", bad, smp.Annotations)
		}
	}
	if _, err := core.ValidSampleAnnotations(smp.Annotations); err != nil {
		t.Fatalf("示例改法过结构校验：%v", err)
	}
	if len(m.copies) != 2 || m.copies[0][1] != "replicate-share:1:result" || m.copies[1][1] != "replicate-share:1:cover" {
		t.Fatalf("成片 / 封面各复制一次，幂等键固定：%v", m.copies)
	}
	// 重复点击：已通过直接返回，不再建示例
	again, err := s.ApproveShare(ctx, 1, 9, "")
	if err != nil || again.SampleID != row.SampleID || len(f.samples) != 1 {
		t.Fatalf("重复通过应幂等：%+v err=%v samples=%d", again, err, len(f.samples))
	}
}

func TestApproveShareTitleGiven(t *testing.T) {
	s, f, _ := shareFixture(t)
	long := strings.Repeat("好", 60)
	if _, err := s.ApproveShare(ctx, 1, 9, "  "+long+"  "); err != nil {
		t.Fatal(err)
	}
	if got := f.samples[0].Title; len([]rune(got)) != 40 {
		t.Fatalf("标题截断到 40 字：%d", len([]rune(got)))
	}
}

func TestApproveShareGuards(t *testing.T) {
	t.Run("不是待审核", func(t *testing.T) {
		for _, st := range []string{model.ShareRejected, model.ShareWithdrawn} {
			s, f, _ := shareFixture(t)
			f.shares[1].Status = st
			if _, err := s.ApproveShare(ctx, 1, 9, ""); xerr.ReasonOf(err) != ReasonShareNotPending || len(f.samples) != 0 {
				t.Errorf("%s 应 SHARE_NOT_PENDING：%v", st, err)
			}
		}
	})
	t.Run("找不到", func(t *testing.T) {
		s, _, _ := shareFixture(t)
		if _, err := s.ApproveShare(ctx, 99, 9, ""); err == nil {
			t.Fatal("应报找不到")
		}
	})
	t.Run("没配媒体服务", func(t *testing.T) {
		s, f, _ := shareFixture(t)
		s.SetShareMedia(nil)
		if _, err := s.ApproveShare(ctx, 1, 9, ""); err == nil || len(f.samples) != 0 {
			t.Fatalf("没配媒体服务不应通过（不让示例引用用户私有桶）：%v", err)
		}
		if code, _, _ := xerr.Parse(func() error { _, e := s.ApproveShare(ctx, 1, 9, ""); return e }()); code != xerr.NotReady {
			t.Fatal("应 7003")
		}
	})
	t.Run("成片已不是完整版", func(t *testing.T) {
		s, f, _ := shareFixture(t)
		f.jobs[31].ResultDocument = json.RawMessage(`{"status":"ready","kind":"preview","videoRid":"x"}`)
		if _, err := s.ApproveShare(ctx, 1, 9, ""); xerr.ReasonOf(err) != core.ReasonJobNotDelivered || f.shares[1].Status != model.SharePending {
			t.Fatalf("应 JOB_NOT_DELIVERED 且仍待审核：%v", err)
		}
	})
	t.Run("原片分析没了", func(t *testing.T) {
		s, f, _ := shareFixture(t)
		delete(f.analyses, 11)
		if _, err := s.ApproveShare(ctx, 1, 9, ""); err == nil || len(f.samples) != 0 {
			t.Fatalf("分析不在不能通过：%v", err)
		}
	})
	t.Run("成片复制失败整条不通过", func(t *testing.T) {
		s, f, m := shareFixture(t)
		m.failRid = "abc-work-final"
		if _, err := s.ApproveShare(ctx, 1, 9, ""); err == nil || f.shares[1].Status != model.SharePending || len(f.samples) != 0 {
			t.Fatalf("成片复制失败应整条失败、仍待审核、不建示例：%v", err)
		}
	})
	t.Run("封面复制失败不挡通过", func(t *testing.T) {
		s, f, m := shareFixture(t)
		m.failRid = "abc-work-cover"
		if _, err := s.ApproveShare(ctx, 1, 9, ""); err != nil || f.samples[0].CoverRid != nil {
			t.Fatalf("封面失败只是没封面：%v", err)
		}
	})
}

func TestRejectShare(t *testing.T) {
	s, f, _ := shareFixture(t)
	if _, err := s.RejectShare(ctx, 1, 9, "  "); xerr.ReasonOf(err) == "" && err == nil {
		t.Fatal("驳回原因必填")
	}
	if _, err := s.RejectShare(ctx, 1, 9, strings.Repeat("字", 201)); err == nil {
		t.Fatal("原因过长应拒绝")
	}
	row, err := s.RejectShare(ctx, 1, 9, " 画面不够清晰 ")
	if err != nil || row.Status != model.ShareRejected || row.RejectReason != "画面不够清晰" || f.shares[1].ReviewedBy != 9 {
		t.Fatalf("%+v err=%v", row, err)
	}
	if again, err := s.RejectShare(ctx, 1, 9, "画面不够清晰"); err != nil || again.Status != model.ShareRejected {
		t.Fatalf("同原因重复驳回应幂等：%v", err)
	}
	if _, err := s.RejectShare(ctx, 1, 9, "换个原因"); xerr.ReasonOf(err) != ReasonShareNotPending {
		t.Fatalf("已驳回再换原因应冲突：%v", err)
	}
	if _, err := s.ApproveShare(ctx, 1, 9, ""); xerr.ReasonOf(err) != ReasonShareNotPending {
		t.Fatalf("驳回后不能再通过：%v", err)
	}
	f.shares[1].Status = model.ShareApproved
	if _, err := s.RejectShare(ctx, 1, 9, "x"); xerr.ReasonOf(err) != ReasonShareNotPending {
		t.Fatalf("已通过不能再驳回：%v", err)
	}
}

// claim → mark 两阶段：幂等、点数快照、月上限（发放 + 发放中都占额度）。
func TestClaimThenMarkIdempotentAndMonthlyLimit(t *testing.T) {
	s, f, _ := shareFixture(t)
	if _, err := s.ClaimReward(ctx, 1, 9); xerr.ReasonOf(err) != ReasonShareNotApproved {
		t.Fatalf("待审核不能占位：%v", err)
	}
	f.shares[1].Status = model.ShareApproved
	l, _ := s.ListShares(ctx, ShareListReq{})
	if !l.Items[0].RewardAllowed {
		t.Fatalf("已通过 + 没发 + 本月 0 次：应允许：%+v", l.Items[0])
	}
	// 没占位就标记：409
	if _, err := s.MarkRewardGranted(ctx, 1, 9); xerr.ReasonOf(err) != ReasonRewardNotClaimed {
		t.Fatalf("没占位不能标记：%v", err)
	}
	c, err := s.ClaimReward(ctx, 1, 9)
	if err != nil || c.RewardStatus != model.RewardGranting || c.Granted || c.RewardPoints != 30 || c.RewardKey != "replicate-share-reward:1" {
		t.Fatalf("占位：%+v err=%v", c, err)
	}
	if g, _ := s.GetShare(ctx, 1); g.RewardAllowed || g.RewardStatus != model.RewardGranting || g.MonthRewardCount != 1 {
		t.Fatalf("发放中不再允许新占位且占额度：%+v", g)
	}
	if again, err := s.ClaimReward(ctx, 1, 9); err != nil || again.RewardStatus != model.RewardGranting || again.RewardPoints != 30 {
		t.Fatalf("重复占位应幂等：%+v err=%v", again, err)
	}
	row, err := s.MarkRewardGranted(ctx, 1, 9)
	if err != nil || row.RewardStatus != model.RewardGranted || row.RewardReplay || row.RewardAllowed || row.MonthRewardCount != 1 || row.RewardAt == nil {
		t.Fatalf("标记：%+v err=%v", row, err)
	}
	if again, err := s.MarkRewardGranted(ctx, 1, 9); err != nil || !again.RewardReplay || again.MonthRewardCount != 1 {
		t.Fatalf("重复标记应幂等（不重复计数）：%+v err=%v", again, err)
	}
	if c, err := s.ClaimReward(ctx, 1, 9); err != nil || !c.Granted || c.RewardStatus != model.RewardGranted {
		t.Fatalf("已发放的占位返回 granted=true：%+v err=%v", c, err)
	}
	// 同一用户再来几条已通过的：本月第 2、3 次可以占位，第 4 次拒
	for i := int64(2); i <= 5; i++ {
		f.shares[i] = &model.Share{ID: i, UserID: 12345678, JobID: 40 + i, PlanID: 21, Status: model.ShareApproved, RewardStatus: model.RewardNone}
		f.id = i
	}
	for i := int64(2); i <= 3; i++ {
		if _, err := s.ClaimReward(ctx, i, 9); err != nil {
			t.Fatalf("第 %d 次应可占位：%v", i, err)
		}
	}
	if _, err := s.ClaimReward(ctx, 4, 9); xerr.ReasonOf(err) != ReasonRewardMonthlyLimit || f.shares[4].RewardStatus != model.RewardNone {
		t.Fatalf("第 4 次应超限且不落状态：%v", err)
	}
	l, _ = s.ListShares(ctx, ShareListReq{Status: "approved"})
	for _, it := range l.Items {
		if it.MonthRewardCount != 3 || (it.ID >= 4 && it.RewardAllowed) {
			t.Fatalf("超限后 rewardAllowed 应置灰：%+v", it)
		}
	}
	// 次月重新计数
	s.now = func() time.Time { return now0.AddDate(0, 1, 0) }
	if c, err := s.ClaimReward(ctx, 4, 9); err != nil || c.RewardStatus != model.RewardGranting {
		t.Fatalf("下个月应重新计数：%+v err=%v", c, err)
	}
	if _, err := s.ClaimReward(ctx, 99, 9); err == nil {
		t.Fatal("找不到应报错")
	}
	if _, err := s.MarkRewardGranted(ctx, 99, 9); err == nil {
		t.Fatal("找不到应报错")
	}
}

// release：granting → none（清快照，让出额度）；none 幂等；granted 拒。
func TestReleaseReward(t *testing.T) {
	s, f, _ := shareFixture(t)
	f.shares[1].Status = model.ShareApproved
	if _, err := s.ClaimReward(ctx, 1, 9); err != nil {
		t.Fatal(err)
	}
	row, err := s.ReleaseReward(ctx, 1, 9)
	if err != nil || row.RewardStatus != model.RewardNone || row.MonthRewardCount != 0 || !row.RewardAllowed {
		t.Fatalf("取消占位：%+v err=%v", row, err)
	}
	if f.shares[1].RewardClaimedAt != nil || f.shares[1].RewardClaimedBy != 0 {
		t.Fatalf("占位信息应清空：%+v", f.shares[1])
	}
	if _, err := s.ReleaseReward(ctx, 1, 9); err != nil {
		t.Fatalf("none 重复取消应幂等：%v", err)
	}
	_, _ = s.ClaimReward(ctx, 1, 9)
	_, _ = s.MarkRewardGranted(ctx, 1, 9)
	if _, err := s.ReleaseReward(ctx, 1, 9); xerr.ReasonOf(err) != ReasonRewardAlreadyGranted {
		t.Fatalf("已发放不能取消：%v", err)
	}
	if _, err := s.ReleaseReward(ctx, 99, 9); err == nil {
		t.Fatal("找不到应报错")
	}
}

func TestMarkRewardRejectsWithdrawn(t *testing.T) {
	s, f, _ := shareFixture(t)
	f.shares[1].Status = model.ShareWithdrawn
	if _, err := s.ClaimReward(ctx, 1, 9); xerr.ReasonOf(err) != ReasonShareNotApproved {
		t.Fatalf("用户撤回的投稿不发奖励：%v", err)
	}
}
