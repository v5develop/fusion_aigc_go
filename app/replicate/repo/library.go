package repo

import (
	"context"
	"database/sql"
	"errors"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
)

// 示例 / 改标注 / 我的方案（Go_V1.0.41）。

var _ core.LibraryStore = (*Repo)(nil)

// ---------------------------------------------------------------- 示例

func (r *Repo) GetSample(ctx context.Context, id int64) (*model.Sample, error) {
	var s model.Sample
	return noRows(&s, r.db.NewSelect().Model(&s).Where("sm.id = ?", id).Limit(1).Scan(ctx))
}

func (r *Repo) ListVisibleSamples(ctx context.Context, industry string, limit int) ([]*model.Sample, error) {
	var out []*model.Sample
	q := r.db.NewSelect().Model(&out).Where("sm.deleted = 0 AND sm.enabled = 1")
	if industry != "" {
		q = q.Where("sm.industry IN (?)", bun.In([]string{industry, ""}))
	}
	err := q.OrderExpr("sm.sort_order, sm.id DESC").Limit(int64(limit)).Scan(ctx)
	return out, err
}

func (r *Repo) PromoteSampleAnalysis(ctx context.Context, sampleID, pendingID int64) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableSample).Set("analysis_id = pending_analysis_id").Set("pending_analysis_id = NULL").
		Set("analysis_error = NULL").Set("analyzed_at = NOW(3)").
		Where("id = ? AND pending_analysis_id = ?", sampleID, pendingID).Exec(ctx))
}

func (r *Repo) FailSampleAnalysis(ctx context.Context, sampleID, pendingID int64, reason string) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableSample).Set("pending_analysis_id = NULL").Set("analysis_error = ?", clip(reason, 255)).
		Where("id = ? AND pending_analysis_id = ?", sampleID, pendingID).Exec(ctx))
}

// SampleFilter 后台示例检索。
type SampleFilter struct {
	Industry string
	Enabled  *bool
}

// AdminListSamples 后台列表（未删除，sort_order、id 倒序）。
func (r *Repo) AdminListSamples(ctx context.Context, f SampleFilter, offset, limit int) ([]*model.Sample, int64, error) {
	var out []*model.Sample
	q := r.db.NewSelect().Model(&out).Where("sm.deleted = 0")
	if f.Industry != "" {
		q = q.Where("sm.industry = ?", f.Industry)
	}
	if f.Enabled != nil {
		q = q.Where("sm.enabled = ?", *f.Enabled)
	}
	n, err := q.OrderExpr("sm.sort_order, sm.id DESC").Offset(int64(offset)).Limit(int64(limit)).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return out, int64(n), nil
}

// InsertSample 新建示例。
func (r *Repo) InsertSample(ctx context.Context, s *model.Sample) error {
	_, err := r.db.NewInsert().Model(s).Exec(ctx)
	return err
}

// UpdateSample 改示例（未删除）：set 为列 → 值。sourceChanged=true 时一并清掉已发布 / 进行中的分析（换了原片，旧分析不再对应）。
func (r *Repo) UpdateSample(ctx context.Context, id int64, set map[string]any, sourceChanged bool) (bool, error) {
	s := conv(set)
	if sourceChanged {
		s["analysis_id"] = nil
		s["pending_analysis_id"] = nil
		s["analysis_error"] = nil
		s["analyzed_at"] = nil
	}
	return bundb.UpdateIf(ctx, r.db, (*model.Sample)(nil), s, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Where("id = ? AND deleted = 0", id)
	})
}

// DeleteSample 逻辑删除。
func (r *Repo) DeleteSample(ctx context.Context, id, adminID int64) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableSample).Set("deleted = 1").Set("enabled = 0").Set("updated_by = ?", adminID).
		Where("id = ? AND deleted = 0", id).Exec(ctx))
}

