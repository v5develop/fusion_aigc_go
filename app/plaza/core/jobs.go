package core

import (
	"context"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// enabledExpr 与 recomputeEnabled 同一口径的 SQL 表达式（库里的当前时刻）。
const enabledExpr = "IF(deleted = 0 AND state = 'PUBLISHED' AND (online_at IS NULL OR online_at <= NOW()) AND (offline_at IS NULL OR offline_at > NOW()), 1, 0)"

// lifecycleFlipWhere 边界任务只翻这两类行：
//   - 下翻：enabled=1 但此刻已不可见（下线到点 / 被删 / 状态变了）→ 0；
//   - 上翻：enabled=0、此刻应可见，且是定时上线行（online_at IS NOT NULL）→ 1。
//
// 上翻刻意不碰 online_at 为空的行：Go 立即发布时已在同一条 UPDATE 里写了 enabled=1，
// 剩下的「PUBLISHED 但 enabled=0、无定时」只会是 Java 写权属期间被 Java 停用、state 没跟着改的老行，
// 把它们翻成 1 等于替 Java 重新上架。切换写权前用 api-plaza.md「写权切换步骤」里的对账 SQL 把它们改成 OFFLINE。
const lifecycleFlipWhere = "(enabled = 1 AND " + enabledExpr + " = 0) OR (enabled = 0 AND online_at IS NOT NULL AND " + enabledExpr + " = 1)"

// SyncLifecycle 每分钟的边界任务：把刚跨过 online_at / offline_at 的行翻转 enabled（Java 的老 v1 列表与 Agent 快照只认 enabled），
// 翻转了就使目录缓存失效。用户侧读取在查询时就按时间过滤，不依赖这个任务；Java 老读者最多滞后 1 分钟。
// 受写权属开关保护：交接前 Java 是写者，Go 不碰 enabled。翻转范围见 lifecycleFlipWhere。
func (s *Service) SyncLifecycle(ctx context.Context) (int64, error) {
	if s.db == nil || !s.cfg.AdminWriteEnabled {
		return 0, nil
	}
	res, err := s.db.NewUpdate().Model((*model.Template)(nil)).
		Set("enabled = " + enabledExpr).
		Where(lifecycleFlipWhere).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		logx.WithContext(ctx).Infof("plaza: 生命周期边界翻转 %d 个模板（下翻=enabled=1 且已不可见；上翻仅限 online_at 非空的定时上线行）", n)
		s.Invalidate(ctx)
	}
	return n, nil
}

// PurgeExpiredTrash 每日清理：物理删除进回收站超过 30 天的模板（连同关系行、模板级示例素材与头条规则），并写 PURGE 审计。
func (s *Service) PurgeExpiredTrash(ctx context.Context) (int, error) {
	if s.db == nil || !s.cfg.AdminWriteEnabled {
		return 0, nil
	}
	var ids []int64
	if err := s.db.NewSelect().Model((*model.Template)(nil)).Column("id").
		Where("deleted = 1").Where("deleted_at IS NOT NULL").
		Where("deleted_at < DATE_SUB(NOW(), INTERVAL ? DAY)", trashDays).Limit(500).Scan(ctx, &ids); err != nil {
		return 0, err
	}
	sys := Operator{Name: "system"}
	done := 0
	for _, id := range ids {
		err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error { return s.purgeIn(ctx, tx, sys, id, "") })
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza: 清理回收站模板 %d 失败: %v", id, err)
			continue
		}
		done++
	}
	if done > 0 {
		s.Invalidate(ctx)
	}
	return done, nil
}

// tryLock 多副本只让一个跑某个定时任务：有 Redis 用 SET NX；没有（单实例 / 开发）恒为 true。
func (s *Service) tryLock(ctx context.Context, key string, ttl time.Duration) bool {
	if s.rd == nil {
		return true
	}
	ok, err := s.rd.SetNX(ctx, "aigc:plaza:job:"+key, 1, ttl).Result()
	return err == nil && ok
}

// RunBackground 后台任务循环（admin-api 进程里起一个 goroutine）：
//   - 每分钟：生命周期边界翻转
//   - 每 15 秒：视频模板媒体处理（单独的 goroutine，行租约）
//   - 每 10 分钟：搜索缺口增量入库
//   - 每天 04:00（北京时间）：清理回收站、日表保留 90 天、上传废弃对象清理（开关默认关）
//
// 多副本靠 Redis 锁只让一个实例执行；ctx 取消即退出。
func (s *Service) RunBackground(ctx context.Context) {
	go s.RunMediaJobLoop(ctx) // 视频模板抽封面 / 转码：每 15 秒一轮，多副本靠行租约互斥（media_job.go）
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	n := 0
	lastDay := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n++
		if s.tryLock(ctx, "lifecycle", 50*time.Second) {
			if _, err := s.SyncLifecycle(ctx); err != nil {
				logx.WithContext(ctx).Errorf("plaza: 生命周期任务失败: %v", err)
			}
		}
		if n%10 == 0 && s.tryLock(ctx, "miss-flush", 9*time.Minute) {
			if _, err := s.FlushSearchMisses(ctx); err != nil {
				logx.WithContext(ctx).Errorf("plaza: 搜索缺口入库失败: %v", err)
			}
		}
		now := s.now().In(cst)
		if day := now.Format("20060102"); now.Hour() == 4 && day != lastDay && s.tryLock(ctx, "daily-"+day, 23*time.Hour) {
			lastDay = day
			if _, err := s.PurgeExpiredTrash(ctx); err != nil {
				logx.WithContext(ctx).Errorf("plaza: 清理回收站失败: %v", err)
			}
			if err := s.PurgeOldMissDaily(ctx); err != nil {
				logx.WithContext(ctx).Errorf("plaza: 清理搜索缺口日表失败: %v", err)
			}
			// 上传废弃对象清理（Plaza.Upload.Cleanup.Enabled，默认关）：单独 goroutine，不阻塞分钟 tick。
			if s.uploader != nil && s.uploader.cfg.Cleanup.Enabled {
				go func() {
					if _, err := s.CleanupUploads(ctx); err != nil {
						logx.WithContext(ctx).Errorf("plaza: 上传废弃对象清理失败: %v", err)
					}
				}()
			}
		}
	}
}
