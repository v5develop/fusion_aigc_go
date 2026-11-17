package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"

	"aigc-local-life/app/gateway/api/doc"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/app/gateway/api/internal/handler"
	"aigc-local-life/app/gateway/api/internal/handler/canvasagent"
	"aigc-local-life/app/gateway/api/internal/handler/canvasevents"
	"aigc-local-life/app/gateway/api/internal/handler/internalauth"
	plazahandler "aigc-local-life/app/gateway/api/internal/handler/plaza"
	"aigc-local-life/app/gateway/api/internal/handler/replicateagent"
	"aigc-local-life/app/gateway/api/internal/svc"
	headline "aigc-local-life/app/plaza/headline/httpapi"
	plazav1 "aigc-local-life/app/plaza/v1compat"
	replicate "aigc-local-life/app/replicate/httpapi"
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
	// 服务间入口（/internal/v1/**：画布工具、复刻智能体）验签要原始请求体：须在 jsoncase 改写请求体之前存下
	server.Use(internalauth.PreserveInternalBody)
	// JSON 字段 snake_case / camelCase 双写（与 Java 命名对齐，迁移期两者并存）
	server.Use(jsoncase.Middleware)

	result.Setup()
	ctx := svc.NewServiceContext(c)
	handler.RegisterHandlers(server, ctx)
	// 画布智能体工具（MCP + Function Call）：不在 goctl 生成的 routes.go 里，单独注册
	canvasagent.Register(server, ctx)
	// 服务间入口共用一把校验器（同一份 CanvasInternal.Keys + 同一个 nonce 存储；没配可用密钥时都不注册）：
	// /internal/v1/canvas/agent/*（画布工具）与 /internal/v1/replicate/agent/*（复刻智能体工具）。
	if iv := internalauth.NewVerifier(ctx); iv != nil {
		canvasagent.RegisterInternal(server, ctx, iv)
		replicateagent.RegisterInternal(server, ctx, iv)
	}
	// /api/v1/canvas/canvases/:id/events（SSE）、/revision
	canvasevents.Register(server, ctx)

	plazahandler.Register(server, ctx)                  // /api/v1/plaza/*：手写路由，v1wire 信封
	headline.RegisterUser(server, ctx.Headline)         // GET /api/v1/plaza/headlines（公开；头条后台任务只在 admin-api）
	replicate.Register(server, ctx.Auth, ctx.Replicate) // /api/v1/replicate/**（未开启时回 7003）
	if c.Plaza.V1Compat.Enabled {
		// creaft v1 兼容三接口的 Go 复刻（默认不注册；注册了也要 nginx 切过来才有流量，DESIGN 9.5）
		plazav1.Register(server, plazav1.MustBuild(c.DataSource))
	}
	if ctx.Replicate != nil {
		// 轮询器：每行推进前抢 pkg/lease 行租约，多实例同时跑也安全
		go ctx.Replicate.Poller.Start()
		defer ctx.Replicate.Poller.Stop()
	}
	if ctx.Plaza.OverlayAutoReady() {
		// 自动叠字：扫描叠字意图的已入库产物、处理叠字作业（多副本靠行租约 / 条件更新互斥，不需要 Redis 锁）
		bg, cancel := context.WithCancel(context.Background())
		defer cancel()
		go ctx.Plaza.RunOverlayAutoLoop(bg)
	}
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
