package main

import (
	"flag"
	"fmt"
	"net/http"

	"aigc-life/app/gateway/api/doc"
	"aigc-life/app/gateway/api/internal/config"
	"aigc-life/app/gateway/api/internal/handler"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/pkg/result"
	"aigc-life/pkg/swaggerui"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/rest"
)

var configFile = flag.String("f", "deploy/etc/gateway-api.yaml", "the config file")

func main() {
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	var opts []rest.RunOption
	if c.Storage.Mode == "local" {
		// 本地存储（仅开发）：/files 下提供上传与配音文件访问
		opts = append(opts, rest.WithFileServer("/files", filesOnly{http.Dir(c.Storage.LocalDir)}))
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
