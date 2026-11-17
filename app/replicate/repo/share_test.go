package repo

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var t0 = time.UnixMilli(1763000000123)

// 近 7 天已确认方案：按方案取 user_id + 改法，时间交给库换算。
func TestConfirmedPlanChanges(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("SELECT p.user_id AS user_id, p.annotations AS annotations FROM `afv_replicate_plan` p JOIN `afv_replicate_job` j ON j.plan_id = p.id " +
		"WHERE p.status = 'CONFIRMED' AND j.created_at >= FROM_UNIXTIME(1763000000123 / 1000) ORDER BY p.id DESC LIMIT 100")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "annotations"}).AddRow(7, `{"dims":{},"audio":"keep"}`).AddRow(8, `{}`))
	rows, err := r.ConfirmedPlanChanges(ctx, t0, 100)
	if err != nil || len(rows) != 2 || rows[0].UserID != 7 || string(rows[0].Annotations) != `{"dims":{},"audio":"keep"}` {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

// 行业偏好：二级行业连同父级一起返回；没设置的用户不在结果里。
func TestUserIndustries(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery("FROM afv_user_industry_pref uip JOIN afv_template_industry ti").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "code", "parent_code"}).AddRow(7, "shoes", "fashion").AddRow(8, "food", nil))
	got, err := r.UserIndustries(ctx, []int64{7, 8, 9})
	if err != nil || len(got[7]) != 2 || got[7][0] != "shoes" || got[7][1] != "fashion" || len(got[8]) != 1 || got[8][0] != "food" || len(got[9]) != 0 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if m, err := r.UserIndustries(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("空入参不查库：%v %v", m, err)
	}
}

func TestUserIndustryPrefersParentAndHandlesNone(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery("ORDER BY uip.is_primary DESC").WillReturnRows(sqlmock.NewRows([]string{"code", "parent_code"}).AddRow("shoes", "fashion"))
	if c, err := r.UserIndustry(ctx, 7); err != nil || c != "fashion" {
		t.Fatalf("二级主偏好取父级：%q %v", c, err)
	}
	mock.ExpectQuery("ORDER BY uip.is_primary DESC").WillReturnRows(sqlmock.NewRows([]string{"code", "parent_code"}))
	if c, err := r.UserIndustry(ctx, 8); err != nil || c != "" {
		t.Fatalf("没设置返回空：%q %v", c, err)
	}
}

// 重新投稿只认 withdrawn（CAS），并清掉审核痕迹；奖励状态不动。
func TestReopenShareCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_share` SET status = 'pending', portrait_consent = TRUE, industry = 'food', reject_reason = '', sample_id = NULL, reviewed_by = 0, reviewed_at = NULL WHERE (id = 5 AND status = 'withdrawn')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.ReopenShare(ctx, 5, true, "food"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestRejectShareCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_share` SET status = 'rejected', reject_reason = '画面不清', reviewed_by = 9, reviewed_at = NOW(3) WHERE (id = 5 AND status = 'pending')")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if ok, err := r.RejectShare(ctx, 5, 9, "画面不清"); err != nil || ok {
		t.Fatalf("已不是待审核应没抢到：ok=%v err=%v", ok, err)
	}
}

var shareCols = []string{"id", "user_id", "job_id", "plan_id", "status", "reject_reason", "sample_id", "portrait_consent", "industry", "reward_status", "reward_at", "reviewed_by", "reviewed_at", "created_at", "updated_at"}

