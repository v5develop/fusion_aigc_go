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

// Put 写缓存：INSERT … ON DUPLICATE KEY UPDATE payload（命中 uk_batch_key 时覆盖）。
func (r *CacheRepo) Put(ctx context.Context, batchID int64, key, kind string, payload json.RawMessage) error {
	if !json.Valid(payload) {
		return errors.New("repo: 缓存 payload 不是合法 JSON")
	}
	c := &model.PromptCache{BatchID: batchID, CacheKey: key, Kind: kind, Payload: payload}
	return bundb.Upsert(ctx, r.db, c, []string{"batch_id", "cache_key", "kind"}, []string{"payload"})
}
