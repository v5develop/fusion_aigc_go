// Package repo assetlib 的 bun 数据访问：Java afv_asset（只读）+ 五张旁表（Go_V1.0.23）。
//
// 约定：
//   - 全部方法带 ctx；查单条找不到返回 (nil, nil)，不返回 sql.ErrNoRows。
//   - 用户侧查询一律带归属条件 a.user_id = ? AND a.deleted = 0 AND 非草稿，越权与不存在不区分。
//   - 事务：Repo.RunInTx 给回调一个绑在事务上的 *Repo。
//   - afv_asset 只读；唯一例外是 AssetRepo.Rename（改标题，pkg/asset 没有改名方法）。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"aigc-local-life/pkg/bundb"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// Repo 数据访问入口，各子 Repo 共用同一个句柄（*bun.DB 或 bun.Tx）。
type Repo struct {
	db         *bun.DB
	videoTypes []string

	Asset    *AssetRepo
	Ext      *ExtRepo
	Version  *VersionRepo
	Relation *RelationRepo
	Tag      *TagRepo
}

// New 根 Repo。videoTypes 为额外判为视频的 afv_asset.type（见 config.VideoTypes）。
func New(db *bun.DB, videoTypes []string) *Repo {
	r := bind(db, videoTypes)
	r.db = db
	return r
}

func bind(db bun.IDB, videoTypes []string) *Repo {
	return &Repo{
		videoTypes: videoTypes,
		Asset:      &AssetRepo{db: db, videoTypes: videoTypes},
		Ext:        &ExtRepo{db: db},
		Version:    &VersionRepo{db: db},
		Relation:   &RelationRepo{db: db},
		Tag:        &TagRepo{db: db},
	}
}

// RunInTx 在事务里执行 fn：fn 返回 nil 提交，返回 error / panic 回滚。
func (r *Repo) RunInTx(ctx context.Context, fn func(ctx context.Context, tx *Repo) error) error {
	if r.db == nil {
		return errors.New("repo: 事务内不能再开事务")
	}
	return bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		return fn(ctx, bind(tx, r.videoTypes))
	})
}

// IsDuplicate 是否唯一键冲突（MySQL 1062）。
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// ErrAssetNotOwned 资产不属于该用户（写旁表前的纵深防御，调用方应已校验过归属）。
var ErrAssetNotOwned = errors.New("repo: 资产不属于该用户")

// ownedAssetSub 本人未删除资产 id 的子查询条件（带 userID 参数）。
const ownedAssetSub = "asset_id IN (SELECT id FROM afv_asset WHERE user_id = ? AND deleted = 0)"

// assertOwned 写入前确认 assetID 是 userID 的未删除资产。
func assertOwned(ctx context.Context, db bun.IDB, assetID, userID int64) error {
	n, err := db.NewSelect().TableExpr("afv_asset").Where("id = ?", assetID).Where("user_id = ?", userID).Where("deleted = 0").Count(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAssetNotOwned
	}
	return nil
}

func noRows[T any](v *T, err error) (*T, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

func affected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// likeArg LIKE 参数：转义 % _ \ 后两边加 %。
func likeArg(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}
