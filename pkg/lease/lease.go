// Package lease 基于行的数据库租约：多实例部署时，同一行（批次 / 子任务 / node run）同一时刻只由一个实例推进。
//
// 表须有三列：lease_owner VARCHAR(64) NULL、lease_until DATETIME(3) NULL、updated_at（ON UPDATE CURRENT_TIMESTAMP），
// 建议索引 (status, lease_until)。租约读写都显式 updated_at = updated_at，不触发自动更新：
// updated_at 仍只反映业务变化（任务中心窗口同步、超时起点、轮询排队顺序都依赖它）。
//
// 语义：
//   - Acquire：行上没有租约或租约已过期才抢得到（严格互斥，同一 owner 也不可重入；进程内去重交给调用方）。
//   - Extend / Release：只对 owner 本人生效。Extend 在租约过期但尚未被别人抢走时仍可续上。
//   - 进程崩溃不释放：租约到期后自然可被其它实例接管。接管后的幂等由业务侧的幂等键 / CAS 保证。
//   - 时间一律用库里的 NOW(3)，与各实例本地时钟无关。
//
// 依赖 MySQL 默认的 RowsAffected =「实际变更行数」语义（DSN 不得加 clientFoundRows=true，见 pkg/bundb）。
// lease_until 每次都写成新的 NOW(3)+ttl，所以同一行两次 Extend 至少要隔 1ms（心跳间隔是秒级，满足）。
package lease

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Execer *sql.DB / *bun.DB / bun.Tx / bun.IDB 都满足。
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

var tableRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

var (
	ownerOnce sync.Once
	owner     string
)

// Owner 本进程的租约持有者标识：hostname-pid-8 位随机十六进制（≤64 字节）。进程内所有租约共用。
func Owner() string {
	ownerOnce.Do(func() {
		host, _ := os.Hostname()
		host = strings.Map(func(r rune) rune {
			if r == ' ' || r > 126 {
				return '_'
			}
			return r
		}, host)
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		suffix := fmt.Sprintf("-%d-%s", os.Getpid(), hex.EncodeToString(b))
		if max := 64 - len(suffix); len(host) > max {
			host = host[:max]
		}
		owner = host + suffix
	})
	return owner
}

func check(table, owner string, ttl time.Duration) error {
	if !tableRe.MatchString(table) {
		return fmt.Errorf("lease: 非法表名 %q", table)
	}
	if owner == "" || len(owner) > 64 {
		return errors.New("lease: owner 为空或超过 64 字节")
	}
	if ttl < time.Second {
		return errors.New("lease: ttl 至少 1 秒")
	}
	return nil
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Acquire 抢租约：行上无租约或租约已过期才成功（true）。false 且 err=nil 表示别的实例持有（或行不存在）。
func Acquire(ctx context.Context, db Execer, table string, id int64, owner string, ttl time.Duration) (bool, error) {
	if err := check(table, owner, ttl); err != nil {
		return false, err
	}
	return affected(db.ExecContext(ctx,
		"UPDATE `"+table+"` SET `lease_owner` = ?, `lease_until` = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), `updated_at` = `updated_at` "+
			"WHERE `id` = ? AND (`lease_until` IS NULL OR `lease_until` < NOW(3))",
		owner, ttl.Microseconds(), id))
}

// Extend 续租到 NOW(3)+ttl，只对 owner 生效。false 且 err=nil 表示租约已被别人抢走（调用方应停手）。
func Extend(ctx context.Context, db Execer, table string, id int64, owner string, ttl time.Duration) (bool, error) {
	if err := check(table, owner, ttl); err != nil {
		return false, err
	}
	return affected(db.ExecContext(ctx,
		"UPDATE `"+table+"` SET `lease_until` = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), `updated_at` = `updated_at` "+
			"WHERE `id` = ? AND `lease_owner` = ?",
		ttl.Microseconds(), id, owner))
}

// Release 释放租约（只对 owner 生效；已被别人接管时是空操作）。
func Release(ctx context.Context, db Execer, table string, id int64, owner string) error {
	if err := check(table, owner, time.Second); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		"UPDATE `"+table+"` SET `lease_owner` = NULL, `lease_until` = NULL, `updated_at` = `updated_at` "+
			"WHERE `id` = ? AND `lease_owner` = ?",
		id, owner)
	return err
}

// releaseTimeout 释放 / 心跳的独立超时（与调用方 ctx 的取消脱钩：进程停止时也要尽量把租约还回去）。
const releaseTimeout = 5 * time.Second

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
}

