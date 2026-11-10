package main

import (
	"flag"
	"fmt"
	"net/http"

	"aigc-local-life/app/gateway/api/doc"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/app/gateway/api/internal/handler"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/idempotency"
	"aigc-local-life/pkg/jsoncase"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/swaggerui"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/rest"
)

var configFile = flag.String("f", "deploy/etc/gateway-api.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	var opts []rest.RunOption
	if c.Cors {
		// App 内置 H5 离线包从 file:// 发请求（Origin: null），带自定义头会先发预检
		opts = append(opts, rest.WithCorsHeaders("X-User-Id", "Idempotency-Key", "X-Request-Id"))
	}
	if c.Storage.Mode == "local" {
		// 本地存储（仅开发）：/files 下提供上传与配音文件访问
		opts = append(opts, rest.WithFileServer("/files", filesOnly{http.Dir(c.Storage.LocalDir)}))
	}
	server := rest.MustNewServer(c.RestConf, opts...)
	defer server.Stop()
	// 幂等键：Idempotency-Key（AIGC 口径）同步成 X-Request-Id，旧头仍接受
	server.Use(idempotency.Middleware)
	// JSON 字段 snake_case / camelCase 双写（与 Java 命名对齐，迁移期两者并存）
	server.Use(jsoncase.Middleware)

	result.Setup()
	ctx := svc.NewServiceContext(c)
	handler.RegisterHandlers(server, ctx)
	if c.Swagger {
		swaggerui.Register(server, doc.Title, doc.Spec) // 接口文档：/swagger
	}

	fmt.Printf("Starting server at %s:%d...\n", c.Host, c.Port)
	server.Start()
}

// filesOnly 只提供文件，不列目录。
type filesOnly struct{ fs http.FileSystem }

func (f filesOnly) Open(name string) (http.File, error) {
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	if st, err := file.Stat(); err != nil || st.IsDir() {
		file.Close()
		return nil, http.ErrMissingFile
	}
	return file, nil
}
