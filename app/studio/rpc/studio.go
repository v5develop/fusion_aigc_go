package main

import (
	"flag"
	"fmt"

	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/logic"
	"aigc-local-life/app/studio/rpc/internal/server"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "deploy/etc/studio-rpc.yaml", "the config file")

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
		studio.RegisterStudioServer(grpcServer, server.NewStudioServer(ctx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	group.Add(s)
	// 编排批次（切图 → 反推 → 生图）、轮询 gateway 结果、转存结算入库、同步任务中心
	group.Add(logic.NewPoller(ctx))
	// 真扣费时回收过期没确认的预留单（只收 golife: 的；release 是 CAS，与 imagegen-rpc 的回收器同时跑也安全）。
	// 结果未知（UNKNOWN）的子任务不主动退款、不再续期，预留单到期由它自动退回
	if c.Billing.Mode == billing.ModeOn {
		group.Add(billing.NewReclaimer(sqlx.NewMysql(c.DataSource), 0))
	}

	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	group.Start()
}