// BeginSampleAnalysis 开一轮预分析（同一事务）：锁示例行 → 上一轮还在跑（pending 指向排队 / 进行中的分析）返回 busy →
// 插入系统分析行 → pending_analysis_id = 新行、analysis_round = round、analysis_error = NULL。
// build 按「下一轮轮次」生成分析行（source_key 带轮次）。
func (r *Repo) BeginSampleAnalysis(ctx context.Context, sampleID, adminID int64, build func(round int, sourceRid string) *model.Analysis) (a *model.Analysis, busy bool, err error) {
	err = bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var s model.Sample
		if err := tx.NewSelect().Model(&s).Where("sm.id = ? AND sm.deleted = 0", sampleID).For("UPDATE").Limit(1).Scan(ctx); err != nil {
			return err
		}
		if s.PendingAnalysisID != nil {
			var st int8
			err := tx.NewSelect().Table(model.TableAnalysis).Column("status").Where("id = ?", *s.PendingAnalysisID).Limit(1).Scan(ctx, &st)
			if err == nil && (st == model.AnalysisQueued || st == model.AnalysisRunning) {
				busy = true
				return nil
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		a = build(s.AnalysisRound+1, s.SourceRid)
		if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Table(model.TableSample).Set("pending_analysis_id = ?", a.ID).Set("analysis_round = ?", s.AnalysisRound+1).
			Set("analysis_error = NULL").Set("updated_by = ?", adminID).Where("id = ?", sampleID).Exec(ctx)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil || busy {
		return nil, busy, err
	}
	return a, false, nil
}

// AnalysesByIDs 批量读分析行（后台列表展示分析状态）。
func (r *Repo) AnalysesByIDs(ctx context.Context, ids []int64) (map[int64]*model.Analysis, error) {
	out := map[int64]*model.Analysis{}
	if len(ids) == 0 {
		return out, nil
	}
	var list []*model.Analysis
	if err := r.db.NewSelect().Model(&list).Where("a.id IN (?)", bun.In(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	for _, a := range list {
		out[a.ID] = a
	}
	return out, nil
}

// ---------------------------------------------------------------- 改标注

func (r *Repo) SupersedePlan(ctx context.Context, parentID int64, child *model.Plan) (*model.Plan, bool, error) {
	var got *model.Plan
	created := false
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var parent model.Plan
		if err := tx.NewSelect().Model(&parent).Where("p.id = ?", parentID).For("UPDATE").Limit(1).Scan(ctx); err != nil {
			return err
		}
		if parent.Status == model.PlanStale {
			if parent.SupersededBy != nil {
				var cur model.Plan
				err := tx.NewSelect().Model(&cur).Where("p.id = ?", *parent.SupersededBy).Limit(1).Scan(ctx)
				if err == nil && cur.UserID == child.UserID && cur.PlanHash == child.PlanHash {
					got = &cur // 同一改动的重放
					return nil
				}
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			return core.ErrPlanStale
		}
		if parent.Status != model.PlanReady {
			return core.ErrPlanConfirming // 占用中 / 已确认：不出子方案（否则确认失败放回后新旧两张都可确认）
		}
		if _, err := tx.NewInsert().Model(child).Value("expires_at", string(msExpr(child.ExpiresAt))).Exec(ctx); err != nil {
			if !isDup(err) {
				return err
			}
			var cur model.Plan
			if err := tx.NewSelect().Model(&cur).Where("p.user_id = ? AND p.plan_hash = ?", child.UserID, child.PlanHash).Limit(1).Scan(ctx); err != nil {
				return err
			}
			got = &cur
		} else {
			got, created = child, true
		}
		if parent.Status == model.PlanReady {
			_, err := tx.NewUpdate().Table(model.TablePlan).Set("status = ?", model.PlanStale).Set("superseded_by = ?", got.ID).
				Where("id = ? AND status = ?", parent.ID, model.PlanReady).Exec(ctx)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		// 插入时 expires_at 交给库算，读回真实值
		if cur, err := r.GetPlan(ctx, got.ID); err == nil && cur != nil {
			got = cur
		}
	}
	return got, created, nil
}

func (r *Repo) ChildPlans(ctx context.Context, parentID int64) ([]*model.Plan, error) {
	var out []*model.Plan
	err := r.db.NewSelect().Model(&out).Where("p.parent_plan_id = ?", parentID).OrderExpr("p.id DESC").Limit(50).Scan(ctx)
	return out, err
}

// ---------------------------------------------------------------- 我的方案

// SavePlan 按用户串行：先对 afv_replicate_user_pref 该用户行加排他锁（没有就插入；同一用户的并发保存在此排队），
// 锁到之后再数（REPEATABLE READ 下本事务的一致性快照在第一次普通读时建立，晚于拿到锁，能看到前一个持锁者已提交的行）。
func (r *Repo) SavePlan(ctx context.Context, sp *model.SavedPlan, max int, rename bool) (*model.SavedPlan, bool, error) {
	var got *model.SavedPlan
	created := false
	err := bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		// INSERT … ON DUPLICATE KEY UPDATE 对已存在的行直接加排他锁（INSERT IGNORE 只加共享锁，几个并发事务都拿到共享锁后
		// 再各自要排他锁会互相死锁）；新行则由插入本身持有排他锁。锁一直持有到事务结束：同一用户的保存在这里排队。
		if _, err := tx.ExecContext(ctx, "INSERT INTO `"+model.TablePref+"` (`user_id`, `auto_continue`) VALUES (?, 0) "+
			"ON DUPLICATE KEY UPDATE `user_id` = `user_id`", sp.UserID); err != nil {
			return err
		}
		var cur model.SavedPlan
		err := tx.NewSelect().Model(&cur).Where("sp.user_id = ? AND sp.plan_id = ?", sp.UserID, sp.PlanID).Limit(1).Scan(ctx)
		switch {
		case err == nil:
			if rename && cur.Name != sp.Name {
				if _, err := tx.NewUpdate().Table(model.TableSavedPlan).Set("name = ?", sp.Name).Where("id = ?", cur.ID).Exec(ctx); err != nil {
					return err
				}
				cur.Name = sp.Name
			}
			got = &cur
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		n, err := tx.NewSelect().Table(model.TableSavedPlan).Where("user_id = ?", sp.UserID).Count(ctx)
		if err != nil {
			return err
		}
		if int64(n) >= int64(max) {
			return core.ErrSavedPlanLimit
		}
		if _, err := tx.NewInsert().Model(sp).Exec(ctx); err != nil {
			return err
		}
		got, created = sp, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		if cur, err := r.GetSavedPlan(ctx, sp.UserID, got.ID); err == nil && cur != nil {
			got = cur
		}
	}
	return got, created, nil
}

func (r *Repo) GetSavedPlan(ctx context.Context, userID, id int64) (*model.SavedPlan, error) {
	var sp model.SavedPlan
	return noRows(&sp, r.db.NewSelect().Model(&sp).Where("sp.id = ? AND sp.user_id = ?", id, userID).Limit(1).Scan(ctx))
}

func (r *Repo) ListSavedPlans(ctx context.Context, userID int64, offset, limit int) ([]*model.SavedPlan, int64, error) {
	var out []*model.SavedPlan
	n, err := r.db.NewSelect().Model(&out).Where("sp.user_id = ?", userID).OrderExpr("sp.id DESC").
		Offset(int64(offset)).Limit(int64(limit)).ScanAndCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	return out, int64(n), nil
}

func (r *Repo) DeleteSavedPlan(ctx context.Context, userID, id int64) (bool, error) {
	return one(r.db.NewDelete().TableExpr("`"+model.TableSavedPlan+"`").Where("id = ? AND user_id = ?", id, userID).Exec(ctx))
}

func (r *Repo) TouchSavedPlan(ctx context.Context, userID, id int64) error {
	_, err := r.db.NewUpdate().Table(model.TableSavedPlan).Set("use_count = use_count + 1").Set("last_used_at = NOW(3)").
		Where("id = ? AND user_id = ?", id, userID).Exec(ctx)
	return err
}

func (r *Repo) ExistingAnalyses(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	var got []int64
	if err := r.db.NewSelect().Table(model.TableAnalysis).Column("id").Where("id IN (?)", bun.In(ids)).Scan(ctx, &got); err != nil {
		return nil, err
	}
	for _, id := range got {
		out[id] = true
	}
	return out, nil
}
