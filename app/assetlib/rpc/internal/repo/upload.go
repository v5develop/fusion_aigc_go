package repo

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/workspace"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件：「存为资产」——用户把自己的一张图存进素材库。**afv_asset 的第三处受控写入**
// （先例 pkg/asset 的写入、本包 Rename / SoftDelete）：只 INSERT 一行用户上传（source_type = 1），不改任何已有行。

// NewUpload 一条用户上传类资产。
type NewUpload struct {
	UserID       int64
	SourceTaskID string // 去重键（内容哈希派生），同一用户下未删除的同键资产只会有一条
	Name         string
	URL          string // 素材库公有地址，同时写 cover_url / result_url
	EntryCode    string
	Width        int32
	Height       int32
	AspectRatio  string
	Properties   string   // JSON 文本，可空
	SysTags      []string // 系统标签（规整同 pkg/asset：去重、截断、≤20 个）；空写 NULL
}

// bunSession 让 bun 的事务 / 连接满足 workspace.DB（与 sqlx.Session 同形）。
type bunSession struct{ db bun.IDB }

func (s bunSession) ExecCtx(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, q, args...)
}

func (s bunSession) QueryRowCtx(ctx context.Context, v any, q string, args ...any) error {
	return s.db.QueryRowContext(ctx, q, args...).Scan(v)
}

const insertUploadSQL = "INSERT INTO afv_asset (user_id, workspace_id, source_task_id, type, entry_code, name, cover_url, result_url, " +
	"width, height, aspect_ratio, properties, sys_tags, source_type, status) VALUES (?, ?, ?, 'image', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)"

// InsertUpload 写一行用户上传（个人空间不存在时顺手建，口径同 pkg/asset）。应在事务里调。
func (r *AssetRepo) InsertUpload(ctx context.Context, a NewUpload) (int64, error) {
	ws, err := workspace.EnsurePersonal(ctx, bunSession{r.db}, a.UserID, "")
	if err != nil {
		return 0, err
	}
	var props any
	if a.Properties != "" {
		props = a.Properties
	}
	var sysTags any
	if tags := asset.NormalizeSysTags(a.SysTags); len(tags) > 0 {
		b, _ := json.Marshal(tags)
		sysTags = string(b)
	}
	res, err := r.db.ExecContext(ctx, insertUploadSQL, a.UserID, ws, a.SourceTaskID, a.EntryCode, a.Name, a.URL, a.URL,
		a.Width, a.Height, a.AspectRatio, props, sysTags, model.SourceTypeUpload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// IDByKey 本人未删除、非草稿、source_task_id = key 的资产 id；没有返回 0。
func (r *AssetRepo) IDByKey(ctx context.Context, userID int64, key string) (int64, error) {
	var id int64
	err := owned(r.db.NewSelect().TableExpr("afv_asset AS a").Column("a.id").Where("a.source_task_id = ?", key), userID).
		OrderExpr("a.id DESC").Limit(1).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// IDByURL 本人未删除、非草稿，且 result_url 或 cover_url 就是 url 的资产 id（这张图已经在自己的资产库里）；没有返回 0。
func (r *AssetRepo) IDByURL(ctx context.Context, userID int64, url string) (int64, error) {
	var id int64
	err := owned(r.db.NewSelect().TableExpr("afv_asset AS a").Column("a.id").
		Where("(a.result_url = ? OR a.cover_url = ?)", url, url), userID).
		OrderExpr("a.id DESC").Limit(1).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// lockSeconds 命名锁等待上限：同一张图的并发点击最多互等这么久。
const lockSeconds = 5

// ErrLockBusy 命名锁没抢到（同一资源正在被另一个请求处理）。
var ErrLockBusy = errors.New("repo: 同一资源正在处理")

// lockName 命名锁名：MySQL 上限 64 字符，用户 id / key 任意长都先哈希，固定 15 + 40 = 55 字符。
func lockName(userID int64, key string) string {
	sum := sha1.Sum([]byte(strconv.FormatInt(userID, 10) + ":" + key))
	return "afv_asset_once:" + hex.EncodeToString(sum[:])
}

// CreateOnce 同一用户同一 key 只建一条：在 MySQL 命名锁内先查（本人未删除、source_task_id = key），有就直接返回 (id, false)；
// 没有才开事务调 create 建出来，返回 (id, true)。
//
// afv_asset 是 Java 表（Go 不加迁移），且一个任务的套图共用 source_task_id，加不了 (user_id, source_task_id) 唯一键，
// 所以并发去重只能靠锁。**GET_LOCK、事务、RELEASE_LOCK 全在同一条 *sql.Conn 上**：一个请求只占一个连接，
// 不会出现「锁占一条、事务再要一条」让 MaxOpenConns 被并发请求占满而互相等死的情况。
// 锁释放在提交之后，等锁的请求醒来时一定读得到已提交的行。抢锁超时返回 ErrLockBusy，调用方让用户稍后重试，绝不在没拿到锁时硬写。
// RELEASE_LOCK 失败（连接异常 / 返回不是 1）时丢弃该连接（driver.ErrBadConn），不还回池里带着锁复用；连接关闭时 MySQL 自动释放会话锁。
func (r *Repo) CreateOnce(ctx context.Context, userID int64, key string,
	create func(ctx context.Context, tx *Repo) (int64, error)) (int64, bool, error) {
	if r.db == nil {
		return 0, false, errors.New("repo: 事务内不能再取锁")
	}
	name := lockName(userID, key)
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Close()
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, lockSeconds).Scan(&got); err != nil {
		// 结果未知（可能已拿到锁）：连接不能回池
		discardConn(conn)
		return 0, false, err
	}
	if !got.Valid || got.Int64 != 1 {
		return 0, false, ErrLockBusy
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		var released sql.NullInt64
		if err := conn.QueryRowContext(rctx, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil || !released.Valid || released.Int64 != 1 {
			logx.WithContext(ctx).Errorf("[assetlib] 释放命名锁失败，丢弃连接 lock=%s released=%v err=%v", name, released, err)
			discardConn(conn)
		}
	}()

	var id int64
	created := false
	err = conn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		t := bind(tx, r.videoTypes)
		exist, err := t.Asset.IDByKey(ctx, userID, key)
		if err != nil {
			return err
		}
		if exist > 0 {
			id = exist
			return nil
		}
		id, err = create(ctx, t)
		created = err == nil
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return id, created, nil
}

// discardConn 让 database/sql 丢弃这条连接（Close 时关闭物理连接，而不是还回池）。
func discardConn(c bun.Conn) {
	_ = c.Raw(func(any) error { return driver.ErrBadConn })
}

// LockName 命名锁名（导出供测试校验长度 / 稳定性）。
func LockName(userID int64, key string) string { return lockName(userID, key) }
