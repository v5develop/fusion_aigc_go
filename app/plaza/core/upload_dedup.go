package core

// 上传去重（confirm 阶段）：对象通过全部校验后，流式计算内容 sha256；同 sha256 已登记过就直接返回已登记的结果，
// 本次上传的对象成为孤儿：Cleanup.Enabled 开启时当场删除（失败入队重试），关闭时只记 PLAZA_UPLOAD_DUPLICATE 日志并入
// afv_plaza_upload_orphan 队列（清理任务见 upload_cleanup.go）。
// 去重是优化而非校验：算哈希 / 查登记失败一律放行（返回本次结果），不拦用户。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// uploadDedup 内容哈希登记。
type uploadDedup interface {
	// Find 已登记的结果；没有返回 nil。
	Find(ctx context.Context, sha string) (*ConfirmResult, error)
	// Register 登记；并发下别人先登记了则返回他的结果（existing != nil）。
	Register(ctx context.Context, sha, key string, r *ConfirmResult) (existing *ConfirmResult, err error)
}

type sqlDedup struct{ db *bun.DB }

func (d *sqlDedup) Find(ctx context.Context, sha string) (*ConfirmResult, error) {
	var raw []byte
	err := d.db.NewRaw("SELECT CAST(result_json AS CHAR) FROM afv_plaza_upload_hash WHERE sha256 = ? LIMIT 1", sha).Scan(ctx, &raw)
	if err != nil {
		if noRows(err) {
			return nil, nil
		}
		return nil, err
	}
	var r ConfirmResult
	if err := json.Unmarshal(raw, &r); err != nil || r.URL == "" {
		return nil, fmt.Errorf("登记行 result_json 不可用: %v", err)
	}
	return &r, nil
}

func (d *sqlDedup) Register(ctx context.Context, sha, key string, r *ConfirmResult) (*ConfirmResult, error) {
	b, _ := json.Marshal(r)
	res, err := d.db.ExecContext(ctx, "INSERT IGNORE INTO afv_plaza_upload_hash (sha256, object_key, url, result_json, size) VALUES (?, ?, ?, ?, ?)",
		sha, key, r.URL, string(b), r.Size)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, nil
	}
	return d.Find(ctx, sha) // 并发：别人先登记了
}

// hitToucher 去重命中时刷新登记行的 last_hit_time（清理宽限期据此判断）。可选能力。
//
// alive=false 表示登记行已不在（清理任务在 Find 之后把它连同对象删了）：调用方必须按未命中处理、保留本次新对象。
type hitToucher interface {
	Touch(ctx context.Context, sha string) (alive bool, err error)
}

// Touch 见 hitToucher。清理任务删登记行时持有行锁（见 cleanupRegistered），这里的 UPDATE 会等它提交：
// 已删 → 影响 0 行；回滚 → 照常刷新。影响 0 行也可能只是同一秒内重复命中（值没变），所以再查一次行是否还在。
func (d *sqlDedup) Touch(ctx context.Context, sha string) (bool, error) {
	res, err := d.db.ExecContext(ctx, "UPDATE afv_plaza_upload_hash SET last_hit_time = NOW() WHERE sha256 = ?", sha)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	var alive int
	if err := d.db.NewRaw("SELECT EXISTS (SELECT 1 FROM afv_plaza_upload_hash WHERE sha256 = ?)", sha).Scan(ctx, &alive); err != nil {
		return false, err
	}
	return alive > 0, nil
}

// objectDeleter 存储的删除能力（pkg/storage.Store 已有 Delete；单测里用它注入假存储）。
type objectDeleter interface {
	Delete(ctx context.Context, key string) error
}

// hashObject 流式读取对象算 sha256，最多读 max 字节；超过 max（对象比 Stat 报的大）视为不可去重。
func (u *Uploader) hashObject(ctx context.Context, raw string, max int64) (string, error) {
	rc, err := u.open(ctx, raw, max)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, max+1))
	if err != nil {
		return "", err
	}
	if n > max {
		return "", errors.New("对象超过大小上限，不参与去重")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dedupe 通过校验的 confirm 结果 → 去重后的结果。
func (u *Uploader) dedupe(ctx context.Context, key string, out *ConfirmResult) *ConfirmResult {
	if u.dedup == nil || out == nil {
		return out
	}
	sha, err := u.hashObject(ctx, out.URL, u.cfg.maxBytes(out.Kind))
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 上传去重跳过 判据=计算 sha256 失败 key=%s: %v", key, err)
		return out
	}
	existing, err := u.dedup.Find(ctx, sha)
	if err == nil && existing == nil {
		existing, err = u.dedup.Register(ctx, sha, key, out)
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 上传去重跳过 判据=读写登记表失败 key=%s sha256=%s: %v", key, sha, err)
		return out
	}
	if existing != nil {
		// 复用已登记对象：刷新命中时间，防止编辑器刚拿到它的 URL、还没保存就被清理任务当成「久未引用」删掉。
		if t, ok := u.dedup.(hitToucher); ok {
			alive, terr := t.Touch(ctx, sha)
			switch {
			case terr != nil:
				// 判不出登记对象是否还在：不复用，保留本次对象（去重是优化，宁可多留一份）
				logx.WithContext(ctx).Errorf("plaza: 上传去重跳过 判据=刷新命中时间失败 key=%s sha256=%s: %v（保留本次对象）", key, sha, terr)
				return out
			case !alive:
				// 登记行在 Find 之后被清理任务删了（对象也已删）：按未命中处理，本次对象重新登记并保留
				logx.WithContext(ctx).Infof("plaza: 上传去重未命中 判据=登记行已被清理（Touch 影响 0 行）key=%s sha256=%s 原登记=%s（保留本次对象）", key, sha, existing.URL)
				if again, rerr := u.dedup.Register(ctx, sha, key, out); rerr != nil || again != nil {
					logx.WithContext(ctx).Errorf("plaza: 去重未命中后重新登记没成功 key=%s sha256=%s 并发登记=%v: %v", key, sha, again != nil, rerr)
				}
				return out
			}
		}
	}
	if existing == nil || existing.URL == out.URL {
		return out
	}
	// 重复对象：开关开启 → 立刻删（刚刚确认过它不是登记对象，也没有任何模板引用过它）；删失败或开关关闭 → 入待清理队列。
	if u.cfg.Cleanup.Enabled {
		if d, ok := u.store.(objectDeleter); ok {
			if derr := d.Delete(ctx, key); derr == nil {
				logx.WithContext(ctx).Infof("plaza: PLAZA_UPLOAD_DUPLICATE 已删除重复对象 key=%s sha256=%s 复用=%s", key, sha, existing.URL)
				return existing
			} else {
				logx.WithContext(ctx).Errorf("plaza: PLAZA_UPLOAD_DUPLICATE 删除重复对象失败 key=%s: %v（入待清理队列重试）", key, derr)
			}
		}
	}
	logx.WithContext(ctx).Infof("plaza: PLAZA_UPLOAD_DUPLICATE key=%s url=%s sha256=%s 复用=%s（本次对象成为孤儿，已入待清理队列）", key, out.URL, sha, existing.URL)
	u.enqueueOrphan(ctx, key, out.URL, orphanDuplicate)
	return existing
}

// httpOpen 打开对象流（Range 限量，防整段拉超大文件）。
func httpOpen(ctx context.Context, rawURL string, max int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(max, 10))
	resp, err := uploadHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
