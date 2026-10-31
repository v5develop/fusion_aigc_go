package svc

import (
	"aigc-local-life/app/assetlib/rpc/internal/config"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/storage"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ServiceContext struct {
	Config config.Config
	// DB 业务读写的 bun 句柄（bundb.New：复用 go-zero 连接池，挂 logx QueryHook，慢阈值 500ms）
	DB *bun.DB
	// Repo afv_asset（只读）+ 五张旁表的数据访问
	Repo *repo.Repo
	// Storage 校验图片地址归属（storage.Owned）
	Storage storage.Store
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	db, err := bundb.New(conn)
	if err != nil {
		logx.Must(err)
	}
	return &ServiceContext{Config: c, DB: db, Repo: repo.New(db, c.VideoTypes), Storage: storage.New(c.Storage, conn)}
}
