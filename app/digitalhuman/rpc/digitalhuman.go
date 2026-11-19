package main

import (
	"flag"
	"fmt"

	"aigc-local-life/app/digitalhuman/rpc/internal/config"
	"aigc-local-life/app/digitalhuman/rpc/internal/logic"
	"aigc-local-life/app/digitalhuman/rpc/internal/server"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/traceid"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "deploy/etc/digitalhuman-rpc.yaml", "the config file")

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
		pb.RegisterDigitalHumanServer(grpcServer, server.NewDigitalHumanServer(ctx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(traceid.UnaryServerInterceptor) // X-Trace-Id：metadata → ctx + 日志字段
	group.Add(s)
	// 轮询执行面结果，回收超时与卡住的记录
	group.Add(logic.NewPoller(ctx))

	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	group.Start()
}
