package config

import (
	"aigc-life/pkg/storage"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	rest.RestConf
	Auth struct {
		// dev：从请求头 X-User-Id 读取用户（接入现有用户体系前使用）
		Mode string `json:",default=dev,options=dev"`
	}
	CopywritingRpc  zrpc.RpcClientConf
	DigitalHumanRpc zrpc.RpcClientConf
	NoteRpc         zrpc.RpcClientConf
	ImageGenRpc     zrpc.RpcClientConf
	// DataSource 仅 Storage.Mode=oss 时使用（从 afv_system_config 读公有桶配置）
	DataSource string `json:",optional"`
	// Storage 须与 digitalhuman-rpc 一致：rpc 只接受本存储签发的上传地址
	Storage storage.Conf
	Upload  struct {
		MaxImageBytes int64 `json:",default=10485760"` // 10MB
	}
}
