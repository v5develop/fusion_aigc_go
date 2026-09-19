package svc

import (
	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/gateway/api/internal/config"
	"aigc-life/app/gateway/api/internal/middleware"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config         config.Config
	Auth           rest.Middleware
	CopywritingRpc copywriting.Copywriting
}

func NewServiceContext(c config.Config) *ServiceContext {
	return &ServiceContext{
		Config:         c,
		Auth:           middleware.NewAuthMiddleware(c.Auth.Mode).Handle,
		CopywritingRpc: copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
	}
}
