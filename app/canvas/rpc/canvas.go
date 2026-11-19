package main

import (
	"flag"
	"fmt"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/app/canvas/rpc/internal/logic"
	"aigc-local-life/app/canvas/rpc/internal/server"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/traceid"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var configFile = flag.String("f", "deploy/etc/canvas-rpc.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)
	// 自检：各类 run 的总时限须小于预留单时长，否则 fatal（见 Config.CheckHold）
	logx.Must(c.CheckHold())
	// 轮询器每几秒查一次库，普通 SQL 日志会刷屏；慢查询日志保留
	sqlx.DisableStmtLog()
	ctx := svc.NewServiceContext(c)

	group := service.NewServiceGroup()
	defer group.Stop()

	s := zrpc.MustNewServer(c.RpcServerConf, func(grpcServer *grpc.Server) {
		canvas.RegisterCanvasServer(grpcServer, server.NewCanvasServer(ctx))

		if c.Mode == service.DevMode || c.Mode == service.TestMode {
			reflection.Register(grpcServer)
		}
	})
	s.AddUnaryInterceptors(traceid.UnaryServerInterceptor) // X-Trace-Id：metadata → ctx + 日志字段
	group.Add(s)
	// 提交排队的 run、轮询 gateway / Java / studio、转存结算入库、同步任务中心
	group.Add(logic.NewPoller(ctx))
	// 真扣费时回收过期没确认的预留单（只收 golife: 前缀的本服务单据，与 imagegen 同一张表同一套规则，重复启动无害：
	// 每张单的回收是 CAS）。review（gateway UNKNOWN）不退款的 run 也靠它到期自动退回；Done/Fail 写库失败的单子同理兜底。
	if c.Billing.Mode == billing.ModeOn {
		group.Add(billing.NewReclaimer(sqlx.NewMysql(c.DataSource), 0))
	}

	fmt.Printf("Starting rpc server at %s...\n", c.ListenOn)
	group.Start()
}
