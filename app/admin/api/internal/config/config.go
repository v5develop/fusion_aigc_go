package config

import (
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/token"

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
		// Jwt 独立模式（无会话 Redis，仅开发）的后台令牌；共享模式不用
		Jwt token.Conf
		// Session 与 Java 后台共用的会话 Redis（同 gateway）。配了就只认 Java 同款令牌，按 RBAC 查权限
		Session session.Conf
	}
	// DataSource 后台登录要读 afv_admin_user；不配则登录接口返回「暂未开放」
	DataSource      string `json:",optional"`
	CopywritingRpc  zrpc.RpcClientConf
	DigitalHumanRpc zrpc.RpcClientConf
	NoteRpc         zrpc.RpcClientConf
	ImageGenRpc     zrpc.RpcClientConf
}
