package config

import (
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	rest.RestConf
	Auth struct {
		// dev：从请求头 X-User-Id 读取用户（接入现有用户体系前使用）
		Mode string `json:",default=dev,options=dev"`
	}
	CopywritingRpc zrpc.RpcClientConf
}