// Keep 抢一行的租约并在后台每 ttl/3 续一次，直到 stop。
//   - 抢不到返回 ok=false（ctx2 / stop 为 nil）。
//   - 返回的 ctx2 在租约明确丢失（Extend 返回 false）时取消，调用方据此停手；续租出错（库暂时不可用）不取消，下次再续。
//   - stop 停止心跳并释放租约（幂等）。
func Keep(ctx context.Context, db Execer, table string, id int64, owner string, ttl time.Duration) (ctx2 context.Context, stop func(), ok bool, err error) {
	ok, err = Acquire(ctx, db, table, id, owner, ttl)
	if err != nil || !ok {
		return nil, nil, ok, err
	}
	ctx2, cancel := context.WithCancel(ctx)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx2.Done():
				return
			case <-t.C:
				ectx, ecancel := detached(ctx2)
				held, err := Extend(ectx, db, table, id, owner, ttl)
				ecancel()
				if err == nil && !held {
					cancel() // 已被别人接管
					return
				}
			}
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(quit)
			<-done
			cancel()
			rctx, rcancel := detached(ctx)
			_ = Release(rctx, db, table, id, owner)
			rcancel()
		})
	}
	return ctx2, stop, true, nil
}

// Set 一组租约（同表同 owner）：逐个 Acquire / Release，后台每 ttl/3 用一条 UPDATE 给仍持有的全部续租。
// 适合单协程顺序推进一批行的轮询器（例如 canvas 每轮处理至多 BatchSize 个 run）。
type Set struct {
	db    Execer
	table string
	owner string
	ttl   time.Duration

	mu   sync.Mutex
	ids  map[int64]struct{}
	quit chan struct{}
	done chan struct{}
	once sync.Once
}

// NewSet 建一个租约集合并启动心跳；用完必须 Close。
func NewSet(db Execer, table, owner string, ttl time.Duration) *Set {
	s := &Set{db: db, table: table, owner: owner, ttl: ttl, ids: map[int64]struct{}{},
		quit: make(chan struct{}), done: make(chan struct{})}
	go s.heartbeat()
	return s
}

func (s *Set) heartbeat() {
	defer close(s.done)
	every := s.ttl / 3
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			ids := s.held()
			if len(ids) == 0 {
				continue
			}
			ctx, cancel := detached(context.Background())
			_, _ = ExtendMany(ctx, s.db, s.table, ids, s.owner, s.ttl)
			cancel()
		}
	}
}

func (s *Set) held() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	return out
}

// Acquire 抢一行的租约，抢到后纳入心跳。
func (s *Set) Acquire(ctx context.Context, id int64) (bool, error) {
	ok, err := Acquire(ctx, s.db, s.table, id, s.owner, s.ttl)
	if ok {
		s.mu.Lock()
		s.ids[id] = struct{}{}
		s.mu.Unlock()
	}
	return ok, err
}

// Release 释放一行的租约（不在集合里时什么也不做）。
func (s *Set) Release(ctx context.Context, id int64) {
	s.mu.Lock()
	_, ok := s.ids[id]
	delete(s.ids, id)
	s.mu.Unlock()
	if !ok {
		return
	}
	rctx, cancel := detached(ctx)
	_ = Release(rctx, s.db, s.table, id, s.owner)
	cancel()
}

// Close 停止心跳并释放仍持有的全部租约（幂等）。
func (s *Set) Close() {
	s.once.Do(func() {
		close(s.quit)
		<-s.done
		for _, id := range s.held() {
			s.Release(context.Background(), id)
		}
	})
}

// ExtendMany 给 owner 持有的多行一起续租，返回实际续上的行数。
func ExtendMany(ctx context.Context, db Execer, table string, ids []int64, owner string, ttl time.Duration) (int64, error) {
	if err := check(table, owner, ttl); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, ttl.Microseconds())
	marks := make([]string, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args = append(args, id)
	}
	args = append(args, owner)
	res, err := db.ExecContext(ctx,
		"UPDATE `"+table+"` SET `lease_until` = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), `updated_at` = `updated_at` "+
			"WHERE `id` IN ("+strings.Join(marks, ",")+") AND `lease_owner` = ?", args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FreeCond 轮询器列候选时附加的过滤条件（alias 为表别名，可空）：没有租约或租约已过期，减少空抢。
func FreeCond(alias string) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	return "(" + p + "lease_until IS NULL OR " + p + "lease_until < NOW(3))"
}
