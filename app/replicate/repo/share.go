package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
)

// 灵感推荐（CONTRACT §S）与成片分享投稿（CONTRACT §T，Go_V1.0.58）。

var (
	_ core.TrendingStore = (*Repo)(nil)
	_ core.ShareStore    = (*Repo)(nil)
)

// ---------------------------------------------------------------- 灵感推荐

// ConfirmedPlanChanges 近期已确认出片的方案（plan.status=CONFIRMED 且任务创建于 since 之后），只取 user_id + 改法两列，新的在前。
// 一张方案对应一个任务（plan.job_id），按方案去重；时间交给库换算。
func (r *Repo) ConfirmedPlanChanges(ctx context.Context, since time.Time, limit int) ([]core.TrendingRow, error) {
	var rows []struct {
		UserID      int64  `bun:"user_id"`
		Annotations string `bun:"annotations"`
	}
	err := r.db.NewRaw("SELECT p.user_id AS user_id, p.annotations AS annotations FROM ? p JOIN ? j ON j.plan_id = p.id "+
		"WHERE p.status = ? AND j.created_at >= ? ORDER BY p.id DESC LIMIT ?",
		bun.Ident(model.TablePlan), bun.Ident(model.TableJob), model.PlanConfirmed, msExpr(since), limit).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]core.TrendingRow, 0, len(rows))
	for _, x := range rows {
		out = append(out, core.TrendingRow{UserID: x.UserID, Annotations: []byte(x.Annotations)})
	}
	return out, nil
}

// userIndustryChunk 一次查多少个用户的行业偏好（IN 列表不要太长）。
const userIndustryChunk = 1000

// UserIndustries 用户在广场设置的行业 code（只读复用 afv_user_industry_pref + afv_template_industry）：
// 一级行业给它自己的 code；二级行业给它自己与父级的 code（统计时二级用户也算进一级行业）。
func (r *Repo) UserIndustries(ctx context.Context, userIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	for i := 0; i < len(userIDs); i += userIndustryChunk {
		part := userIDs[i:min(i+userIndustryChunk, len(userIDs))]
		var rows []struct {
			UserID int64          `bun:"user_id"`
			Code   string         `bun:"code"`
			Parent sql.NullString `bun:"parent_code"`
		}
		err := r.db.NewRaw("SELECT uip.user_id AS user_id, ti.code AS code, par.code AS parent_code FROM afv_user_industry_pref uip "+
			"JOIN afv_template_industry ti ON ti.id = uip.industry_id AND ti.deleted = 0 "+
			"LEFT JOIN afv_template_industry par ON par.id = ti.parent_id AND par.deleted = 0 "+
			"WHERE uip.user_id IN (?)", bun.In(part)).Scan(ctx, &rows)
		if err != nil {
			return nil, err
		}
		for _, x := range rows {
			out[x.UserID] = append(out[x.UserID], x.Code)
			if x.Parent.Valid && x.Parent.String != "" {
				out[x.UserID] = append(out[x.UserID], x.Parent.String)
			}
		}
	}
	return out, nil
}

// UserIndustry 用户的主行业（一级 code：主偏好是二级时取它的父级，更宽、统计更稳）；没设置返回空串。
func (r *Repo) UserIndustry(ctx context.Context, userID int64) (string, error) {
	var row struct {
		Code   string         `bun:"code"`
		Parent sql.NullString `bun:"parent_code"`
	}
	err := r.db.NewRaw("SELECT ti.code AS code, par.code AS parent_code FROM afv_user_industry_pref uip "+
		"JOIN afv_template_industry ti ON ti.id = uip.industry_id AND ti.deleted = 0 "+
		"LEFT JOIN afv_template_industry par ON par.id = ti.parent_id AND par.deleted = 0 "+
		"WHERE uip.user_id = ? ORDER BY uip.is_primary DESC, uip.sort_order, uip.id LIMIT 1", userID).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if row.Parent.Valid && row.Parent.String != "" {
		return row.Parent.String, nil
	}
	return row.Code, nil
}

// ---------------------------------------------------------------- 投稿（用户侧）

func (r *Repo) GetShare(ctx context.Context, id int64) (*model.Share, error) {
	var sh model.Share
	return noRows(&sh, r.db.NewSelect().Model(&sh).Where("sh.id = ?", id).Limit(1).Scan(ctx))
}

func (r *Repo) GetShareByJob(ctx context.Context, jobID int64) (*model.Share, error) {
	var sh model.Share
	return noRows(&sh, r.db.NewSelect().Model(&sh).Where("sh.job_id = ?", jobID).Limit(1).Scan(ctx))
}

