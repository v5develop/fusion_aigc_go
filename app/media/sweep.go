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

// 单轮预算：标记按类别分前缀列举，各自计数，互不挤占（设计文档 §8.5）。
// sweepMaxPerRun 是 work、deliver、asset 三类合用的预算；用户输入的保留期标记另有 sweepMaxUserPerRun。
// 一轮最多处理 sweepMaxPerRun+sweepMaxUserPerRun 条，超出的留给下一轮。用户输入的积压（含不可删的标记）
// 只消耗它自己的预算，不会把工作产物、素材库与第三方交付物的清理饿死。
const (
	sweepMaxPerRun     = 5000
	sweepMaxUserPerRun = 1000
)

// sweepDeletableClasses 通用清理的类别（按此顺序处理）；classUser 单独一批，见 sweepOnce。
var sweepDeletableClasses = []string{classWork, classDeliver, classAsset}

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

// sweepOnce 执行一轮清理：按存储、按类别列举待删除标记，宽限期已过的先物理删对象，再清标记。
// 先删对象再清标记：中途失败时标记还在，下一轮会重做，不会留下"标记没了、对象还在"的孤儿。
// 先处理 work、deliver、asset（共用 sweepMaxPerRun），再单独处理 user 类别（sweepMaxUserPerRun）。
func (s *Server) sweepOnce(ctx context.Context) (sweepReport, error) {
	var rep sweepReport
	protected := 0
	budget := sweepMaxPerRun
	for _, store := range sweepStores {
		for _, class := range sweepDeletableClasses {
			if err := s.sweepClass(ctx, store, class, &budget, &rep, &protected); err != nil {
				return rep, err
			}
		}
	}
	userBudget := sweepMaxUserPerRun
	for _, store := range sweepStores {
		if err := s.sweepClass(ctx, store, classUser, &userBudget, &rep, &protected); err != nil {
			return rep, err
		}
	}
	if protected > 0 {
		log.Printf("media: 清理跳过用户输入 check=sweep.user_marker_not_retention count=%d（标记原因不是 retention，用户输入不物理删除）", protected)
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

// sweepClass 清理 store 里 class 类别的待删除标记，每条列举到的标记消耗一份 budget；budget 用完即停。
// protected 累计因原因不是 retention 而拒删的用户输入标记数。
func (s *Server) sweepClass(ctx context.Context, store, class string, budget *int, rep *sweepReport, protected *int) error {
	marker := ""
	for *budget > 0 {
		marks, next, err := s.objects.ListPending(ctx, store, class, marker, sweepPageSize)
		if err != nil {
			return err
		}
		for _, m := range marks {
			if *budget <= 0 {
				rep.Pending++
				continue
			}
			*budget--
			if s.now().Before(m.DeleteAfter) {
				rep.Pending++
				continue
			}
			// 只有 work、deliver、asset 与"原因为 retention 的 user"可以物理删；手工写入或别的路径写入的
			// user 标记、历史路径一律不删（delete 已拒绝，这里再拦一次）
			if !sweepEligible(m) {
				if classOfPath(m.Path) == classUser {
					*protected++
				}
				rep.Failed++
				continue
			}
			// 列举到删除之间标记可能已被并发的重新入库清掉（ClearPending）、换了到期时间或原因：
			// 物理删除前再读一次标记，确认它仍在、仍已到期、仍可删，否则跳过，不删刚重新入库的对象
			current, err := s.markerStillDue(ctx, m)
			if err != nil {
				rep.Failed++
				continue
			}
			if !current {
				log.Printf("media: 清理跳过 check=sweep.marker_changed store=%s（列举后标记已清除、未到期或原因已变）", m.Store)
				continue
			}
			if _, err := s.physicalDelete(ctx, m); err != nil {
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
		if next == "" {
			return nil
		}
		marker = next
	}
	return nil
}

// sweepEligible 标记对应的对象能否物理删除：work、deliver、asset 可以；user 只在标记原因为 retention 时可以。
func sweepEligible(m PendingMark) bool {
	if deletable(m.Path) {
		return true
	}
	return classOfPath(m.Path) == classUser && m.Reason == pendingReasonRetention
}

// physicalDelete 按类别选物理删除的入口：user 类别走只收保留期到期对象的 DeleteRetentionExpired，其余走 Delete。
func (s *Server) physicalDelete(ctx context.Context, m PendingMark) (bool, error) {
	if classOfPath(m.Path) == classUser {
		return s.objects.DeleteRetentionExpired(ctx, m.Store, m.Path)
	}
	return s.objects.Delete(ctx, m.Store, m.Path)
}

// markerStillDue 物理删除前的复核：待删除标记仍然存在、到期时间仍已过去、且按复读到的原因仍可物理删除。
func (s *Server) markerStillDue(ctx context.Context, m PendingMark) (bool, error) {
	current, ok, err := s.objects.PendingMarker(ctx, m.Store, m.Path)
	if err != nil || !ok {
		return false, err
	}
	current.Path = m.Path
	return !s.now().Before(current.DeleteAfter) && sweepEligible(current), nil
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
