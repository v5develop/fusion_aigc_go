package core

// 视频模板媒体处理流水线（后台任务）：
//
//	换视频 / 新建视频模板 → media_status=PROCESSING → 本任务认领 → 抽首帧封面、转码（有能力时）→ 写回 poster_url / video_url，置 READY。
//	失败重试 3 次（退避 1 分钟、5 分钟）后置 FAILED 并写 media_error；运营可经「重新处理」重新排队。发布前检查只放行 READY。
//
// 并发：多副本靠「行租约」——认领是一条条件 UPDATE（PROCESSING、租约已过期、到了重试时间、次数没满）写 token + 租约截止，
// 写回时必须仍持有该 token（运营中途换视频 / 重新处理会清 token，旧结果就被丢弃，不会把旧视频的封面写到新视频上）。
// 进程崩溃：租约到期后别的副本重新认领；认领时 attempts+1，所以反复崩溃也会在第 3 次后被 reap 成 FAILED。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	mediaMaxAttempts = 3
	mediaLeaseTTL    = 15 * time.Minute // 大于单次处理超时（14 分钟）
	mediaJobTimeout  = 14 * time.Minute
	mediaBatch       = 5
	mediaErrMax      = 255
)

// mediaBackoff 第 n 次失败后（n 从 1 起）到下次重试的等待。
var mediaBackoff = []time.Duration{time.Minute, 5 * time.Minute}

func mediaBackoffAfter(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(mediaBackoff) {
		attempt = len(mediaBackoff)
	}
	return mediaBackoff[attempt-1]
}

// mediaQueueSet 进入 PROCESSING 并清空流水线状态（新排队 / 重新处理）。
func mediaQueueSet() map[string]any {
	return map[string]any{"media_status": model.MediaStatusProcessing, "media_error": nil, "media_note": nil,
		"media_attempts": 0, "media_next_at": nil, "media_lease_until": nil, "media_lease_token": nil}
}

// mediaReadySet 不需要处理（图片 / 没有视频）：置 READY 并清空流水线状态。
func mediaReadySet() map[string]any {
	s := mediaQueueSet()
	s["media_status"] = model.MediaStatusReady
	return s
}

// mediaChangeSet 编辑后 media_* 列要怎么变（nil = 不变）。参数是编辑后的目标值，cur 是编辑前的行：
//   - 不是视频 / 没有视频地址：不需要处理，非 READY 的复位为 READY；
//   - 视频地址变了（或图片改成视频）：重新排队，旧视频留下的失败 / 备注作废；
//   - 视频没变、封面从空补成了有、且当前 FAILED（运营补了封面）：重新排队，让流水线接着做转码。
func mediaChangeSet(cur *model.Template, mediaType, video, poster string) map[string]any {
	video, poster = strings.TrimSpace(video), strings.TrimSpace(poster)
	if mediaType != model.MediaVideo || video == "" {
		if cur.MediaStatus != model.MediaStatusReady {
			return mediaReadySet()
		}
		return nil
	}
	if video != cur.VideoURL.String || cur.MediaType != model.MediaVideo {
		return mediaQueueSet()
	}
	if cur.MediaStatus == model.MediaStatusFailed && poster != "" && poster != cur.PosterURL.String {
		return mediaQueueSet()
	}
	return nil
}

// AdminReprocessTemplateMedia 重新处理视频：把 FAILED（或已 READY 想重做）的视频模板重新排队。
// 正在处理中的拒绝（避免打断在途任务）。写审计 TEMPLATE_MEDIA_REPROCESS。
func (s *Service) AdminReprocessTemplateMedia(ctx context.Context, op Operator, id int64) (*TemplateWriteResult, error) {
	if err := s.requireWrite(); err != nil {
		return nil, err
	}
	var out *TemplateWriteResult
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		cur, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if cur.MediaType != model.MediaVideo || !cur.VideoURL.Valid || cur.VideoURL.String == "" {
			return errParam("只有带预览视频的视频模板才需要处理")
		}
		if cur.MediaStatus == model.MediaStatusProcessing {
			return errParam("视频正在处理中，请稍后再看结果")
		}
		inds, err := industryIDsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		before := templateSnapshot(*cur, inds)
		if err := applyTemplateUpdate(ctx, tx, id, cur.Revision, mediaQueueSet(), op.ID, s.keepUpdateTime()); err != nil {
			return staleConflict(ctx, err, id, cur.Revision, ReasonConflict, "重新处理视频")
		}
		after, err := lockTemplate(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = &TemplateWriteResult{ID: id, UpdatedAt: after.UpdateTime.Format(timeLayout), Revision: after.Revision}
		return s.audit(ctx, tx, op, "TEMPLATE_MEDIA_REPROCESS", ResTemplate, fmt.Sprint(id), "", before, templateSnapshot(*after, inds))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RunMediaJobLoop 后台循环：每 15 秒扫一轮（多副本靠行租约互斥，不需要 Redis 锁）。受写权属开关保护。
func (s *Service) RunMediaJobLoop(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if _, err := s.RunMediaJobs(ctx); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 视频处理任务本轮失败: %v", err)
		}
	}
}