// InsertShare 新建投稿；job_id 唯一键撞上 = 已有投稿（inserted=false）。
func (r *Repo) InsertShare(ctx context.Context, sh *model.Share) (bool, error) {
	if _, err := r.db.NewInsert().Model(sh).Exec(ctx); err != nil {
		if isDup(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReopenShare 撤回过的投稿重新投：withdrawn → pending（CAS）；审核痕迹清空，奖励状态保留（已发放的奖励不会因撤回再投而重复发）。
func (r *Repo) ReopenShare(ctx context.Context, id int64, consent bool, industry string) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableShare).Set("status = ?", model.SharePending).Set("portrait_consent = ?", consent).
		Set("industry = ?", clip(industry, 64)).Set("reject_reason = ''").Set("sample_id = NULL").Set("reviewed_by = 0").Set("reviewed_at = NULL").
		Where("id = ? AND status = ?", id, model.ShareWithdrawn).Exec(ctx))
}

// WithdrawShare 本人撤回（同一事务，锁投稿行）：pending / approved → withdrawn；approved 的把生成的示例下架
// （逻辑删除 + 停用，与后台 DeleteSample 同一套更新）。已撤回 / 已驳回不改。
func (r *Repo) WithdrawShare(ctx context.Context, userID, id int64) (*model.Share, bool, error) {
	var prev *model.Share
	changed := false
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var sh model.Share
		err := tx.NewSelect().Model(&sh).Where("sh.id = ? AND sh.user_id = ?", id, userID).For("UPDATE").Limit(1).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		cp := sh
		prev = &cp
		if sh.Status != model.SharePending && sh.Status != model.ShareApproved {
			return nil
		}
		if _, err := tx.NewUpdate().Table(model.TableShare).Set("status = ?", model.ShareWithdrawn).Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		if sh.Status == model.ShareApproved && sh.SampleID != nil {
			if _, err := tx.NewUpdate().Table(model.TableSample).Set("deleted = 1").Set("enabled = 0").Set("updated_by = 0").
				Where("id = ? AND deleted = 0", *sh.SampleID).Exec(ctx); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return prev, changed, nil
}

// ---------------------------------------------------------------- 投稿（管理端）

// ShareFilter 管理端投稿检索。
type ShareFilter struct {
	Status string // 空 = 全部
	UserID int64  // 0 = 不限
}

// AdminListShares 投稿列表（id 倒序）。
func (r *Repo) AdminListShares(ctx context.Context, f ShareFilter, offset, limit int) ([]*model.Share, int64, error) {
	var out []*model.Share
	q := r.db.NewSelect().Model(&out)
	if f.Status != "" {
		q = q.Where("sh.status = ?", f.Status)
	}
	if f.UserID > 0 {
		q = q.Where("sh.user_id = ?", f.UserID)
	}
	n, err := q.OrderExpr("sh.id DESC").Offset(int64(offset)).Limit(int64(limit)).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return out, int64(n), nil
}

// rewardMonthExpr 奖励落月时间：占位时间优先（占位即占额度），老数据回落发放时间。
const rewardMonthExpr = "COALESCE(reward_claimed_at, reward_at)"

// RewardCounts 这些用户在 [from, to) 内已发放 + 发放中（已占位）的奖励次数。
func (r *Repo) RewardCounts(ctx context.Context, userIDs []int64, from, to time.Time) (map[int64]int, error) {
	out := map[int64]int{}
	if len(userIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		UserID int64 `bun:"user_id"`
		N      int   `bun:"n"`
	}
	err := r.db.NewRaw("SELECT user_id, COUNT(*) AS n FROM ? WHERE user_id IN (?) AND reward_status IN (?, ?) AND "+rewardMonthExpr+" >= ? AND "+rewardMonthExpr+" < ? GROUP BY user_id",
		bun.Ident(model.TableShare), bun.In(userIDs), model.RewardGranted, model.RewardGranting, msExpr(from), msExpr(to)).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out[x.UserID] = x.N
	}
	return out, nil
}

