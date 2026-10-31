package config

import (
	"aigc-local-life/pkg/fusion"
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/token"
	"aigc-local-life/pkg/unionauth"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	rest.RestConf
	// Swagger 是否提供接口文档页面 /swagger（联调期开着，上线前可关）
	Swagger bool `json:",default=true"`
	// Cors 允许跨域（任意来源，含 App 离线包的 Origin: null），放行 X-User-Id、X-Request-Id 的预检
	Cors bool `json:",default=true"`
	Auth struct {
		// Mode token：只认我们签发的令牌（线上用）；
		// dev：没带令牌时退回信任请求头 X-User-Id，**仅联调环境**
		Mode string `json:",default=dev,options=dev|token"`
		// Jwt 我们自己签发的令牌（Session 没配时用它）
		Jwt token.Conf
		// Session 和 AIGC（Java）服务同一套的登录会话（Redis）。配了就发它那套 token，
		// 一个 token 两边都认；没配则回落到上面的 Jwt。
		Session session.Conf
	}
	// Union 联盟（xxlm）用户服务：登录时用前端给的临时 token 换用户信息
	Union unionauth.Conf
	// Fusion 现有 Java 服务的联盟开放接口：配了就由它建档（它写同一张表，还会建团队），
	// 没配就我们自己写表
	Fusion fusion.Conf

	CopywritingRpc  zrpc.RpcClientConf
	DigitalHumanRpc zrpc.RpcClientConf
	NoteRpc         zrpc.RpcClientConf
	ImageGenRpc     zrpc.RpcClientConf
	// StudioRpc 商拍换装 / 姿态复刻 / 图片生成（/api/v1/studio）
	StudioRpc zrpc.RpcClientConf
	// AssetLibRpc 资产库增强（/api/v1/asset-library）
	AssetLibRpc zrpc.RpcClientConf
	// CanvasRpc 画布 + 提示词库（/api/v1/canvas、/api/v1/prompt-library）
	CanvasRpc zrpc.RpcClientConf
	// DataSource 仅 Storage.Mode=oss 时使用（从 afv_system_config 读公有桶配置）
	DataSource string `json:",optional"`
	// Storage 须与 digitalhuman-rpc 一致：rpc 只接受本存储签发的上传地址
	Storage storage.Conf
	Upload  struct {
		// MaxImageBytes 仅网关代传（开发用）时生效；直传的大小与类型由前端控制
		MaxImageBytes int64 `json:",default=10485760"`
		// CredentialTTLMinutes 直传凭证有效期
		CredentialTTLMinutes int `json:",default=30"`
		// MaxVideoBytes 仅网关代传（开发用）：视频上限
		MaxVideoBytes int64 `json:",default=52428800"`
	}
}
