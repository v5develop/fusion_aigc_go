package media

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"
)

// 软删除的清理（设计文档 §8.5）：只物理删除宽限期已过的待删除对象。
// 清理是幂等的：对象已不在时视为完成，标记照常清除；多实例同时跑也不会重复付出代价。

// sweepPageSize 每页列举的标记数。
const sweepPageSize = 200

// sweepMaxPerRun 单轮最多处理的标记数，超出的留给下一轮，避免一次占满。
const sweepMaxPerRun = 5000

// sweepStores 需要清理的存储（三个桶）。
var sweepStores = []string{storePrivate, storeAsset, storeThirdParty}

// sweepReport 一轮清理的结果计数（只记数量，不记路径）。
type sweepReport struct {
	Deleted int
	Pending int // 仍在宽限期内、本轮未处理
	Failed  int
	Orphans int // 对象已不存在、超过保留期而删掉的索引
}

// RunSweeper 按 sweepEvery 周期清理，直到 ctx 结束。
func (s *Server) RunSweeper(ctx context.Context) {
	t := time.NewTicker(s.sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rep, err := s.sweepOnce(ctx)
			if err != nil {
				log.Printf("media: 清理待删除对象失败: %v", err)
				continue
			}
			if rep.Deleted > 0 || rep.Failed > 0 {
				log.Printf("media: 清理待删除对象：删除 %d，失败 %d", rep.Deleted, rep.Failed)
			}
		}
	}
}

// sweepOnce 执行一轮清理：列举每个桶的待删除标记，宽限期已过的先物理删对象，再清标记。
// 先删对象再清标记：中途失败时标记还在，下一轮会重做，不会留下"标记没了、对象还在"的孤儿。
func (s *Server) sweepOnce(ctx context.Context) (sweepReport, error) {
	var rep sweepReport
	handled := 0
	for _, store := range sweepStores {
		marker := ""
		for {
			marks, next, err := s.objects.ListPending(ctx, store, marker, sweepPageSize)
			if err != nil {
				return rep, err
			}
			for _, m := range marks {
				if handled >= sweepMaxPerRun {
					rep.Pending++
					continue
				}
				handled++
				if s.now().Before(m.DeleteAfter) {
					rep.Pending++
					continue
				}
				// 用户输入与历史路径不可能被标记（delete 已拒绝）；这里再拦一次，防止标记被手工写入后误删
				if !deletable(m.Path) {
					rep.Failed++
					continue
				}
				// 列举到删除之间标记可能已被并发的重新入库清掉（ClearPending）或换了到期时间：
				// 物理删除前再读一次标记，确认它仍在且仍已到期，否则跳过，不删刚重新入库的对象
				current, err := s.markerStillDue(ctx, m)
				if err != nil {
					rep.Failed++
					continue
				}
				if !current {
					log.Printf("media: 清理跳过 check=sweep.marker_changed store=%s（列举后标记已清除或未到期）", m.Store)
					continue
				}
				if _, err := s.objects.Delete(ctx, m.Store, m.Path); err != nil {
					rep.Failed++
					continue
				}
				// 对象删除后同步删索引：不留残留。任一步失败标记保留，下一轮重做（两步都幂等）
				if key, ok := objectKeyOfPath(m.Path); ok {
					if err := s.objects.DeleteIndex(ctx, key); err != nil {
						rep.Failed++
						continue
					}
				}
				if err := s.objects.ClearPending(ctx, m.Store, m.Path); err != nil {
					rep.Failed++
					continue
				}
				rep.Deleted++
			}
			if next == "" || handled >= sweepMaxPerRun {
				break
			}
			marker = next
		}
	}
	// 孤儿索引：对象已不存在、超过保留期（直传未完成或入库中途失败留下的），删掉索引，不留残留
	orphans, err := s.sweepOrphanIndexes(ctx, s.now())
	rep.Orphans = orphans
	if err != nil {
		rep.Failed++
		log.Printf("media: 清理孤儿索引失败（下一轮重试）: %v", err)
	}
	return rep, nil
}

// markerStillDue 物理删除前的复核：待删除标记仍然存在且到期时间仍已过去。
func (s *Server) markerStillDue(ctx context.Context, m PendingMark) (bool, error) {
	deadline, ok, err := s.objects.PendingDeadline(ctx, m.Store, m.Path)
	if err != nil || !ok {
		return false, err
	}
	return !s.now().Before(deadline), nil
}

// orphanIndexTTL 索引超过这段时间仍然没有对应对象，即视为孤儿（直传未完成、入库中途失败）。
const orphanIndexTTL = 24 * time.Hour

// sweepOrphanIndexes 删除孤儿索引。返回删除的条数。
func (s *Server) sweepOrphanIndexes(ctx context.Context, now time.Time) (int, error) {
	removed := 0
	marker := ""
	for {
		entries, next, err := s.objects.ListIndexes(ctx, marker, sweepPageSize)
		if err != nil {
			return removed, err
		}
		for _, e := range entries {
			// 修改时间不可知（解析失败）或未到保留期：保留，宁可多留一轮
			if e.Modified.IsZero() || now.Sub(e.Modified) < orphanIndexTTL {
				continue
			}
			orphan, err := s.indexOrphaned(ctx, e.ObjectKey)
			if err != nil {
				return removed, err
			}
			if !orphan {
				continue
			}
			if err := s.objects.DeleteIndex(ctx, e.ObjectKey); err != nil {
				return removed, err
			}
			removed++
		}
		if next == "" {
			return removed, nil
		}
		marker = next
	}
}

// indexOrphaned 索引指向的对象在它可能所在的每个存储里都不存在。索引本身不存在（已被删）视为非孤儿，留给别的路径处理。
func (s *Server) indexOrphaned(ctx context.Context, objectKey string) (bool, error) {
	dir, err := os.MkdirTemp("", "media-orphan-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	local, err := s.objects.Fetch(ctx, storePrivate, indexKeyOf(objectKey), dir)
	if errors.Is(err, errObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(local)
	if err != nil {
		return false, err
	}
	var idx objectIndex
	if err := json.Unmarshal(raw, &idx); err != nil || classOfPath(idx.Path) == "" {
		return false, nil // 内容损坏：保留并由人工排查，不擅自删
	}
	for _, store := range candidateStores(classOfPath(idx.Path)) {
		_, err := s.objects.Head(ctx, store, idx.Path)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, errObjectNotFound) {
			return false, err
		}
	}
	return true, nil
}

// candidateStores 类别的对象可能所在的存储（交付物可能在私有桶或第三方桶）。
func candidateStores(class string) []string {
	switch class {
	case classAsset:
		return []string{storeAsset}
	case classDeliver:
		return []string{storePrivate, storeThirdParty}
	default:
		return []string{storePrivate}
	}
}
