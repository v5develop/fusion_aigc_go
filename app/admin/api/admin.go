// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package main

import (
	"aigc-local-life/app/admin/api/doc"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/swaggerui"

	"flag"
	"fmt"

	"aigc-local-life/app/admin/api/internal/config"
	"aigc-local-life/app/admin/api/internal/handler"
	"aigc-local-life/app/admin/api/internal/svc"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/rest"
)

var configFile = flag.String("f", "deploy/etc/admin-api.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	var opts []rest.RunOption
	if c.Cors { // 运营后台前端与接口不同域
		opts = append(opts, rest.WithCorsHeaders("X-Admin-Id", "X-Request-Id"))
	}
	server := rest.MustNewServer(c.RestConf, opts...)
	defer server.Stop()

	result.Setup()
	ctx := svc.NewServiceContext(c)
	handler.RegisterHandlers(server, ctx)
	if c.Swagger {
		swaggerui.Register(server, doc.Title, doc.Spec) // 接口文档：/swagger
	}

	fmt.Printf("Starting server at %s:%d...\n", c.Host, c.Port)
	server.Start()
}
