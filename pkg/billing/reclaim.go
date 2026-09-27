package billing

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Reclaimer 回收过期的预留单：到点还没确认的，把冻结退回去。
//
// ⚠️ **只收我们自己写的**（idempotency_key 以 golife: 开头）。
//
// Java 那边也有一个回收器在扫同一张表——证据是 afv_billing_reservation 上有
// idx_status_expire(status, expire_time, id)，admin 侧还有「只读查询过期占用扣费单」
// 和「手动触发过期占用扣费单恢复」两个接口。两边都去收同一批单子会互相抢：
// 它先把我们还在跑的任务的钱解冻了，等任务成功我们去确认，单子已经不是 HELD。
//
// 我们这边做了两件事挡它：
//  1. 我们写的单 expire_time 按任务类型给足（视频 90 分钟 > 任务超时 40 分钟）；
//  2. 这个回收器只认 golife: 前缀，不碰它的单。
//
// 还剩一件**必须他们配合**的：让他们的回收器也按前缀跳过我们的单。
// 没谈妥之前，视频类不要开 Mode=on。
type Reclaimer struct {
	conn   sqlx.SqlConn
	every  time.Duration
	cancel context.CancelFunc
	done   chan struct{}
}

func NewReclaimer(conn sqlx.SqlConn, every time.Duration) *Reclaimer {
	if every <= 0 {
		every = time.Minute
	}
	return &Reclaimer{conn: conn, every: every, done: make(chan struct{})}
}

func (r *Reclaimer) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	defer close(r.done)

	t := time.NewTicker(r.every)
	defer t.Stop()
	logx.Info("billing reclaimer started")
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Reclaimer) Stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
	}
}

// Tick 扫一轮。返回回收了几条（测试用）。
func (r *Reclaimer) Tick(ctx context.Context) int {
	var rows []struct {
		No string `db:"reservation_no"`
	}
	// 走 idx_status_expire。限 200 条，一轮收不完下一轮接着来，不长时间占锁。
	const q = "SELECT `reservation_no` FROM `afv_billing_reservation` " +
		"WHERE `status` = ? AND `deleted` = 0 AND `expire_time` IS NOT NULL AND `expire_time` < NOW() " +
		"AND `idempotency_key` LIKE ? ORDER BY `id` LIMIT 200"
	if err := r.conn.QueryRowsCtx(ctx, &rows, q, statusHeld, freezePrefix+"%"); err != nil {
		logx.WithContext(ctx).Errorf("billing reclaim: 查过期预留单失败: %v", err)
		return 0
	}
	n := 0
	for _, row := range rows {
		if err := refund(ctx, r.conn, row.No, "预留超时自动退回"); err != nil {
			logx.WithContext(ctx).Errorf("billing reclaim %s: %v", row.No, err)
			continue
		}
		logx.WithContext(ctx).Infof("billing reclaim: %s 过期已退回", row.No)
		n++
	}
	return n
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