// RunMediaJobs 一轮：先把「次数已满且租约已过期」的卡死行 reap 成 FAILED，再认领至多 mediaBatch 行逐个处理。返回本轮处理的行数。
func (s *Service) RunMediaJobs(ctx context.Context) (int, error) {
	if s.db == nil || !s.cfg.AdminWriteEnabled {
		return 0, nil
	}
	if err := s.reapMediaJobs(ctx); err != nil {
		return 0, err
	}
	var ids []int64
	if err := s.db.NewSelect().Model((*model.Template)(nil)).Column("id").
		Where("media_status = ?", model.MediaStatusProcessing).Where("media_type = ?", model.MediaVideo).Where("deleted = 0").
		Where("media_attempts < ?", mediaMaxAttempts).
		Where("(media_lease_until IS NULL OR media_lease_until < NOW())").
		Where("(media_next_at IS NULL OR media_next_at <= NOW())").
		Order("id ASC").Limit(mediaBatch).Scan(ctx, &ids); err != nil {
		return 0, err
	}
	done := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if s.runMediaJob(ctx, id) {
			done++
		}
	}
	return done, nil
}

// reapMediaJobs 次数已满、没有活租约、仍是 PROCESSING 的行（进程反复崩溃 / 超时留下的）→ FAILED。
func (s *Service) reapMediaJobs(ctx context.Context) error {
	res, err := s.db.NewUpdate().Model((*model.Template)(nil)).
		Set("media_status = ?", model.MediaStatusFailed).
		Set("media_error = COALESCE(media_error, ?)", "处理多次中断或超时").
		Set("media_lease_until = NULL").Set("media_lease_token = NULL").Set("revision = revision + 1").
		Where("media_status = ?", model.MediaStatusProcessing).Where("media_attempts >= ?", mediaMaxAttempts).
		Where("(media_lease_until IS NULL OR media_lease_until < NOW())").Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logx.WithContext(ctx).Errorf("plaza: 视频处理 %d 行次数已满且无人持有租约，转 FAILED", n)
	}
	return nil
}

func newLeaseToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runMediaJob 认领并处理一行；返回是否真的处理了（认领失败 = 别的副本抢到了，返回 false）。
func (s *Service) runMediaJob(ctx context.Context, id int64) bool {
	token := newLeaseToken()
	res, err := s.db.NewUpdate().Model((*model.Template)(nil)).
		Set("media_lease_token = ?", token).
		Set("media_lease_until = DATE_ADD(NOW(), INTERVAL ? SECOND)", int(mediaLeaseTTL.Seconds())).
		Set("media_attempts = media_attempts + 1").
		Set("update_time = update_time").
		Where("id = ?", id).Where("media_status = ?", model.MediaStatusProcessing).Where("media_type = ?", model.MediaVideo).Where("deleted = 0").
		Where("media_attempts < ?", mediaMaxAttempts).
		Where("(media_lease_until IS NULL OR media_lease_until < NOW())").
		Where("(media_next_at IS NULL OR media_next_at <= NOW())").Exec(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 视频处理认领模板 %d 失败: %v", id, err)
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false
	}
	var t model.Template
	if err := s.db.NewSelect().Model(&t).Where("t.id = ?", id).Scan(ctx); err != nil {
		logx.WithContext(ctx).Errorf("plaza: 视频处理读取模板 %d 失败（租约到期后会被重新认领）: %v", id, err)
		return false
	}
	in := MediaJobInput{TemplateID: id, VideoURL: t.VideoURL.String, PosterURL: t.PosterURL.String}
	pctx, cancel := context.WithTimeout(ctx, mediaJobTimeout)
	defer cancel()
	out, perr := s.safeProcess(pctx, in)
	if perr != nil {
		s.finishMediaFailure(ctx, &t, token, perr)
		return true
	}
	s.finishMediaSuccess(ctx, &t, token, out)
	return true
}

