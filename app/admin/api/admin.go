// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package main

import (
	"aigc-life/app/admin/api/doc"
	"aigc-life/pkg/result"
	"aigc-life/pkg/swaggerui"

	"flag"
	"fmt"

	"aigc-life/app/admin/api/internal/config"
	"aigc-life/app/admin/api/internal/handler"
	"aigc-life/app/admin/api/internal/svc"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/rest"
)

var configFile = flag.String("f", "deploy/etc/admin-api.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	server := rest.MustNewServer(c.RestConf)
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
