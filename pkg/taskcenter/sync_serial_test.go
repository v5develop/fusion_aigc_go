package taskcenter

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// overlapConn 记录同时在执行的同步语句数。
type overlapConn struct {
	sqlx.SqlConn
	cur, peak, calls atomic.Int32
}

func (c *overlapConn) ExecCtx(context.Context, string, ...any) (sql.Result, error) {
	n := c.cur.Add(1)
	defer c.cur.Add(-1)
	c.calls.Add(1)
	for p := c.peak.Load(); n > p && !c.peak.CompareAndSwap(p, n); p = c.peak.Load() {
	}
	time.Sleep(5 * time.Millisecond)
	return nil, nil
}

// 轮询器与请求路径的异步同步并发调用 Sync：整轮串行，同步语句不重叠（不会对同一批行并发 upsert 互锁 / 死锁）。
func TestSyncSerialized(t *testing.T) {
	conn := &overlapConn{}
	y := New(conn, time.Minute,
		Source{Table: "afv_a", Prefix: "a", EntryCode: "e1", Type: "image", Status: "1", Result: "''"},
		Source{Table: "afv_b", Prefix: "b", EntryCode: "e2", Type: "video", Status: "1", Result: "''"})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := y.Sync(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if conn.peak.Load() != 1 || conn.calls.Load() != 12 {
		t.Fatalf("Sync 应整轮串行: peak=%d calls=%d", conn.peak.Load(), conn.calls.Load())
	}
}
