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
// Java 那边也有一个回收器在扫同一张表：BillingHeldReservationRecoveryServiceImpl，
// @Scheduled(fixedDelay=60000) 每 60 秒一轮，Redis 锁互斥，每批 100 条，
// 条件是 status='HELD' AND expire_time <= now()-30min。
//
// **它不按幂等键或来源过滤**，扫出来之后按 biz_type 分派：IMAGE_GEN / VIDEO_GEN /
// CREATION 会去查它自己的任务表并自动确认或退款，认不出来的只打一条 WARN 就跳过。
// 所以我们靠 biz_type=LOCALLIFE 避开它（见 ledger.go 的说明），不是靠前缀。
//
// 我们自己这边仍然按前缀只收自己的，避免反过来动它的单。
// 过期走 UNFREEZE + expired:{no}，和它的 release(EXPIRED) 一致。
//
// 还剩一件**建议他们配合**的：像 EXTERNAL_EXECUTION 那样加一个显式跳过分支，
// 否则我们每有一张在途单，它每分钟就打一条 WARN，还占掉它 LIMIT 100 的名额。
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
		if err := release(ctx, r.conn, row.No); err != nil {
			logx.WithContext(ctx).Errorf("billing reclaim %s: %v", row.No, err)
			continue
		}
		logx.WithContext(ctx).Infof("billing reclaim: %s 过期已退回", row.No)
		n++
	}
	return n
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
