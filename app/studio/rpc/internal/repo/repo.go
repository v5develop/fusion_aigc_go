// Package repo studio 三张表（afv_studio_batch / afv_studio_task / afv_studio_prompt_cache）的 bun 数据访问。
//
// 约定：
//   - 全部方法带 ctx；查单条找不到返回 (nil, nil)，不返回 sql.ErrNoRows。
//   - 状态迁移一律 CAS（bundb.TransitStatus：WHERE id=? AND status=?），返回 ok=false 表示没抢到（状态已变或记录不存在）。
//   - 时间条件用 FROM_UNIXTIME(ms/1000) 交给库按会话时区换算；不要把 time.Time 直接当参数（见 model 包注释）。
//   - 事务：Repo.RunInTx 给回调一个绑在事务上的 *Repo，回调里用它读写即可。
//   - 只写 afv_studio_*；计费 / 任务中心 / 素材库仍走 pkg/billing、pkg/taskcenter、pkg/asset。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/lease"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// 表名（TransitStatus 按表名更新）。
const (
	TableBatch = "afv_studio_batch"
	TableTask  = "afv_studio_task"
	TableCache = "afv_studio_prompt_cache"
	TableMask  = "afv_studio_mask_cache"
)

// Repo 数据访问入口。Batch / Task / Cache 共用同一个句柄（*bun.DB 或 bun.Tx）。
type Repo struct {
	db    *bun.DB // 仅根 Repo 有，用于开事务
	Batch *BatchRepo
	Task  *TaskRepo
	Cache *CacheRepo
	Mask  *MaskRepo
	// CopyJob 付费成品转存素材库 outbox（Go_V1.0.30）
	CopyJob *CopyJobRepo
}

// New 根 Repo。
func New(db *bun.DB) *Repo {
	r := bind(db)
	r.db = db
	return r
}

func bind(db bun.IDB) *Repo {
	return &Repo{Batch: &BatchRepo{db: db}, Task: &TaskRepo{db: db}, Cache: &CacheRepo{db: db}, Mask: &MaskRepo{db: db}, CopyJob: &CopyJobRepo{db: db}}
}

// DB 底层 *bun.DB（事务内的 Repo 返回 nil）。
func (r *Repo) DB() *bun.DB { return r.db }

// RunInTx 在事务里执行 fn：fn 返回 nil 提交，返回 error / panic 回滚。tx 是绑在事务上的 Repo。
func (r *Repo) RunInTx(ctx context.Context, fn func(ctx context.Context, tx *Repo) error) error {
	if r.db == nil {
		return errors.New("repo: 事务内不能再开事务")
	}
	return bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		return fn(ctx, bind(tx))
	})
}

// IsDuplicate 是否唯一键冲突（MySQL 1062），如并发同 request_id 提交撞 uk_user_request。
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// noRows 把 sql.ErrNoRows 转成 (nil, nil)。
func noRows[T any](v *T, err error) (*T, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// affected 取 RowsAffected。
func affected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// msExpr 毫秒时间戳 → DATETIME(3) 表达式（库按会话时区换算）。
const msExpr = "FROM_UNIXTIME(? / 1000)"

// ---------------------------------------------------------------- 行租约（多实例）

// AcquireLease 抢批次的租约（pkg/lease.Acquire）：同一批次同一时刻只由一个 studio-rpc 实例推进。
func (r *BatchRepo) AcquireLease(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	return lease.Acquire(ctx, r.db, TableBatch, id, owner, ttl)
}

// ReleaseLease 释放批次的租约（只对 owner 生效）。
func (r *BatchRepo) ReleaseLease(ctx context.Context, id int64, owner string) error {
	return lease.Release(ctx, r.db, TableBatch, id, owner)
}

// KeepLease 抢批次的租约并后台心跳续租（pkg/lease.Keep）；ok=false 表示别的实例持有。
func (r *BatchRepo) KeepLease(ctx context.Context, id int64, owner string, ttl time.Duration) (context.Context, func(), bool, error) {
	return lease.Keep(ctx, r.db, TableBatch, id, owner, ttl)
}

// KeepLease 抢子任务的租约并后台心跳续租（pkg/lease.Keep）；ok=false 表示别的实例持有。
// stop 释放租约后再 MarkPolled（记下轮询结束时刻，ListByStatus 据此轮转；失败无妨，只影响排序）。
func (r *TaskRepo) KeepLease(ctx context.Context, id int64, owner string, ttl time.Duration) (context.Context, func(), bool, error) {
	lctx, stop, ok, err := lease.Keep(ctx, r.db, TableTask, id, owner, ttl)
	if !ok || err != nil {
		return lctx, stop, ok, err
	}
	var once sync.Once
	return lctx, func() {
		once.Do(func() {
			stop()
			mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = r.MarkPolled(mctx, id)
			cancel()
		})
	}, true, nil
}
