package repo

import (
	"context"

	"aigc-local-life/app/canvas/model"

	"github.com/uptrace/bun"
)

// TableRevision 画布快照历史（Go_V1.0.40）。
const TableRevision = "afv_canvas_revision"

// RevisionRepo afv_canvas_revision。
type RevisionRepo struct{ db bun.IDB }

// Insert 记一个 revision 的快照；同 (canvas_id, revision) 已有则忽略（重放 / 重试无害）。
func (r *RevisionRepo) Insert(ctx context.Context, x *model.CanvasRevision) error {
	_, err := r.db.NewInsert().Model(x).Ignore().ExcludeColumn("id", "created_at").Exec(ctx)
	return err
}

// Get 某画布某 revision 的快照（找不到 nil, nil）。归属由调用方先用 afv_canvas 校验。
func (r *RevisionRepo) Get(ctx context.Context, canvasID, revision string) (*model.CanvasRevision, error) {
	x := new(model.CanvasRevision)
	return noRows(x, r.db.NewSelect().Model(x).Where("cr.canvas_id = ?", canvasID).Where("cr.revision = ?", revision).Limit(1).Scan(ctx))
}

// Prune 只留该画布最近 keep 个（按 id，即写入顺序）；返回删掉的行数。keep<=0 不删。
// 子查询包一层派生表：MySQL 不允许 DELETE 的子查询直接引用目标表（1093），带 LIMIT 的派生表会被物化，不受此限。
func (r *RevisionRepo) Prune(ctx context.Context, canvasID string, keep int) (int64, error) {
	if keep <= 0 {
		return 0, nil
	}
	return affected(r.db.NewRaw("DELETE FROM `"+TableRevision+"` WHERE canvas_id = ? AND id < ("+
		"SELECT id FROM (SELECT id FROM `"+TableRevision+"` WHERE canvas_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?) AS keep_floor)",
		canvasID, canvasID, keep-1).Exec(ctx))
}

// Count 该画布保留中的快照数（测试 / 运维用）。
func (r *RevisionRepo) Count(ctx context.Context, canvasID string) (int64, error) {
	return r.db.NewSelect().Table(TableRevision).Where("canvas_id = ?", canvasID).Count(ctx)
}
