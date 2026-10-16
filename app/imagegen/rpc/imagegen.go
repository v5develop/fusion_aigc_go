package main

import (
	"flag"
	"fmt"

	"aigc-life/app/imagegen/rpc/internal/config"
	"aigc-life/app/imagegen/rpc/internal/logic"
	"aigc-life/app/imagegen/rpc/internal/server"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"aigc-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "deploy/etc/imagegen-rpc.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)
	// 轮询器每几秒查一次库，普通 SQL 日志会刷屏；慢查询日志保留
	sqlx.DisableStmtLog()
	ctx := svc.NewServiceContext(c)

	group := service.NewServiceGroup()
	defer group.Stop()

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterImageGenServer(grpcServer, server.NewImageGenServer(ctx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	group.Add(s)
	// 轮询执行面结果，回收超时的生成
	group.Add(logic.NewPoller(ctx))
	// 真扣费时回收过期没确认的预留单（只收我们自己的；三个服务共用一张表，由这里统一回收）
	if c.Billing.Mode == billing.ModeOn {
		group.Add(billing.NewReclaimer(sqlx.NewMysql(c.DataSource), 0))
	}

	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	group.Start()
}
