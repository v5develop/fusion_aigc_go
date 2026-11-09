package repo

import (
	"context"
	"encoding/json"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
)

// MaskRepo afv_studio_mask_cache（Go_V1.0.28）：区域蒙版缓存，唯一键 uk_mask_url_target_parts(url_key, target, parts)。
type MaskRepo struct{ db bun.IDB }

// Get 按（规范化地址 sha256, 区域, 部位）取缓存；parts 空 = 不细分。没有返回 (nil, nil)。
func (r *MaskRepo) Get(ctx context.Context, urlKey, target, parts string) (*model.MaskCache, error) {
	c := new(model.MaskCache)
	return noRows(c, r.db.NewSelect().Model(c).
		Where("mc.url_key = ?", urlKey).Where("mc.target = ?", target).Where("mc.parts = ?", parts).
		Limit(1).Scan(ctx))
}

// Put 写缓存（命中唯一键时覆盖结果列，首个请求用户与命中次数保留）。
func (r *MaskRepo) Put(ctx context.Context, c *model.MaskCache) error {
	return bundb.Upsert(ctx, r.db, c, []string{"url_key", "target", "parts"}, []string{
		"source_url", "status", "mask_rid", "mask_url", "part_masks", "coverage", "width", "height",
		"model_group", "gateway_call_id", "cost_cny",
	})
}

// Hit 命中次数 +1（只用于用量统计，失败不影响返回）。
func (r *MaskRepo) Hit(ctx context.Context, id int64) error {
	_, err := r.db.NewUpdate().Table(TableMask).Set("hits = hits + 1").Where("id = ?", id).Exec(ctx)
	return err
}

// SetMask 写子任务的服装蒙版地址（及逐件蒙版 JSON，nil = 没有）：只写成功、未删除、还没有蒙版的那一条；返回是否命中。
func (r *TaskRepo) SetMask(ctx context.Context, id int64, maskURL string, parts json.RawMessage) (bool, error) {
	q := r.db.NewUpdate().Table(TableTask).Set("mask_url = ?", maskURL)
	if len(parts) > 0 {
		q = q.Set("mask_parts = ?", string(parts))
	}
	n, err := affected(q.
		Where("id = ?", id).Where("deleted = 0").Where("status = ?", model.TaskSucceeded).
		Where("mask_url IS NULL").Exec(ctx))
	return n > 0, err
}
