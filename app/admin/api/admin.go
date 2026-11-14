// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package main

import (
	"aigc-local-life/app/admin/api/doc"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/swaggerui"
	"context"

	"flag"
	"fmt"

	"aigc-local-life/app/admin/api/internal/config"
	"aigc-local-life/app/admin/api/internal/handler"
	plazahandler "aigc-local-life/app/admin/api/internal/handler/plaza"
	replicatehandler "aigc-local-life/app/admin/api/internal/handler/replicate"
	"aigc-local-life/app/admin/api/internal/svc"
	headline "aigc-local-life/app/plaza/headline/httpapi"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/rest"
)

var configFile = flag.String("f", "deploy/etc/admin-api.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	var opts []rest.RunOption
	if c.Cors { // 运营后台前端与接口不同域（Idempotency-Key：视频做同款人工退点 / 强制收尾的幂等键）
		opts = append(opts, rest.WithCorsHeaders("X-Admin-Id", "X-Request-Id", "Idempotency-Key"))
	}
	server := rest.MustNewServer(c.RestConf, opts...)
	defer server.Stop()
	// JSON 字段 snake_case / camelCase 双写（与 Java 命名对齐，迁移期两者并存）
	server.Use(jsoncase.Middleware)

	result.Setup()
	ctx := svc.NewServiceContext(c)
	handler.RegisterHandlers(server, ctx)
	plazahandler.Register(server, ctx) // /api/v1/plaza/admin/*：手写路由
	// /api/v1/replicate/admin/**：视频做同款后台（权限 replicate:view / refund / force，Go_V1.0.39）
	replicatehandler.Register(server, ctx)
	// 本周头条运营端 /api/v1/plaza/admin/headlines/**（权限同广场后台前缀 creaft_catalog:view / edit）
	headline.RegisterAdmin(server, ctx.AdminAuth, ctx.Headline)
	if ctx.Headline.Jobs != nil {
		// 头条后台任务只在 admin-api 跑（PlazaHeadline.Jobs，默认关）；多副本靠 afv_plaza_headline_ctl 行租约选主
		go ctx.Headline.Jobs.Start()
		defer ctx.Headline.Jobs.Stop()
	}
	if ctx.Plaza != nil {
		// 生命周期边界翻转 / 搜索缺口入库 / 回收站清理（多副本靠 Redis 锁只让一个跑）
		bg, cancel := context.WithCancel(context.Background())
		defer cancel()
		go ctx.Plaza.RunBackground(bg)
	}
	if c.Swagger {
		swaggerui.Register(server, doc.Title, doc.Spec) // 接口文档：/swagger
	}

	fmt.Printf("Starting server at %s:%d...\n", c.Host, c.Port)
	server.Start()
}
