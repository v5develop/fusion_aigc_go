package svc

import (
	"strings"

	"aigc-local-life/app/assetlib/rpc/internal/config"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/mediasvc"
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
	// Media 媒体服务：读时现签 mr://{rid}；未配置为 nil（mr:// 输出为空）
	Media *mediasvc.Client
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	db, err := bundb.New(conn)
	if err != nil {
		logx.Must(err)
	}
	return &ServiceContext{Config: c, DB: db, Repo: repo.New(db, c.VideoTypes), Storage: storage.New(c.Storage, conn),
		Media: newMediaSvc(c)}
}

// newMediaSvc 媒体服务客户端（与 studio / canvas 同口径）：配了注册中心就订阅实例，否则用静态地址；都没配返回 nil。
func newMediaSvc(c config.Config) *mediasvc.Client {
	m := c.Media
	var fallback []string
	for _, u := range append([]string{m.BaseURL}, m.BaseURLs...) {
		if strings.TrimSpace(u) != "" {
			fallback = append(fallback, u)
		}
	}
	if strings.TrimSpace(m.NacosServerAddr) != "" {
		return mediasvc.Discover(mediasvc.DiscoveryConfig{
			ServerAddr: m.NacosServerAddr, Namespace: m.NacosNamespace, Group: m.NacosGroup,
			ServiceName: m.NacosServiceName, Username: m.NacosUsername, Password: m.NacosPassword,
		}, m.Token, fallback)
	}
	if len(fallback) == 0 {
		return nil
	}
	return mediasvc.New(fallback, m.Token)
}
