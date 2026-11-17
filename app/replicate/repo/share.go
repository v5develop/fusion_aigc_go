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

// RewardCounts 这些用户在 [from, to) 内已发放奖励的次数（reward_at 落在区间内的 granted 行）。
func (r *Repo) RewardCounts(ctx context.Context, userIDs []int64, from, to time.Time) (map[int64]int, error) {
	out := map[int64]int{}
	if len(userIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		UserID int64 `bun:"user_id"`
		N      int   `bun:"n"`
	}
	err := r.db.NewRaw("SELECT user_id, COUNT(*) AS n FROM ? WHERE user_id IN (?) AND reward_status = ? AND reward_at >= ? AND reward_at < ? GROUP BY user_id",
		bun.Ident(model.TableShare), bun.In(userIDs), model.RewardGranted, msExpr(from), msExpr(to)).Scan(ctx, &rows)
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

// RewardOutcome 标记奖励已发放的结果。
type RewardOutcome int

const (
	RewardGranted     RewardOutcome = iota // 这次标记成功
	RewardAlready                          // 之前已经标记过（幂等重放）
	RewardLimitHit                         // 同一用户本月已达上限
	RewardNotApproved                      // 投稿不是已通过状态
	RewardNotFound                         // 没有这条投稿
)

// GrantShareReward 标记「奖励已发放」（幂等；管理端在 Java 积分接口调用成功之后调）：
// 先按 id 顺序锁住该用户的全部投稿行（同一用户并发标记串行化、锁序固定不死锁），再在锁内判断：已发放 → 重放；
// 不是已通过 → 拒；本月（[from, to)）已发放 ≥ limit → 拒；否则 reward_status=granted、reward_at=now。返回最新投稿行。
func (r *Repo) GrantShareReward(ctx context.Context, id int64, limit int, from, to, now time.Time) (*model.Share, RewardOutcome, error) {
	var out *model.Share
	outcome := RewardNotFound
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var uid int64
		if err := tx.NewSelect().Table(model.TableShare).Column("user_id").Where("id = ?", id).Limit(1).Scan(ctx, &uid); err != nil {
			return err
		}
		var rows []*model.Share
		if err := tx.NewSelect().Model(&rows).Where("sh.user_id = ?", uid).OrderExpr("sh.id").For("UPDATE").Scan(ctx); err != nil {
			return err
		}
		var sh *model.Share
		for _, x := range rows {
			if x.ID == id {
				sh = x
			}
		}
		if sh == nil {
			return nil
		}
		out = sh
		switch {
		case sh.RewardStatus == model.RewardGranted:
			outcome = RewardAlready
			return nil
		case sh.Status != model.ShareApproved:
			outcome = RewardNotApproved
			return nil
		}
		// 必须是锁定读（FOR UPDATE，读最新已提交）：事务第一条普通 SELECT（上面读 user_id）已在 REPEATABLE READ 下定好快照，
		// 普通 COUNT 会看不到排在我们前面的并发事务刚提交的发放，导致并发超发。
		var n int
		if err := tx.NewSelect().Table(model.TableShare).ColumnExpr("COUNT(*)").
			Where("user_id = ? AND reward_status = ? AND reward_at >= ? AND reward_at < ?", uid, model.RewardGranted, msExpr(from), msExpr(to)).For("UPDATE").Scan(ctx, &n); err != nil {
			return err
		}
		if n >= limit {
			outcome = RewardLimitHit
			return nil
		}
		if _, err := tx.NewUpdate().Table(model.TableShare).Set("reward_status = ?", model.RewardGranted).Set("reward_at = ?", msExpr(now)).
			Where("id = ?", id).Exec(ctx); err != nil {
			return err
		}
		sh.RewardStatus, sh.RewardAt = model.RewardGranted, &now
		outcome = RewardGranted
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, RewardNotFound, nil
	}
	if err != nil {
		return nil, RewardNotFound, err
	}
	return out, outcome, nil
}
