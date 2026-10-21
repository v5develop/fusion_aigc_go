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
	retire := flag.String("retire", "", "下线实例 ID：写入墓碑列表后退出，此后该 ID 永不能再注册")
	flag.Parse()

	var c media.Config
	// UseEnv：展开配置里的 ${VAR}（容器里 Listen / Token 靠它注入）。不开的话 Token 会被当成字面量 "${…}"
	conf.MustLoad(*cfgFile, &c, conf.UseEnv())
	// 内网放行只能显式开：环境变量为 true 才打开（SSRF 防护的唯一开关，不进 yaml 的 ${…} 展开）
	if os.Getenv("MEDIA_ALLOW_PRIVATE_HOSTS") == "true" {
		c.AllowPrivateHosts = true
	}

	if *retire != "" {
		if err := media.Retire(c.Registry, *retire); err != nil {
			log.Fatal(err)
		}
		log.Printf("media: 已下线实例 %s，此后该 ID 不能再注册", *retire)
		return
	}

	srv, err := media.NewServer(c)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 软删除的清理：只删宽限期已过的待删除对象（设计文档 §8.5）
	go srv.RunSweeper(ctx)

	// 注册中心：启动后自注册。实例 ID 冲突或已下线属于致命错误：拒绝启动（非零退出）。
	// 注册中心暂时连不上不致命：后台重试，服务本身照常可用（调用方可走静态地址）。
	var fatal error
	deregister, err := media.Register(ctx, c.Registry, c.InstanceID, func(err error) {
		fatal = err
		stop()
	})
	if err != nil {
		if media.IsRefusedRegistration(err) {
			log.Fatalf("media: 拒绝启动: %v", err)
		}
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
	srv.Close()
	// 运行中发现实例 ID 冲突：已停止服务，以非零退出，不带着冲突的身份继续跑
	if fatal != nil {
		log.Fatalf("media: 拒绝继续运行: %v", fatal)
	}
}
