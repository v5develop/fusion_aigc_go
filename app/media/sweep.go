package media

import (
	"context"
	"log"
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
				if _, err := s.objects.Delete(ctx, m.Store, m.Path); err != nil {
					rep.Failed++
					continue
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
	return rep, nil
}
