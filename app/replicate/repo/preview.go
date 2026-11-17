package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
)

// 定妆图（Go_V1.0.52）：一方案一张 + 每人每天额度。

var _ core.PreviewStore = (*Repo)(nil)

func (r *Repo) GetPreview(ctx context.Context, userID int64, planHash string) (*model.Preview, error) {
	var p model.Preview
	return noRows(&p, r.db.NewSelect().Model(&p).Where("pv.user_id = ? AND pv.plan_hash = ?", userID, planHash).Limit(1).Scan(ctx))
}

// ClaimPreview 占坑：INSERT 成功即占到；撞唯一键时，只有「出图中且超过 staleAfter 没动静」的旧占坑才能接手（CAS 刷新 updated_at）。
func (r *Repo) ClaimPreview(ctx context.Context, p *model.Preview, staleAfter time.Duration) (bool, error) {
	p.Status = model.PreviewGenerating
	if _, err := r.db.NewInsert().Model(p).Exec(ctx); err == nil {
		return true, nil
	} else if !isDup(err) {
		return false, err
	}
	var cur model.Preview
	got, err := noRows(&cur, r.db.NewSelect().Model(&cur).Where("pv.user_id = ? AND pv.plan_hash = ?", p.UserID, p.PlanHash).Limit(1).Scan(ctx))
	if err != nil || got == nil {
		return false, err
	}
	ok, err := one(r.db.NewUpdate().Table(model.TablePreview).Set("updated_at = NOW(3)").Set("call_id = ''").
		Where("id = ? AND status = ? AND updated_at < NOW(3) - INTERVAL ? MICROSECOND", got.ID, model.PreviewGenerating, staleAfter.Microseconds()).Exec(ctx))
	if ok {
		p.ID = got.ID
	}
	return ok, err
}

func (r *Repo) FinishPreview(ctx context.Context, id int64, imageRid, callID string) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TablePreview).Set("status = ?", model.PreviewDone).Set("image_rid = ?", imageRid).
		Set("call_id = ?", clip(callID, 64)).Where("id = ? AND status = ?", id, model.PreviewGenerating).Exec(ctx))
}

func (r *Repo) DeletePreviewClaim(ctx context.Context, id int64) error {
	_, err := r.db.NewDelete().Table(model.TablePreview).Where("id = ? AND status = ?", id, model.PreviewGenerating).Exec(ctx)
	return err
}

// AcquirePreviewQuota 原子占一次额度：先 INSERT IGNORE 建当日行，再「used < total 才 +1」的条件更新（单条语句，并发安全）。
func (r *Repo) AcquirePreviewQuota(ctx context.Context, userID int64, day string, total int) (int, bool, error) {
	if _, err := r.db.ExecContext(ctx, "INSERT IGNORE INTO `afv_replicate_preview_quota` (`user_id`, `day`, `used`) VALUES (?, ?, 0)", userID, day); err != nil {
		return 0, false, err
	}
	ok, err := one(r.db.NewUpdate().Table(model.TablePreviewQuota).Set("used = used + 1").
		Where("user_id = ? AND day = ? AND used < ?", userID, day, total).Exec(ctx))
	if err != nil {
		return 0, false, err
	}
	used, err := r.PreviewQuotaUsed(ctx, userID, day)
	return used, ok, err
}

func (r *Repo) ReleasePreviewQuota(ctx context.Context, userID int64, day string) error {
	_, err := r.db.NewUpdate().Table(model.TablePreviewQuota).Set("used = used - 1").Where("user_id = ? AND day = ? AND used > 0", userID, day).Exec(ctx)
	return err
}

func (r *Repo) PreviewQuotaUsed(ctx context.Context, userID int64, day string) (int, error) {
	var used int
	err := r.db.NewSelect().Table(model.TablePreviewQuota).Column("used").Where("user_id = ? AND day = ?", userID, day).Limit(1).Scan(ctx, &used)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return used, nil
}
