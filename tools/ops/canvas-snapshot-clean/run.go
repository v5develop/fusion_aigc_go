package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 每行的结论（汇总按它计数）。
const (
	resClean    = "clean"     // 没有下划线副本
	resWould    = "would_fix" // dry-run：将清理
	resFixed    = "fixed"     // apply：已写回
	resConflict = "conflict"  // apply：写回时行已变（被保存 / 删除），跳过，重跑即可
	resBadJSON  = "bad_json"  // 快照不是合法 JSON：不动，人工看
	resError    = "error"     // 写回 / 备份出错
)

type options struct {
	Targets      []target
	Scan         scanOpts
	Limit        int  // 每张表最多扫多少行（0 = 不限）
	Apply        bool // false = dry-run
	BumpRevision bool
	Backup       io.Writer // apply 时写原快照（JSON Lines），写回前先落盘
	Sync         func() error
}

type summary struct {
	Scanned      int
	Polluted     int // 有副本的行
	KeysRemoved  int
	KeysDiffered int
	ByResult     map[string]int
}

// backupLine 备份文件一行：恢复时按 table + id 把 snapshot 原样写回（见 README）。
type backupLine struct {
	Table     string          `json:"table"`
	ID        string          `json:"id"`
	Revision  string          `json:"revision,omitempty"`
	UpdatedAt string          `json:"updated_at"`
	Snapshot  json.RawMessage `json:"snapshot"`
}

// pendingFix 一批里待写回的行。
type pendingFix struct {
	r       record
	out     []byte
	removed []removal
}

func run(ctx context.Context, st *store, o options, out io.Writer) (summary, error) {
	s := summary{ByResult: map[string]int{}}
	w := bufio.NewWriter(out)
	defer w.Flush()
	for _, t := range o.Targets {
		cursor, seen := startCursor(t), 0
		for {
			n := o.Scan.Batch
			if o.Limit > 0 && o.Limit-seen < n {
				n = o.Limit - seen
			}
			if n <= 0 {
				break
			}
			so := o.Scan
			so.Batch = n
			rows, err := st.nextBatch(ctx, t, cursor, so)
			if err != nil {
				return s, fmt.Errorf("%s: 查第 %d 行之后失败: %w", t.Table, seen, err)
			}
			if len(rows) == 0 {
				break
			}
			var fixes []pendingFix
			for _, r := range rows {
				seen++
				s.Scanned++
				cursor = r.ID
				cleaned, removed, err := cleanSnapshot(r.Snapshot)
				if err != nil {
					s.ByResult[resBadJSON]++
					fmt.Fprintf(w, "ROW table=%s id=%s result=%s err=%v\n", t.Table, r.ID, resBadJSON, err)
					continue
				}
				if len(removed) == 0 {
					s.ByResult[resClean]++
					continue
				}
				s.Polluted++
				s.KeysRemoved += len(removed)
				for _, rm := range removed {
					if rm.Differs {
						s.KeysDiffered++
					}
				}
				if !o.Apply {
					s.ByResult[resWould]++
					printRow(w, t, r, removed, resWould, nil)
					continue
				}
				fixes = append(fixes, pendingFix{r: r, out: cleaned, removed: removed})
			}
			if len(fixes) > 0 {
				if err := applyBatch(ctx, st, t, o, fixes, &s, w); err != nil {
					return s, err
				}
			}
			_ = w.Flush()
			if len(rows) < n {
				break
			}
		}
	}
	return s, nil
}

// applyBatch 先把整批原快照写进备份并落盘，再逐行乐观锁写回。备份失败：本批一行不写，并返回错误中止整次运行——
// 备份文件此时可能留着半行（磁盘满 / IO 错误），再往后追加只会让它更难恢复，修好后用新的备份文件重跑即可（幂等）。
func applyBatch(ctx context.Context, st *store, t target, o options, fixes []pendingFix, s *summary, w io.Writer) error {
	if err := backupBatch(t, o, fixes); err != nil {
		for _, f := range fixes {
			s.ByResult[resError]++
			printRow(w, t, f.r, f.removed, resError, fmt.Errorf("写备份失败，本行不写回: %w", err))
		}
		return fmt.Errorf("%s: 写备份失败，已中止（本批与之后的行都没有写回）: %w", t.Table, err)
	}
	for _, f := range fixes {
		rev := ""
		if o.BumpRevision && t.HasRevision {
			rev = newRevision()
		}
		ok, err := st.writeBack(ctx, t, f.r, f.out, rev)
		switch {
		case err != nil:
			s.ByResult[resError]++
			printRow(w, t, f.r, f.removed, resError, err)
		case !ok:
			s.ByResult[resConflict]++
			printRow(w, t, f.r, f.removed, resConflict, nil)
		default:
			s.ByResult[resFixed]++
			printRow(w, t, f.r, f.removed, resFixed, nil)
		}
	}
	return nil
}

func backupBatch(t target, o options, fixes []pendingFix) error {
	if o.Backup == nil {
		return fmt.Errorf("apply 模式没有备份输出")
	}
	for _, f := range fixes {
		line := backupLine{Table: t.Table, ID: f.r.ID, Revision: f.r.Revision, UpdatedAt: f.r.UpdatedAt, Snapshot: json.RawMessage(f.r.Snapshot)}
		b, err := json.Marshal(line)
		if err != nil {
			return err
		}
		if _, err := o.Backup.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	if o.Sync != nil {
		return o.Sync()
	}
	return nil
}

func printRow(w io.Writer, t target, r record, removed []removal, result string, err error) {
	differ := 0
	for _, rm := range removed {
		if rm.Differs {
			differ++
		}
	}
	fmt.Fprintf(w, "ROW table=%s id=%s removed=%d differed=%d result=%s", t.Table, r.ID, len(removed), differ, result)
	if err != nil {
		fmt.Fprintf(w, " err=%v", err)
	}
	fmt.Fprintln(w)
	for _, rm := range removed {
		note := "值相同"
		if rm.Differs {
			note = "值不同（副本已陈旧，保留 " + rm.Twin + "）"
		}
		fmt.Fprintf(w, "  - %s  %s\n", rm.Path, note)
	}
}

func printSummary(w io.Writer, s summary, apply bool) {
	mode := "DRY-RUN（未写库，加 -apply 才写）"
	if apply {
		mode = "APPLY"
	}
	parts := make([]string, 0, len(s.ByResult))
	for _, k := range []string{resClean, resWould, resFixed, resConflict, resBadJSON, resError} {
		if n := s.ByResult[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		}
	}
	fmt.Fprintf(w, "== %s 扫描 %d 行，有副本 %d 行，删除键 %d 个（其中值不同 %d 个）；%s\n",
		mode, s.Scanned, s.Polluted, s.KeysRemoved, s.KeysDiffered, strings.Join(parts, " "))
}

// newRevision 与 canvas-rpc 同一格式：<unix毫秒>-<4位随机hex>。
func newRevision() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

// openBackup 新建备份文件（已存在就拒绝，绝不覆盖上一次的备份），并 fsync 所在目录：新文件的目录项也落盘，
// 机器掉电后不会出现「库已写回、备份文件却不见了」。
func openBackup(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		serr := d.Sync()
		_ = d.Close()
		if serr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("fsync 备份目录失败: %w", serr)
		}
	}
	return f, nil
}
