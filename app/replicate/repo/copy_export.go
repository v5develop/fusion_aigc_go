package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
)

// 发布文案 / 多比例导出（Go_V1.0.55）：占坑唯一 + 文案每日额度（与定妆图同一套原子更新）。

var (
	_ core.CopyStore   = (*Repo)(nil)
	_ core.ExportStore = (*Repo)(nil)
)

// ---- 发布文案 ----

func (r *Repo) GetCopy(ctx context.Context, jobID int64, platform string) (*model.Copy, error) {
	var c model.Copy
	return noRows(&c, r.db.NewSelect().Model(&c).Where("pc.job_id = ? AND pc.platform = ?", jobID, platform).Limit(1).Scan(ctx))
}

// ClaimCopy 占坑：INSERT 成功即占到；撞唯一键时只有「生成中且超过 staleAfter 没动静」的旧占坑能 CAS 接手。
func (r *Repo) ClaimCopy(ctx context.Context, c *model.Copy, staleAfter time.Duration) (bool, error) {
	c.Status = model.ClaimGenerating
	if _, err := r.db.NewInsert().Model(c).Exec(ctx); err == nil {
		return true, nil
	} else if !isDup(err) {
		return false, err
	}
	cur, err := r.GetCopy(ctx, c.JobID, c.Platform)
	if err != nil || cur == nil {
		return false, err
	}
	ok, err := one(r.db.NewUpdate().Table(model.TableCopy).Set("updated_at = NOW(3)").Set("call_id = ''").
		Where("id = ? AND status = ? AND updated_at < NOW(3) - INTERVAL ? MICROSECOND", cur.ID, model.ClaimGenerating, staleAfter.Microseconds()).Exec(ctx))
	if ok {
		c.ID = cur.ID
	}
	return ok, err
}

func (r *Repo) FinishCopy(ctx context.Context, id int64, content json.RawMessage, callID string) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableCopy).Set("status = ?", model.ClaimDone).Set("content = ?", string(content)).
		Set("call_id = ?", clip(callID, 64)).Where("id = ? AND status = ?", id, model.ClaimGenerating).Exec(ctx))
}

func (r *Repo) DeleteCopyClaim(ctx context.Context, id int64) error {
	_, err := r.db.NewDelete().Table(model.TableCopy).Where("id = ? AND status = ?", id, model.ClaimGenerating).Exec(ctx)
	return err
}

// AcquireCopyQuota 原子占一次额度：先 INSERT IGNORE 建当日行，再「used < total 才 +1」的单条条件更新（并发安全）。
func (r *Repo) AcquireCopyQuota(ctx context.Context, userID int64, day string, total int) (int, bool, error) {
	if _, err := r.db.ExecContext(ctx, "INSERT IGNORE INTO `afv_replicate_copy_quota` (`user_id`, `day`, `used`) VALUES (?, ?, 0)", userID, day); err != nil {
		return 0, false, err
	}
	ok, err := one(r.db.NewUpdate().Table(model.TableCopyQuota).Set("used = used + 1").
		Where("user_id = ? AND day = ? AND used < ?", userID, day, total).Exec(ctx))
	if err != nil {
		return 0, false, err
	}
	used, err := r.CopyQuotaUsed(ctx, userID, day)
	return used, ok, err
}

func (r *Repo) ReleaseCopyQuota(ctx context.Context, userID int64, day string) error {
	_, err := r.db.NewUpdate().Table(model.TableCopyQuota).Set("used = used - 1").Where("user_id = ? AND day = ? AND used > 0", userID, day).Exec(ctx)
	return err
}

func (r *Repo) CopyQuotaUsed(ctx context.Context, userID int64, day string) (int, error) {
	var used int
	err := r.db.NewSelect().Table(model.TableCopyQuota).Column("used").Where("user_id = ? AND day = ?", userID, day).Limit(1).Scan(ctx, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
}

// ---- 多比例导出 ----

func (r *Repo) GetExport(ctx context.Context, jobID int64, ratio, mode, srcRid string) (*model.Export, error) {
	var e model.Export
	return noRows(&e, r.db.NewSelect().Model(&e).
		Where("pe.job_id = ? AND pe.ratio = ? AND pe.mode = ? AND pe.src_rid = ?", jobID, ratio, mode, srcRid).Limit(1).Scan(ctx))
}

func (r *Repo) ListExports(ctx context.Context, jobID int64) ([]*model.Export, error) {
	var out []*model.Export
	err := r.db.NewSelect().Model(&out).Where("pe.job_id = ?", jobID).OrderExpr("pe.id").Scan(ctx)
	return out, err
}

func (r *Repo) ClaimExport(ctx context.Context, e *model.Export, staleAfter time.Duration) (bool, error) {
	e.Status = model.ClaimGenerating
	if _, err := r.db.NewInsert().Model(e).Exec(ctx); err == nil {
		return true, nil
	} else if !isDup(err) {
		return false, err
	}
	cur, err := r.GetExport(ctx, e.JobID, e.Ratio, e.Mode, e.SrcRid)
	if err != nil || cur == nil {
		return false, err
	}
	ok, err := one(r.db.NewUpdate().Table(model.TableExport).Set("updated_at = NOW(3)").
		Where("id = ? AND status = ? AND updated_at < NOW(3) - INTERVAL ? MICROSECOND", cur.ID, model.ClaimGenerating, staleAfter.Microseconds()).Exec(ctx))
	if ok {
		e.ID = cur.ID
	}
	return ok, err
}

func (r *Repo) FinishExport(ctx context.Context, id int64, videoRid string, version int) (bool, error) {
	return one(r.db.NewUpdate().Table(model.TableExport).Set("status = ?", model.ClaimDone).Set("video_rid = ?", videoRid).
		Set("version = ?", version).Where("id = ? AND status = ?", id, model.ClaimGenerating).Exec(ctx))
}

func (r *Repo) DeleteExportClaim(ctx context.Context, id int64) error {
	_, err := r.db.NewDelete().Table(model.TableExport).Where("id = ? AND status = ?", id, model.ClaimGenerating).Exec(ctx)
	return err
}
