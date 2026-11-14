package core

// 后台上传的废弃对象清理。
//
// 对象来源（都在公有桶 <Prefix>/plaza-template/ 下）：
//   - 登记对象：afv_plaza_upload_hash 登记的首次上传对象。超过宽限期仍没有被任何模板 / 示例素材引用 → 废弃。
//   - 孤儿对象：confirm 拒收、去重命中的重复对象，入 afv_plaza_upload_orphan 队列；过了宽限期再删。
//
// 删除的安全边界（缺一不删，任何一项判不出来都视为「不能删」）：
//  1. 总开关 Plaza.Upload.Cleanup.Enabled 默认 false；关闭时本任务整个不跑，confirm 路径也不删对象（只入队）。
//  2. key 必须整体匹配 <Prefix>/plaza-template/<yyyymm>/<32位hex>.<ext>（与 confirm 同一条正则），否则跳过；
//     存储层 Delete 另有 Prefix 边界。
//  3. 宽限期：登记对象取 GREATEST(create_time, last_hit_time)（去重命中会刷新），孤儿取入队时间。
//  4. 引用检查：按 key 在 afv_compat_plaza_template（video_url / poster_url / data_json / slots_json /
//     channels_json / tags_json / description，含回收站与下线模板，不看 deleted / state）和 afv_plaza_sample_asset
//     （url / thumb_url，含已删行）里做子串匹配；用 key 而不是整条 URL，换域名 / 换 CDN 也认得出来。
//     命中 = 被引用 = 不删。引用检查本身出错 = 终止本轮。
//  5. 孤儿对象若同时是登记对象（例如先被拒收、客户端重试后 confirm 成功），不删，只清队列行。
//  6. 孤儿：先删对象，成功后才删队列行。登记对象：同一事务里先条件删登记行（命中时间仍早于阈值才删，影响 0 行 = 刚被去重命中，
//     跳过、不删对象），再删对象，删对象失败整笔回滚；删对象失败都保留行，下轮重试。删除幂等（对象已不在也算成功）。
//  7. 分批（BatchSize）、每轮最多删 MaxDeletesPerRun 个；多副本靠 Redis 锁只跑一份。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	orphanRejected  = "REJECTED"
	orphanDuplicate = "DUPLICATE"
)

// CleanupConfig 废弃对象清理配置（Plaza.Upload.Cleanup）。
type CleanupConfig struct {
	// Enabled 总开关，默认关闭，需要显式开启。
	Enabled bool `json:",default=false"`
	// HashGraceDays 登记对象超过多少天没被引用才算废弃（默认 7）。
	HashGraceDays int `json:",default=7"`
	// OrphanGraceHours 孤儿对象入队后多少小时才删（默认 24）。
	OrphanGraceHours int `json:",default=24"`
	// BatchSize 每批扫描行数（默认 100）；MaxDeletesPerRun 每轮最多删除对象数（默认 1000）。
	BatchSize        int `json:",default=100"`
	MaxDeletesPerRun int `json:",default=1000"`
}

func (c CleanupConfig) hashGraceDays() int    { return orDefault(c.HashGraceDays, 7) }
func (c CleanupConfig) orphanGraceHours() int { return orDefault(c.OrphanGraceHours, 24) }
func (c CleanupConfig) batchSize() int        { return orDefault(c.BatchSize, 100) }
func (c CleanupConfig) maxDeletes() int       { return orDefault(c.MaxDeletesPerRun, 1000) }

// CleanupReport 一轮清理的结果。
type CleanupReport struct {
	OrphansDeleted, HashDeleted, CoversDeleted int
	SkippedReferenced, SkippedInvalid          int
	Failed                                     int
}

// orphanSink 孤儿对象待清理队列。
type orphanSink interface {
	Enqueue(ctx context.Context, key, url, reason string) error
}

type sqlOrphans struct{ db *bun.DB }