// ApproveShare 审核通过（同一事务）：锁投稿行 → 仍是 pending 才继续 → 插入系统身份的分析行（(user_id=0, source_key) 撞键 = 重放，复用已有那行）
// → 插入示例（analysis_id 指向它，启用）→ 投稿 pending → approved + sample_id。返回最新投稿行与这次是否真的通过。
func (r *Repo) ApproveShare(ctx context.Context, id, adminID int64, a *model.Analysis, smp *model.Sample) (*model.Share, bool, error) {
	var out *model.Share
	approved := false
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var sh model.Share
		if err := tx.NewSelect().Model(&sh).Where("sh.id = ?", id).For("UPDATE").Limit(1).Scan(ctx); err != nil {
			return err
		}
		out = &sh
		if sh.Status != model.SharePending {
			return nil
		}
		if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
			if !isDup(err) {
				return err
			}
			var got model.Analysis
			if err := tx.NewSelect().Model(&got).Where("a.user_id = ? AND a.source_key = ?", a.UserID, a.SourceKey).Limit(1).Scan(ctx); err != nil {
				return err
			}
			a.ID = got.ID
		}
		smp.AnalysisID = &a.ID
		if _, err := tx.NewInsert().Model(smp).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Table(model.TableShare).Set("status = ?", model.ShareApproved).Set("sample_id = ?", smp.ID).
			Set("reject_reason = ''").Set("reviewed_by = ?", adminID).Set("reviewed_at = NOW(3)").Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		sh.Status, sh.SampleID = model.ShareApproved, &smp.ID
		approved = true
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return out, approved, nil
}

// RejectShare 驳回：pending → rejected（CAS），记原因与审核人。没抢到（已不是待审核）返回 false。
func (r *Repo) RejectShare(ctx context.Context, id, adminID int64, reason string) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableShare).Set("status = ?", model.ShareRejected).Set("reject_reason = ?", clip(reason, 255)).
		Set("reviewed_by = ?", adminID).Set("reviewed_at = NOW(3)").Where("id = ? AND status = ?", id, model.SharePending).Exec(ctx))
}

// ClaimOutcome 占位（claim）的结果。
type ClaimOutcome int

const (
	ClaimOK          ClaimOutcome = iota // 这次新占位成功（none → granting）
	ClaimReplay                          // 已是发放中（任何人占的）：原样返回，点数沿用快照
	ClaimGranted                         // 已发放
	ClaimNotApproved                     // 投稿不是已通过状态
	ClaimDisabled                        // 奖励已关闭
	ClaimLimitHit                        // 同一用户本月（发放 + 发放中）已达上限
	ClaimNotFound                        // 没有这条投稿
)

// ClaimParams 占位参数（规则取自调用时刻的奖励设置）。
type ClaimParams struct {
	Enabled  bool
	Points   int
	Limit    int // <=0 = 不限
	From, To time.Time
	Now      time.Time
	AdminID  int64
}

// lockUserShares 同一事务内：按 id 顺序锁住该用户全部投稿行（同一用户并发占位 / 标记串行化、锁序固定不死锁），返回目标那行。
func lockUserShares(ctx context.Context, tx bun.Tx, id int64) (*model.Share, error) {
	var uid int64
	if err := tx.NewSelect().Table(model.TableShare).Column("user_id").Where("id = ?", id).Limit(1).Scan(ctx, &uid); err != nil {
		return nil, err
	}
	var rows []*model.Share
	if err := tx.NewSelect().Model(&rows).Where("sh.user_id = ?", uid).OrderExpr("sh.id").For("UPDATE").Scan(ctx); err != nil {
		return nil, err
	}
	for _, x := range rows {
		if x.ID == id {
			return x, nil
		}
	}
	return nil, nil
}

