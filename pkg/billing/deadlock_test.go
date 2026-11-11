package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

var errDeadlock = &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}

// 冻结事务死锁回滚（1213）：整笔重做直到成功；包了一层的错误同样识别。
func TestRetryDeadlockRedoesUntilSuccess(t *testing.T) {
	n := 0
	err := retryDeadlock(context.Background(), func() error {
		n++
		if n <= 2 {
			return fmt.Errorf("tx: %w", errDeadlock)
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("死锁两次后第三次应成功: n=%d err=%v", n, err)
	}
}

// 非死锁错误（余额不足 / 撞唯一键 / 库不可用）不重做，原样返回。
func TestRetryDeadlockOtherErrorsNotRetried(t *testing.T) {
	for _, e := range []error{&NotEnough{Need: 10, Available: 1}, &mysql.MySQLError{Number: 1062}, errors.New("conn refused")} {
		n := 0
		err := retryDeadlock(context.Background(), func() error { n++; return e })
		if n != 1 || !errors.Is(err, e) {
			t.Fatalf("%v 不应重做: n=%d err=%v", e, n, err)
		}
	}
}

// 一直死锁：最多重做 deadlockRetries 次，返回死锁错误（调用方按冻结失败处理、善后退款）。
func TestRetryDeadlockGivesUp(t *testing.T) {
	n := 0
	err := retryDeadlock(context.Background(), func() error { n++; return errDeadlock })
	if n != deadlockRetries+1 || !isDeadlock(err) {
		t.Fatalf("应尝试 %d 次后放弃: n=%d err=%v", deadlockRetries+1, n, err)
	}
}

// ctx 已取消：不再退避重做。
func TestRetryDeadlockStopsOnCanceledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	err := retryDeadlock(ctx, func() error { n++; return errDeadlock })
	if n != 1 || !isDeadlock(err) {
		t.Fatalf("ctx 取消后不应重做: n=%d err=%v", n, err)
	}
}

// 并发调用互不干扰（-race）。
func TestRetryDeadlockConcurrent(t *testing.T) {
	var calls atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			_ = retryDeadlock(context.Background(), func() error {
				calls.Add(1)
				if first {
					first = false
					return errDeadlock
				}
				return nil
			})
		}()
	}
	wg.Wait()
	if calls.Load() != 16 {
		t.Fatalf("每个调用应恰好重做一次: %d", calls.Load())
	}
}

// 退避带抖动且逐次变长：互相死锁的几笔事务不会同一时刻重做再撞一次。
func TestDeadlockBackoffJitter(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := deadlockBackoff(0)
		if d < 10*time.Millisecond || d >= 20*time.Millisecond {
			t.Fatalf("第 1 次退避应在 [10ms,20ms): %v", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("退避应有随机抖动")
	}
	if d := deadlockBackoff(2); d < 30*time.Millisecond || d >= 60*time.Millisecond {
		t.Fatalf("第 3 次退避应在 [30ms,60ms): %v", d)
	}
}
