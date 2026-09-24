package config

import (
	"aigc-life/pkg/token"

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
		// Mode token：只认登录接口签发的令牌（线上用）；
		// dev：没带令牌时退回信任请求头 X-Admin-Id，**仅联调环境**
		Mode string `json:",default=dev,options=dev|token"`
		// Jwt 后台令牌，密钥与 App 那套分开
		Jwt token.Conf
	}
	// DataSource 后台登录要读 afv_admin_user；不配则登录接口返回「暂未开放」
	DataSource      string `json:",optional"`
	CopywritingRpc  zrpc.RpcClientConf
	DigitalHumanRpc zrpc.RpcClientConf
	NoteRpc         zrpc.RpcClientConf
	ImageGenRpc     zrpc.RpcClientConf
}