// ClaimShareReward 占位「奖励发放」（两阶段的第一阶段，管理端在调 Java 加积分之前调）：
// 同一事务内先锁用户的全部投稿行，再在锁内判断：已发放 → ClaimGranted；已是发放中 → ClaimReplay（幂等，点数快照不变，重试可继续）；
// 不是已通过 → 拒；奖励已关闭 → 拒；本月（[From, To)）已发放 + 发放中 ≥ Limit → 拒；
// 否则 reward_status none → granting，记点数快照 / 占位人 / 占位时间。占位成功之后的 Java 加点与 mark 不再受开关 / 上限影响。
func (r *Repo) ClaimShareReward(ctx context.Context, id int64, p ClaimParams) (*model.Share, ClaimOutcome, error) {
	var out *model.Share
	outcome := ClaimNotFound
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		sh, err := lockUserShares(ctx, tx, id)
		if err != nil || sh == nil {
			return err
		}
		out = sh
		switch {
		case sh.RewardStatus == model.RewardGranted:
			outcome = ClaimGranted
			return nil
		case sh.RewardStatus == model.RewardGranting:
			outcome = ClaimReplay
			return nil
		case sh.Status != model.ShareApproved:
			outcome = ClaimNotApproved
			return nil
		case !p.Enabled:
			outcome = ClaimDisabled
			return nil
		}
		// 必须是锁定读（FOR UPDATE，读最新已提交）：事务第一条普通 SELECT 已在 REPEATABLE READ 下定好快照，
		// 普通 COUNT 会看不到排在我们前面的并发事务刚提交的占位，导致并发超发。
		var n int
		if err := tx.NewSelect().Table(model.TableShare).ColumnExpr("COUNT(*)").
			Where("user_id = ? AND reward_status IN (?, ?) AND "+rewardMonthExpr+" >= ? AND "+rewardMonthExpr+" < ?",
				sh.UserID, model.RewardGranted, model.RewardGranting, msExpr(p.From), msExpr(p.To)).For("UPDATE").Scan(ctx, &n); err != nil {
			return err
		}
		if p.Limit > 0 && n >= p.Limit {
			outcome = ClaimLimitHit
			return nil
		}
		if _, err := tx.NewUpdate().Table(model.TableShare).Set("reward_status = ?", model.RewardGranting).Set("reward_points = ?", p.Points).
			Set("reward_claimed_by = ?", p.AdminID).Set("reward_claimed_at = ?", msExpr(p.Now)).Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		sh.RewardStatus, sh.RewardPoints, sh.RewardClaimedBy, sh.RewardClaimedAt = model.RewardGranting, p.Points, p.AdminID, &p.Now
		outcome = ClaimOK
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ClaimNotFound, nil
	}
	if err != nil {
		return nil, ClaimNotFound, err
	}
	return out, outcome, nil
}

// MarkOutcome 标记（mark）的结果。
type MarkOutcome int

const (
	MarkDone       MarkOutcome = iota // granting → granted 成功
	MarkAlready                       // 之前已经 granted（幂等重放）
	MarkNotClaimed                    // 还没占位（none），不能标记
	MarkNotFound                      // 没有这条投稿
)

// MarkShareRewardGranted 两阶段的第三阶段：granting → granted（CAS，无条件——积分已经发出，不再看开关 / 月上限）。
// granted 重放幂等；none（没占位）→ MarkNotClaimed。返回最新投稿行。
func (r *Repo) MarkShareRewardGranted(ctx context.Context, id int64, now time.Time) (*model.Share, MarkOutcome, error) {
	ok, err := one(r.db.NewUpdate().Table(model.TableShare).Set("reward_status = ?", model.RewardGranted).Set("reward_at = ?", msExpr(now)).
		Where("id = ? AND reward_status = ?", id, model.RewardGranting).Exec(ctx))
	if err != nil {
		return nil, MarkNotFound, err
	}
	sh, err := r.GetShare(ctx, id)
	if err != nil {
		return nil, MarkNotFound, err
	}
	switch {
	case sh == nil:
		return nil, MarkNotFound, nil
	case ok:
		return sh, MarkDone, nil
	case sh.RewardStatus == model.RewardGranted:
		return sh, MarkAlready, nil
	}
	return sh, MarkNotClaimed, nil
}

// ReleaseOutcome 取消占位（release）的结果。
type ReleaseOutcome int

const (
	ReleaseDone     ReleaseOutcome = iota // granting → none 成功
	ReleaseNoop                           // 本来就是 none（幂等）
	ReleaseGranted                        // 已经 granted，不能取消
	ReleaseNotFound                       // 没有这条投稿
)

// ReleaseShareReward 取消占位：granting → none（CAS），清点数快照与占位信息，让出月额度。只用于 Java 明确没加点（调用都没发出 / 明确不可重试的失败）。
func (r *Repo) ReleaseShareReward(ctx context.Context, id int64) (*model.Share, ReleaseOutcome, error) {
	ok, err := one(r.db.NewUpdate().Table(model.TableShare).Set("reward_status = ?", model.RewardNone).Set("reward_points = 0").
		Set("reward_claimed_by = 0").Set("reward_claimed_at = NULL").
		Where("id = ? AND reward_status = ?", id, model.RewardGranting).Exec(ctx))
	if err != nil {
		return nil, ReleaseNotFound, err
	}
	sh, err := r.GetShare(ctx, id)
	if err != nil {
		return nil, ReleaseNotFound, err
	}
	switch {
	case sh == nil:
		return nil, ReleaseNotFound, nil
	case ok:
		return sh, ReleaseDone, nil
	case sh.RewardStatus == model.RewardGranted:
		return sh, ReleaseGranted, nil
	}
	return sh, ReleaseNoop, nil
}