func (s *Service) safeProcess(ctx context.Context, in MediaJobInput) (out *MediaJobResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("处理时发生内部错误: %v", r)
		}
	}()
	return s.processMedia(ctx, in)
}

func clipMediaErr(msg string) string {
	r := []rune(msg)
	if len(r) > mediaErrMax-3 {
		return string(r[:mediaErrMax-3]) + "..."
	}
	return msg
}

// finishMediaSuccess 写回 READY。poster_url 只在当前仍为空时才写（运营中途传了封面就不覆盖）；围栏 = 仍持有租约 token。
func (s *Service) finishMediaSuccess(ctx context.Context, t *model.Template, token string, out *MediaJobResult) {
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		q := tx.NewUpdate().Model((*model.Template)(nil)).
			Set("media_status = ?", model.MediaStatusReady).Set("media_error = NULL").Set("media_note = ?", nullableNote(out.Note)).
			Set("media_next_at = NULL").Set("media_lease_until = NULL").Set("media_lease_token = NULL").
			Set("revision = revision + 1")
		if s.keepUpdateTime() {
			q = q.Set("update_time = update_time")
		}
		if out.PosterURL != "" {
			q = q.Set("poster_url = IF(poster_url IS NULL OR poster_url = '', ?, poster_url)", out.PosterURL)
		}
		if out.VideoURL != "" {
			q = q.Set("video_url = ?", out.VideoURL)
		}
		res, err := q.Where("id = ?", t.ID).Where("media_lease_token = ?", token).Where("media_status = ?", model.MediaStatusProcessing).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			logx.WithContext(ctx).Infof("plaza: 视频处理结果作废 判据=模板 %d 的租约已被清除（处理期间换了视频 / 重新处理）", t.ID)
			return nil
		}
		after := map[string]any{"mediaStatus": model.MediaStatusReady, "mediaNote": out.Note, "posterUrl": out.PosterURL, "videoUrl": out.VideoURL}
		return s.audit(ctx, tx, Operator{Name: "system"}, "TEMPLATE_MEDIA_READY", ResTemplate, fmt.Sprint(t.ID), "",
			map[string]any{"mediaStatus": model.MediaStatusProcessing}, after)
	})
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 视频处理成功但写回模板 %d 失败（租约到期后会重做）: %v", t.ID, err)
		return
	}
	s.Invalidate(ctx)
}

func nullableNote(n string) any {
	if strings.TrimSpace(n) == "" {
		return nil
	}
	return clipMediaErr(n)
}

// finishMediaFailure 永久失败或次数已满 → FAILED（写 media_error + 审计）；否则退避后留在 PROCESSING 等下一轮。
func (s *Service) finishMediaFailure(ctx context.Context, t *model.Template, token string, cause error) {
	attempt := t.MediaAttempts // 认领时已 +1，t 是认领之后读的，这里就是本次是第几次
	msg := clipMediaErr(cause.Error())
	final := isPermanent(cause) || attempt >= mediaMaxAttempts
	logx.WithContext(ctx).Errorf("plaza: 视频处理失败 模板=%d 第 %d/%d 次 永久=%v 判据=%v", t.ID, attempt, mediaMaxAttempts, isPermanent(cause), cause)
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		q := tx.NewUpdate().Model((*model.Template)(nil)).
			Set("media_error = ?", msg).Set("media_lease_until = NULL").Set("media_lease_token = NULL")
		if final {
			q = q.Set("media_status = ?", model.MediaStatusFailed).Set("media_next_at = NULL").Set("revision = revision + 1")
			if s.keepUpdateTime() {
				q = q.Set("update_time = update_time")
			}
		} else {
			q = q.Set("media_next_at = DATE_ADD(NOW(), INTERVAL ? SECOND)", int(mediaBackoffAfter(attempt).Seconds())).Set("update_time = update_time")
		}
		res, err := q.Where("id = ?", t.ID).Where("media_lease_token = ?", token).Where("media_status = ?", model.MediaStatusProcessing).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 || !final {
			return nil
		}
		return s.audit(ctx, tx, Operator{Name: "system"}, "TEMPLATE_MEDIA_FAILED", ResTemplate, fmt.Sprint(t.ID), "",
			map[string]any{"mediaStatus": model.MediaStatusProcessing}, map[string]any{"mediaStatus": model.MediaStatusFailed, "mediaError": msg, "attempts": attempt})
	})
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 视频处理失败状态写回模板 %d 失败（租约到期后会重试）: %v", t.ID, err)
		return
	}
	if final {
		s.Invalidate(ctx)
	}
}
