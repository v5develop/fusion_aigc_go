package main

import (
	"flag"
	"fmt"

	"aigc-local-life/app/copywriting/rpc/internal/config"
	"aigc-local-life/app/copywriting/rpc/internal/logic"
	"aigc-local-life/app/copywriting/rpc/internal/server"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "etc/copywriting.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)
	ctx := svc.NewServiceContext(c)

	group := service.NewServiceGroup()
	defer group.Stop()

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		pb.RegisterCopywritingServer(grpcServer, server.NewCopywritingServer(ctx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	group.Add(s)
	// 回收卡在「生成中」的记录（服务重启或崩溃留下的）
	group.Add(logic.NewStuckRecovery(ctx))

	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	group.Start()
}
