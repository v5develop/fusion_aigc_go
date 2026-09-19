package svc

import (
	"aigc-life/app/admin/api/internal/config"
	"aigc-life/app/admin/api/internal/middleware"
	"aigc-life/app/copywriting/rpc/copywriting"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config         config.Config
	AdminAuth      rest.Middleware
	CopywritingRpc copywriting.Copywriting
}

func NewServiceContext(c config.Config) *ServiceContext {
	return &ServiceContext{
		Config:         c,
		AdminAuth:      middleware.NewAdminAuthMiddleware(c.Auth.Mode).Handle,
		CopywritingRpc: copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
	}
}
