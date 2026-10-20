// media 是封面 / 水印 / 合成 / 音视频处理的共享媒体服务，供 fusion_aigc_go 与 fusion-aigc（Java）调用。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aigc-local-life/app/media"

	"github.com/zeromicro/go-zero/core/conf"
)

func main() {
	cfgFile := flag.String("f", "deploy/etc/media-svc.yaml", "配置文件")
	flag.Parse()

	var c media.Config
	// UseEnv：展开配置里的 ${VAR}（容器里 Listen / Token 靠它注入）。不开的话 Token 会被当成字面量 "${…}"
	conf.MustLoad(*cfgFile, &c, conf.UseEnv())
	// 内网放行只能显式开：环境变量为 true 才打开（SSRF 防护的唯一开关，不进 yaml 的 ${…} 展开）
	if os.Getenv("MEDIA_ALLOW_PRIVATE_HOSTS") == "true" {
		c.AllowPrivateHosts = true
	}

	srv, err := media.NewServer(c)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 注册中心：启动后自注册，连不上会在后台重试，不影响服务本身
	deregister, err := media.Register(ctx, c.Registry)
	if err != nil {
		// 注册失败**不致命**：媒体服务仍可被静态地址调用（Go 调用方走 MEDIA_SVC_BASE_URL）。
		// 致命的是媒体服务本身起不来 —— Java 三个角色 depends_on 它 healthy，会被一起拖住。
		log.Printf("media: 注册到注册中心失败，本次不注册（调用方需走静态地址）: %v", err)
		deregister = func() {}
	}

	httpSrv := &http.Server{
		Addr:              c.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// 合成 / 水印要等 ffmpeg 跑完才回包，写超时跟着操作超时走
		WriteTimeout: time.Duration(c.OpTimeoutSeconds+60) * time.Second,
	}
	go func() {
		<-ctx.Done()
		// 先从注册中心摘掉，让调用方不再把新请求打过来；再等在途请求结束
		deregister()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(c.OpTimeoutSeconds)*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("media service listening on %s", c.Listen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