func (o *sqlOrphans) Enqueue(ctx context.Context, key, url, reason string) error {
	_, err := o.db.ExecContext(ctx, "INSERT IGNORE INTO afv_plaza_upload_orphan (object_key, url, reason) VALUES (?, ?, ?)", key, url, reason)
	return err
}

// enqueueOrphan 入队；失败只记日志（对象仍在桶里，下次 confirm / 运维可再处理），不影响 confirm 结果。
func (u *Uploader) enqueueOrphan(ctx context.Context, key, rawURL, reason string) {
	if u.orphans == nil {
		return
	}
	if err := u.orphans.Enqueue(ctx, key, rawURL, reason); err != nil {
		logx.WithContext(ctx).Errorf("plaza: 孤儿对象入待清理队列失败 key=%s reason=%s: %v", key, reason, err)
	}
}

// deletable key 是否落在允许清理的目录与命名规则里。
func (u *Uploader) deletable(key string) bool {
	return strings.HasPrefix(key, u.prefix+"/"+UploadDir+"/") && u.keyRe.MatchString(key)
}

// keyReferenced key 是否被模板 / 示例素材引用（含回收站、下线、已删行，宁可多留）。
func (s *Service) keyReferenced(ctx context.Context, key string) (bool, error) {
	const q = `SELECT (EXISTS (SELECT 1 FROM afv_compat_plaza_template WHERE
		INSTR(COALESCE(video_url, ''), ?) > 0 OR INSTR(COALESCE(poster_url, ''), ?) > 0 OR
		INSTR(COALESCE(CAST(data_json AS CHAR), ''), ?) > 0 OR INSTR(COALESCE(CAST(slots_json AS CHAR), ''), ?) > 0 OR
		INSTR(COALESCE(CAST(channels_json AS CHAR), ''), ?) > 0 OR INSTR(COALESCE(CAST(tags_json AS CHAR), ''), ?) > 0 OR
		INSTR(COALESCE(description, ''), ?) > 0)
	OR EXISTS (SELECT 1 FROM afv_plaza_sample_asset WHERE INSTR(url, ?) > 0 OR INSTR(COALESCE(thumb_url, ''), ?) > 0)) AS ref`
	args := make([]any, 9)
	for i := range args {
		args[i] = key
	}
	var n int
	if err := s.db.NewRaw(q, args...).Scan(ctx, &n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// keyRegistered key 是否出现在上传登记表里（object_key / url / result_json 里的封面地址）。
func (s *Service) keyRegistered(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.NewRaw("SELECT EXISTS (SELECT 1 FROM afv_plaza_upload_hash WHERE object_key = ? OR INSTR(url, ?) > 0 OR INSTR(CAST(result_json AS CHAR), ?) > 0)", key, key, key).Scan(ctx, &n)
	return n > 0, err
}

// CleanupUploads 一轮清理。开关关闭 / 没装配上传存储时什么也不做。
func (s *Service) CleanupUploads(ctx context.Context) (CleanupReport, error) {
	var rep CleanupReport
	u := s.uploader
	if s.db == nil || u == nil || !u.cfg.Cleanup.Enabled {
		return rep, nil
	}
	lg := logx.WithContext(ctx)
	budget := u.cfg.Cleanup.maxDeletes()
	if err := s.cleanupOrphans(ctx, u, &rep, &budget); err != nil {
		lg.Errorf("plaza: 上传清理 孤儿阶段中止: %v", err)
		return rep, err
	}
	if err := s.cleanupRegistered(ctx, u, &rep, &budget); err != nil {
		lg.Errorf("plaza: 上传清理 登记阶段中止: %v", err)
		return rep, err
	}
	lg.Infof("plaza: 上传清理完成 孤儿删除=%d 登记删除=%d 封面删除=%d 跳过(被引用/已登记)=%d 跳过(key 不合规)=%d 删除失败=%d",
		rep.OrphansDeleted, rep.HashDeleted, rep.CoversDeleted, rep.SkippedReferenced, rep.SkippedInvalid, rep.Failed)
	return rep, nil
}

type orphanRow struct {
	ID  int64  `bun:"id"`
	Key string `bun:"object_key"`
	URL string `bun:"url"`
}

func (s *Service) cleanupOrphans(ctx context.Context, u *Uploader, rep *CleanupReport, budget *int) error {
	lg := logx.WithContext(ctx)
	var last int64
	for *budget > 0 {
		var rows []orphanRow
		err := s.db.NewRaw("SELECT id, object_key, url FROM afv_plaza_upload_orphan WHERE id > ? AND create_time < DATE_SUB(NOW(), INTERVAL ? HOUR) ORDER BY id LIMIT ?",
			last, u.cfg.Cleanup.orphanGraceHours(), u.cfg.Cleanup.batchSize()).Scan(ctx, &rows)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, r := range rows {
			last = r.ID
			if *budget <= 0 {
				return nil
			}
			dropRow := func() {
				if _, err := s.db.ExecContext(ctx, "DELETE FROM afv_plaza_upload_orphan WHERE id = ?", r.ID); err != nil {
					lg.Errorf("plaza: 清理队列删行失败 id=%d key=%s: %v", r.ID, r.Key, err)
				}
			}
			if !u.deletable(r.Key) {
				lg.Errorf("plaza: 上传清理跳过 判据=key 不在 %s/%s/ 或命名不合规，不删对象，仅移出队列 key=%s", u.prefix, UploadDir, r.Key)
				rep.SkippedInvalid++
				dropRow()
				continue
			}
			if reg, err := s.keyRegistered(ctx, r.Key); err != nil {
				return err
			} else if reg {
				lg.Infof("plaza: 上传清理跳过 判据=孤儿对象同时是登记对象（拒收后重试 confirm 成功），不删，仅移出队列 key=%s", r.Key)
				rep.SkippedReferenced++
				dropRow()
				continue
			}
			if ref, err := s.keyReferenced(ctx, r.Key); err != nil {
				return err
			} else if ref {
				lg.Infof("plaza: 上传清理跳过 判据=孤儿对象被模板 / 示例素材引用，不删，仅移出队列 key=%s", r.Key)
				rep.SkippedReferenced++
				dropRow()
				continue
			}
			if err := u.store.Delete(ctx, r.Key); err != nil {
				rep.Failed++
				lg.Errorf("plaza: 上传清理 删除孤儿对象失败（保留队列行，下轮重试）key=%s: %v", r.Key, err)
				_, _ = s.db.ExecContext(ctx, "UPDATE afv_plaza_upload_orphan SET attempts = attempts + 1, last_error = ? WHERE id = ?", truncStr(err.Error(), 250), r.ID)
				continue
			}
			lg.Infof("plaza: 上传清理已删除孤儿对象 key=%s", r.Key)
			rep.OrphansDeleted++
			*budget--
			dropRow()
		}
	}
	return nil
}

type hashRow struct {
	ID         int64  `bun:"id"`
	SHA        string `bun:"sha256"`
	Key        string `bun:"object_key"`
	URL        string `bun:"url"`
	ResultJSON string `bun:"result_json"`
}

func (s *Service) cleanupRegistered(ctx context.Context, u *Uploader, rep *CleanupReport, budget *int) error {
	lg := logx.WithContext(ctx)
	var last int64
	for *budget > 0 {
		var rows []hashRow
		err := s.db.NewRaw(`SELECT id, sha256, object_key, url, CAST(result_json AS CHAR) AS result_json FROM afv_plaza_upload_hash
			WHERE id > ? AND GREATEST(create_time, COALESCE(last_hit_time, create_time)) < DATE_SUB(NOW(), INTERVAL ? DAY)
			ORDER BY id LIMIT ?`, last, u.cfg.Cleanup.hashGraceDays(), u.cfg.Cleanup.batchSize()).Scan(ctx, &rows)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, r := range rows {
			last = r.ID
			if *budget <= 0 {
				return nil
			}
			if !u.deletable(r.Key) || !strings.Contains(r.URL, r.Key) {
				lg.Errorf("plaza: 上传清理跳过 判据=登记行 key 不合规或与 url 不一致，不删 id=%d key=%s url=%s", r.ID, r.Key, r.URL)
				rep.SkippedInvalid++
				continue
			}
			ref, err := s.keyReferenced(ctx, r.Key)
			if err != nil {
				return err
			}
			if ref {
				rep.SkippedReferenced++
				continue
			}
			// 与去重命中竞态：扫描之后可能有同内容上传刚命中它（Touch 刷新了命中时间）。所以在同一事务里先按
			// 「命中时间仍早于阈值」条件删登记行（持行锁，并发 Touch 会等本事务结束），影响 0 行 = 刚被命中 → 跳过、不删对象；
			// 删到了再删对象，删对象失败整笔回滚、登记行保留，下轮重试。
			claimed, err := s.deleteStaleHash(ctx, u, r)
			if err != nil {
				rep.Failed++
				lg.Errorf("plaza: 上传清理 删除登记对象失败（保留登记行，下轮重试）key=%s: %v", r.Key, err)
				continue
			}
			if !claimed {
				lg.Infof("plaza: 上传清理跳过 判据=登记行刚被去重命中（命中时间已不早于 %d 天阈值）或已不在，不删对象 key=%s", u.cfg.Cleanup.hashGraceDays(), r.Key)
				rep.SkippedReferenced++
				continue
			}
			lg.Infof("plaza: 上传清理已删除登记对象 key=%s sha256=%s 判据=超过 %d 天未被引用", r.Key, r.SHA, u.cfg.Cleanup.hashGraceDays())
			rep.HashDeleted++
			*budget--
			s.cleanupCover(ctx, u, r, rep)
		}
	}
	return nil
}

// errStaleHashGone 条件删登记行影响 0 行（刚被去重命中 / 已不在）：回滚事务、不删对象。
var errStaleHashGone = errors.New("登记行已不满足清理条件")

// deleteStaleHash 条件删登记行 + 删对象，同一事务：claimed=false 表示行已不满足「命中时间早于阈值」（跳过、对象不动）；
// 删对象失败返回错误并回滚（登记行保留）。
func (s *Service) deleteStaleHash(ctx context.Context, u *Uploader, r hashRow) (bool, error) {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM afv_plaza_upload_hash WHERE id = ? AND sha256 = ?
			AND GREATEST(create_time, COALESCE(last_hit_time, create_time)) < DATE_SUB(NOW(), INTERVAL ? DAY)`,
			r.ID, r.SHA, u.cfg.Cleanup.hashGraceDays())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errStaleHashGone
		}
		return u.store.Delete(ctx, r.Key)
	})
	if errors.Is(err, errStaleHashGone) {
		return false, nil
	}
	return err == nil, err
}

// cleanupCover 登记结果里的视频封面（也在 plaza-template/ 下）：同样的判据，失败只记日志。
func (s *Service) cleanupCover(ctx context.Context, u *Uploader, r hashRow, rep *CleanupReport) {
	lg := logx.WithContext(ctx)
	var res ConfirmResult
	if json.Unmarshal([]byte(r.ResultJSON), &res) != nil || res.CoverURL == "" || res.CoverURL == r.URL {
		return
	}
	ckey, err := u.checkOwnedURL(res.CoverURL)
	if err != nil || !u.deletable(ckey) || ckey == r.Key {
		return
	}
	if ref, err := s.keyReferenced(ctx, ckey); err != nil || ref {
		return
	}
	if reg, err := s.keyRegistered(ctx, ckey); err != nil || reg {
		return
	}
	if err := u.store.Delete(ctx, ckey); err != nil {
		lg.Errorf("plaza: 上传清理 删除封面对象失败 key=%s: %v", ckey, err)
		return
	}
	rep.CoversDeleted++
	lg.Infof("plaza: 上传清理已删除封面对象 key=%s 随登记对象 %s", ckey, r.Key)
}

func truncStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
