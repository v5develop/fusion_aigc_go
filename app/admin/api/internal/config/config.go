package config

import (
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	rest.RestConf
	// Swagger 是否提供接口文档页面 /swagger（联调期开着，上线前可关）
	Swagger bool `json:",default=true"`
	// Cors 允许跨域，放行 X-Admin-Id、X-Request-Id 的预检
	Cors bool `json:",default=true"`
	Auth struct {
		// dev：从请求头 X-Admin-Id 读取管理员（接入现有 Admin 体系前使用）
		Mode string `json:",default=dev,options=dev"`
	}
	CopywritingRpc  zrpc.RpcClientConf
	DigitalHumanRpc zrpc.RpcClientConf
	NoteRpc         zrpc.RpcClientConf
	ImageGenRpc     zrpc.RpcClientConf
}
