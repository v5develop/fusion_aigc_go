package taskcenter

// 锁行为验证（同样受 TASKCENTER_PARITY_DSN 控制）：新路径不对 afv_billing_reservation / 源表加锁，旧路径会。
//
//  1. 阻塞实验：另一个会话先持有预留单行（或源表行）的写锁不提交，各自跑一轮同步，会话级 innodb_lock_wait_timeout=1。
//     旧路径的 INSERT … SELECT 读到这些行要加共享锁 → 被挡住报 1205；新路径是快照读 → 不被挡，几乎立刻成功。
//  2. 锁清单实验：在同一个显式事务（RR）里执行同步用到的读取语句，再查 performance_schema.data_locks，
//     旧语句在预留单 / 源表上留下共享 next-key 锁，新语句一个锁都没有。

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// lockSummary 当前事务在 schema 下持有的锁：表名/模式 → 数量。
func lockSummary(t *testing.T, q interface {
	Query(string, ...any) (*sql.Rows, error)
}, schema string) map[string]int {
	t.Helper()
	rows, err := q.Query("SELECT object_name, lock_mode, COUNT(*) FROM performance_schema.data_locks "+
		"WHERE object_schema = ? AND engine_transaction_id = (SELECT trx_id FROM information_schema.innodb_trx WHERE trx_mysql_thread_id = CONNECTION_ID()) "+
		"GROUP BY object_name, lock_mode", schema)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var tb, mode string
		var n int
		if err := rows.Scan(&tb, &mode, &n); err != nil {
			t.Fatal(err)
		}
		out[tb+"/"+mode] = n
	}
	return out
}

func TestLockBehaviourLegacyVsNew(t *testing.T) {
	e := parityDB(t)
	ctx := context.Background()
	name := e.newSchema(t, "lock")
	db, _ := sql.Open("mysql", e.dsnFor(name))
	defer db.Close()
	var seq int64
	phase := readDBNow(t, db)
	f := fx{t, db, &seq, &phase}
	seed0(f)

	srcs := paritySources()
	mk := func(mode SyncMode, sources ...Source) *Syncer {
		return New(sqlx.NewMysql(e.dsnFor(name, "innodb_lock_wait_timeout=1")), 10*time.Minute, sources...).WithMode(mode)
	}
	hold := func(q string) func() {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
		return func() { tx.Rollback() }
	}

	// ---- 1) 阻塞实验：预留单行被写锁占着（模拟 Java / Go 的计费确认 UPDATE 还没提交）----
	release := hold("UPDATE afv_billing_reservation SET status = status WHERE biz_type = 'LOCALLIFE' AND biz_id = '7_r2'")
	t0 := time.Now()
	oldErr := mk(ModeLegacy, srcs[0]).Sync(ctx)
	oldDur := time.Since(t0)
	t0 = time.Now()
	newErr := mk(ModeNew, srcs[0]).Sync(ctx)
	newDur := time.Since(t0)
	release()
	t.Logf("预留单行被占：legacy err=%v 用时 %v；new err=%v 用时 %v", oldErr, oldDur.Round(time.Millisecond), newErr, newDur.Round(time.Millisecond))
	if oldErr == nil || !strings.Contains(oldErr.Error(), "1205") {
		t.Errorf("legacy 应被预留单行写锁挡住（1205），实际 err=%v", oldErr)
	}
	if newErr != nil || newDur >= 900*time.Millisecond {
		t.Errorf("new 不应被预留单行写锁挡住，err=%v 用时=%v", newErr, newDur)
	}

	// ---- 1b) 源表行被写锁占着（别的写者正在改源记录）----
	release = hold("UPDATE tc_note SET title = title WHERE id = 1")
	t0 = time.Now()
	oldErr = mk(ModeLegacy, srcs[3]).Sync(ctx)
	oldDur = time.Since(t0)
	t0 = time.Now()
	newErr = mk(ModeNew, srcs[3]).Sync(ctx)
	newDur = time.Since(t0)
	release()
	t.Logf("源表行被占：legacy err=%v 用时 %v；new err=%v 用时 %v", oldErr, oldDur.Round(time.Millisecond), newErr, newDur.Round(time.Millisecond))
	if oldErr == nil || !strings.Contains(oldErr.Error(), "1205") {
		t.Errorf("legacy 应被源表行写锁挡住（1205），实际 err=%v", oldErr)
	}
	if newErr != nil || newDur >= 900*time.Millisecond {
		t.Errorf("new 不应被源表行写锁挡住，err=%v 用时=%v", newErr, newDur)
	}

	// ---- 2) 锁清单：同一显式事务里执行读取语句 ----
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	run := func(label string, stmts func(tx *sql.Tx)) map[string]int {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		stmts(tx)
		got := lockSummary(t, tx, name)
		t.Logf("%s 持有的锁：%v", label, got)
		return got
	}
	s := srcs[0]
	oldLocks := run("legacy INSERT … SELECT（全量）", func(tx *sql.Tx) {
		args := s.args(0)
		// 目标表换成一次性库里的同名表：直接执行原语句
		if _, err := tx.Exec(s.sql(), args...); err != nil {
			t.Fatal(err)
		}
	})
	newLocks := run("new 读取语句（源表页 + 预留单 + 提交快照 + 预留单窗口）", func(tx *sql.Tx) {
		q, a := s.newRowsSQL(scanFilter{limit: 500})
		drain := func(q string, args ...any) {
			r, err := tx.Query(q, args...)
			if err != nil {
				t.Fatal(err)
			}
			for r.Next() {
			}
			r.Close()
		}
		drain(q, append(a, 500)...)
		drain(resvByKeysSQL(2), "7_r2", "8_r3")
		drain("SELECT exec_task_id, input FROM "+SubmissionTable+" WHERE exec_task_id IN (?,?)", "E2", "E13")
		drain("SELECT biz_id, update_time, id FROM afv_billing_reservation WHERE biz_type='LOCALLIFE' AND deleted=0 AND update_time >= DATE_SUB(CAST(? AS DATETIME(6)), INTERVAL ? SECOND) ORDER BY update_time, id LIMIT 1000", ts(phase, -time.Hour), 600)
	})
	heldOn := func(m map[string]int, table string) int {
		n := 0
		for k, v := range m {
			if strings.HasPrefix(k, table+"/") {
				n += v
			}
		}
		return n
	}
	if heldOn(oldLocks, "afv_billing_reservation") == 0 || heldOn(oldLocks, "tc_img") == 0 {
		t.Errorf("legacy 应在预留单与源表上持有锁: %v", oldLocks)
	}
	for _, tb := range []string{"afv_billing_reservation", "tc_img", SubmissionTable} {
		if n := heldOn(newLocks, tb); n != 0 {
			t.Errorf("new 的读取不应在 %s 上持有任何锁，实际 %d: %v", tb, n, newLocks)
		}
	}
}