// 撤回已通过的投稿：同一事务里改状态并下架示例（逻辑删除 + 停用）。
func TestWithdrawShareApprovedUnpublishesSample(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery("FROM `afv_replicate_share` AS `sh` WHERE \\(sh.id = 5 AND sh.user_id = 7\\).*FOR UPDATE").
		WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 1, "food", "none", nil, 9, nil, t0, t0))
	mock.ExpectExec(q("UPDATE `afv_replicate_share` SET status = 'withdrawn' WHERE (id = 5)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("UPDATE `afv_replicate_sample` SET deleted = 1, enabled = 0, updated_by = 0 WHERE (id = 88 AND deleted = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	prev, changed, err := r.WithdrawShare(ctx, 7, 5)
	if err != nil || !changed || prev == nil || prev.Status != "approved" || prev.SampleID == nil || *prev.SampleID != 88 {
		t.Fatalf("prev=%+v changed=%v err=%v", prev, changed, err)
	}
}

// 已驳回 / 已撤回的不改；不是本人的返回 nil。
func TestWithdrawShareNoopAndNotOwner(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "rejected", "x", nil, 0, "", "none", nil, 9, nil, t0, t0))
	mock.ExpectCommit()
	if prev, changed, err := r.WithdrawShare(ctx, 7, 5); err != nil || changed || prev == nil || prev.Status != "rejected" {
		t.Fatalf("prev=%+v changed=%v err=%v", prev, changed, err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("FOR UPDATE").WillReturnRows(sqlmock.NewRows(shareCols))
	mock.ExpectCommit()
	if prev, changed, err := r.WithdrawShare(ctx, 8, 5); err != nil || changed || prev != nil {
		t.Fatalf("不是本人应返回 nil：prev=%+v changed=%v err=%v", prev, changed, err)
	}
}

// 占位奖励：先按 id 顺序锁住该用户的全部投稿行，再在锁内数本月已发放 + 发放中次数；已达上限不落状态。
func TestClaimShareRewardLocksUserRowsThenChecksLimit(t *testing.T) {
	p := ClaimParams{Enabled: true, Points: 30, Limit: 3, From: time.UnixMilli(1762000000000), To: time.UnixMilli(1764000000000), Now: t0, AdminID: 9}
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(q("SELECT `user_id` FROM `afv_replicate_share` WHERE (id = 5) LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(7))
	mock.ExpectQuery("WHERE \\(sh.user_id = 7\\) ORDER BY sh.id FOR UPDATE").
		WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", "none", nil, 9, nil, t0, t0))
	mock.ExpectQuery(q("SELECT COUNT(*) FROM `afv_replicate_share` WHERE (user_id = 7 AND reward_status IN ('granted', 'granting') AND COALESCE(reward_claimed_at, reward_at) >= FROM_UNIXTIME(1762000000000 / 1000) AND COALESCE(reward_claimed_at, reward_at) < FROM_UNIXTIME(1764000000000 / 1000)) FOR UPDATE")).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(3))
	mock.ExpectCommit()
	sh, out, err := r.ClaimShareReward(ctx, 5, p)
	if err != nil || out != ClaimLimitHit || sh == nil || sh.RewardStatus != "none" {
		t.Fatalf("本月已 3 次应拒：out=%v sh=%+v err=%v", out, sh, err)
	}
}

func TestClaimShareRewardClaimsAndReplays(t *testing.T) {
	p := ClaimParams{Enabled: true, Points: 30, Limit: 3, From: time.UnixMilli(1762000000000), To: time.UnixMilli(1764000000000), Now: t0, AdminID: 9}
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT `user_id`").WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(7))
	mock.ExpectQuery("FOR UPDATE").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", "none", nil, 9, nil, t0, t0))
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(2))
	mock.ExpectExec(q("UPDATE `afv_replicate_share` SET reward_status = 'granting', reward_points = 30, reward_claimed_by = 9, reward_claimed_at = FROM_UNIXTIME(1763000000123 / 1000) WHERE (id = 5)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if sh, out, err := r.ClaimShareReward(ctx, 5, p); err != nil || out != ClaimOK || sh.RewardStatus != "granting" || sh.RewardPoints != 30 {
		t.Fatalf("out=%v sh=%+v err=%v", out, sh, err)
	}
	// 已是发放中：直接返回 Replay，不数、不写（即使奖励已关闭）
	p.Enabled = false
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT `user_id`").WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(7))
	mock.ExpectQuery("FOR UPDATE").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", "granting", nil, 9, nil, t0, t0))
	mock.ExpectCommit()
	if _, out, err := r.ClaimShareReward(ctx, 5, p); err != nil || out != ClaimReplay {
		t.Fatalf("out=%v err=%v", out, err)
	}
	// 不存在
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT `user_id`").WillReturnRows(sqlmock.NewRows([]string{"user_id"}))
	mock.ExpectRollback()
	if _, out, err := r.ClaimShareReward(ctx, 6, p); err != nil || out != ClaimNotFound {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestClaimShareRewardDisabledAndNotApproved(t *testing.T) {
	p := ClaimParams{Enabled: false, Points: 30, Limit: 3, Now: t0}
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT `user_id`").WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(7))
	mock.ExpectQuery("FOR UPDATE").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", "none", nil, 9, nil, t0, t0))
	mock.ExpectCommit()
	if _, out, err := r.ClaimShareReward(ctx, 5, p); err != nil || out != ClaimDisabled {
		t.Fatalf("关闭应拒（不数不写）：out=%v err=%v", out, err)
	}
	p.Enabled = true
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT `user_id`").WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(7))
	mock.ExpectQuery("FOR UPDATE").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "withdrawn", "", 88, 0, "", "none", nil, 9, nil, t0, t0))
	mock.ExpectCommit()
	if _, out, err := r.ClaimShareReward(ctx, 5, p); err != nil || out != ClaimNotApproved {
		t.Fatalf("撤回应拒：out=%v err=%v", out, err)
	}
}

// mark 是 CAS（granting → granted），不再数月额度。
func TestMarkShareRewardGrantedCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_share` SET reward_status = 'granted', reward_at = FROM_UNIXTIME(1763000000123 / 1000) WHERE (id = 5 AND reward_status = 'granting')")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("WHERE \\(sh.id = 5\\)").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", "granted", t0, 9, nil, t0, t0))
	if sh, out, err := r.MarkShareRewardGranted(ctx, 5, t0); err != nil || out != MarkDone || sh.RewardStatus != "granted" {
		t.Fatalf("out=%v sh=%+v err=%v", out, sh, err)
	}
	// 没抢到：已 granted → Already；none → NotClaimed
	for st, want := range map[string]MarkOutcome{"granted": MarkAlready, "none": MarkNotClaimed} {
		mock.ExpectExec("UPDATE `afv_replicate_share`").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("WHERE \\(sh.id = 5\\)").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", st, nil, 9, nil, t0, t0))
		if _, out, err := r.MarkShareRewardGranted(ctx, 5, t0); err != nil || out != want {
			t.Fatalf("%s：out=%v err=%v", st, out, err)
		}
	}
}

func TestReleaseShareRewardCAS(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_share` SET reward_status = 'none', reward_points = 0, reward_claimed_by = 0, reward_claimed_at = NULL WHERE (id = 5 AND reward_status = 'granting')")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("WHERE \\(sh.id = 5\\)").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", "none", nil, 9, nil, t0, t0))
	if _, out, err := r.ReleaseShareReward(ctx, 5); err != nil || out != ReleaseDone {
		t.Fatalf("out=%v err=%v", out, err)
	}
	for st, want := range map[string]ReleaseOutcome{"granted": ReleaseGranted, "none": ReleaseNoop} {
		mock.ExpectExec("UPDATE `afv_replicate_share`").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("WHERE \\(sh.id = 5\\)").WillReturnRows(sqlmock.NewRows(shareCols).AddRow(5, 7, 31, 21, "approved", "", 88, 0, "", st, nil, 9, nil, t0, t0))
		if _, out, err := r.ReleaseShareReward(ctx, 5); err != nil || out != want {
			t.Fatalf("%s：out=%v err=%v", st, out, err)
		}
	}
}
