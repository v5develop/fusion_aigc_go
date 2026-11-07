package repo

import (
	"context"
	"encoding/json"
	"errors"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
)

// CacheRepo afv_studio_prompt_cache：批次内反推 / 切图缓存，唯一键 uk_batch_key(batch_id, cache_key, kind)。
type CacheRepo struct{ db bun.IDB }

// Get 取缓存；没有返回 (nil, false, nil)。kind 见 model.Cache*。
func (r *CacheRepo) Get(ctx context.Context, batchID int64, key, kind string) (json.RawMessage, bool, error) {
	c := new(model.PromptCache)
	got, err := noRows(c, r.db.NewSelect().Model(c).
		Where("pc.batch_id = ?", batchID).Where("pc.cache_key = ?", key).Where("pc.kind = ?", kind).
		Limit(1).Scan(ctx))
	if err != nil || got == nil {
		return nil, false, err
	}
	return got.Payload, true, nil
}

// Put 写缓存：INSERT … ON DUPLICATE KEY UPDATE payload, created_at（命中 uk_batch_key 时覆盖，并把 created_at 刷成现在，
// 这样按 created_at 清理的 DeleteStalePreview 不会误删刚被覆盖写的行）。
func (r *CacheRepo) Put(ctx context.Context, batchID int64, key, kind string, payload json.RawMessage) error {
	if !json.Valid(payload) {
		return errors.New("repo: 缓存 payload 不是合法 JSON")
	}
	c := &model.PromptCache{BatchID: batchID, CacheKey: key, Kind: kind, Payload: payload}
	return bundb.Upsert(ctx, r.db, c, []string{"batch_id", "cache_key", "kind"}, []string{"payload", "created_at"})
}

// DeleteStalePreview 删除配对预览缓存（batch_id < 0，见 logic.previewSourceBatchID）里 created_at 早于 olderThanSec
// （距今秒数，换算交给库）的行，最多 limit 行，返回实际删除行数。正数 batch_id（真实批次缓存）永远不会被碰到。
//
// 索引：uk_batch_key(batch_id, cache_key, kind) 以 batch_id 打头，`batch_id < 0` 是范围扫描，只扫预览行、不会碰真实批次行；
// created_at 无索引，在这段范围内逐行过滤（预览行数 = 用户数 × 不同原图数，量级可控）。分批 + LIMIT 控制单条锁与回滚段。
// 多实例并发安全：DELETE 是幂等的，两个实例删同一批行时后者影响行数少而已；不需要租约。
func (r *CacheRepo) DeleteStalePreview(ctx context.Context, olderThanSec int64, limit int) (int64, error) {
	if limit <= 0 || olderThanSec <= 0 {
		return 0, errors.New("repo: DeleteStalePreview 需要正的 olderThanSec 与 limit")
	}
	res, err := r.db.NewDelete().ModelTableExpr(TableCache).
		Where("batch_id < 0").
		Where("created_at < DATE_SUB(NOW(3), INTERVAL ? SECOND)", olderThanSec).
		Limit(int64(limit)).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
