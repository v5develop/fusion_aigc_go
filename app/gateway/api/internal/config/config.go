package config

import (
	"aigc-local-life/app/plaza/core"
	hlservice "aigc-local-life/app/plaza/headline/service"
	replicate "aigc-local-life/app/replicate/httpapi"
	"aigc-local-life/pkg/dbpool"
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
	// Plaza 广场模板域（行业、模板、偏好、行业建议、匿名报价；/api/v1/plaza/*）。库用下面的 DataSource。
	Plaza core.Config `json:",optional"`
	// PlazaHeadline 本周头条用户端（GET /api/v1/plaza/headlines）。gateway-api 不跑头条后台任务（PlazaHeadline.Jobs 在这里无效，
	// 后台任务只在 admin-api）；库用下面的 DataSource，计数 Redis 默认复用 Auth.Session。
	PlazaHeadline hlservice.Conf `json:",optional"`
	// Replicate 视频做同款（/api/v1/replicate/**，契约 docs/api/api-replicate.md）。Enabled 默认 false；
	// 运行参数读 afv_system_config 的 replicate.*（总开关 replicate.enabled 默认 false）。库用下面的 DataSource。
	Replicate replicate.Conf `json:",optional"`
	// DataSource 仅 Storage.Mode=oss 时使用（从 afv_system_config 读公有桶配置）
	DataSource string `json:",optional"`
	// DB 连接池上限（可选；不配用 pkg/dbpool 的服务默认值，见 docs/api/db-pool.md）
	DB dbpool.Config `json:",optional"`
	// Storage 须与 digitalhuman-rpc 一致：rpc 只接受本存储签发的上传地址
	Storage storage.Conf
	// Studio 工作室接口的网关侧限制
	Studio struct {
		// PosePreviewPerMinute / PosePreviewPerDay 姿态配对预览（免费、但每次都要读图）每用户每分钟 / 每天（北京时间自然日）的次数上限，
		// 超出回 4002「识别太频繁，请稍后再试」。计数放在 Auth.Session 的 Redis（多实例共享）；没配 Session 时退回进程内计数（仅开发）。
		// <= 0 表示该窗口不限。
		PosePreviewPerMinute int `json:",default=6"`
		PosePreviewPerDay    int `json:",default=200"`
		// MaskPerMinute / MaskPerDay 区域蒙版（POST /studio/masks，免费、未命中缓存时要调分割）每用户每分钟 / 每天上限，
		// 超出回 4002「识别太频繁，请稍后再试」；计数同上（命中缓存的请求也计数）。<= 0 表示该窗口不限。
		MaskPerMinute int `json:",default=30"`
		MaskPerDay    int `json:",default=500"`
		// DetailsPerMinute / DetailsPerDay 商拍「AI 找细节」（POST /studio/tryon/details，免费、每次都要读图）每用户每分钟 / 每天上限，
		// 超出回 4002「识别太频繁，请稍后再试」；计数同上。<= 0 表示该窗口不限。
		DetailsPerMinute int `json:",default=6"`
		DetailsPerDay    int `json:",default=200"`
		// ColorFixAutoPerMinute / ColorFixAutoPerDay 参考图自动校色（POST /studio/colorfix/auto，免费、每次都要调求解服务，未命中蒙版缓存时还要一次分割）
		// 每用户每分钟 / 每天上限，超出回 4002 · COLORFIX_AUTO_TOO_FREQUENT；计数同上。<= 0 表示该窗口不限。
		ColorFixAutoPerMinute int `json:",default=6"`
		ColorFixAutoPerDay    int `json:",default=200"`
	}
	// CanvasAgent 画布智能体工具（POST /api/v1/canvas/mcp、/api/v1/canvas/agent/*；设计见 docs/design/canvas-agent-tools.md）
	CanvasAgent struct {
		// Enabled false 时三个入口都回 404（灰度开关）
		Enabled bool `json:",default=true"`
		// CallsPerMinute / CallsPerDay 每用户工具调用次数上限（含只读）；计数在 Auth.Session 的 Redis（多实例共享），<=0 不限
		CallsPerMinute int `json:",default=120"`
		CallsPerDay    int `json:",default=5000"`
		// GeneratePerMinute / GeneratePerDay 每用户生成类调用（canvas_run_node）上限，另计入上面的总数
		GeneratePerMinute int `json:",default=10"`
		GeneratePerDay    int `json:",default=300"`
		// McpTokenCallsPerMinute / McpTokenGeneratePerMinute 每个 mcp_ 访问令牌独立的每分钟上限（在上面的按用户限速之外再叠一层；
		// 令牌调用先过本层，超出回 is_error RATE_LIMITED）。生成类调用另计入总数；<=0 不限。登录令牌 / 服务间入口不受此限。
		McpTokenCallsPerMinute    int `json:",default=60"`
		McpTokenGeneratePerMinute int `json:",default=10"`
		// AllowedOrigins MCP 入口接受的浏览器 Origin（防 DNS rebinding）；空 = 只接受不带 Origin 的服务端 / 桌面客户端
		AllowedOrigins []string `json:",optional"`
		// MaxBodyBytes 请求体上限
		MaxBodyBytes int64 `json:",default=1048576"`
	}
	Upload struct {
		// MaxImageBytes 仅网关代传（开发用）时生效；直传的大小与类型由前端控制
		MaxImageBytes int64 `json:",default=10485760"`
		// CredentialTTLMinutes 直传凭证有效期
		CredentialTTLMinutes int `json:",default=30"`
		// MaxVideoBytes 仅网关代传（开发用）：视频上限
		MaxVideoBytes int64 `json:",default=52428800"`
	}
	// CanvasInternal 服务间内部入口（Java 智能体 → POST /internal/v1/canvas/agent/call、GET /internal/v1/canvas/agent/tools），
	// HMAC 鉴权（签名契约见 docs/design/canvas-agent-tools.md §12）。Keys 为空（或没有一把合格）时入口不注册并 WARN。
	CanvasInternal CanvasInternalConf `json:",optional"`
	// CanvasEvents 画布实时同步（GET /api/v1/canvas/canvases/:id/events SSE、/revision 轮询；订阅 Auth.Session 的 Redis）
	CanvasEvents CanvasEventsConf `json:",optional"`
}

// CanvasInternalConf 服务间 HMAC 入口配置。
type CanvasInternalConf struct {
	// Keys 多把密钥（KeyId → Secret），轮换时新旧并存；Secret ≥32 字节，不合格的 key 跳过并 WARN
	Keys []CanvasInternalKey `json:",optional"`
	// MaxSkewSeconds 时间戳允许偏差（秒）
	MaxSkewSeconds int `json:",default=300"`
	// NonceTTLSeconds nonce 防重放保留时长（秒，不小于 2×MaxSkewSeconds+30）；存 Auth.Session 的 Redis。
	// 没配会话 Redis 时：Mode=dev|test 退回进程内，其他模式入口不注册（ERROR）
	NonceTTLSeconds int `json:",default=630"`
}

// CanvasInternalKey 一把服务间密钥。
type CanvasInternalKey struct {
	KeyId  string
	Secret string
}

// CanvasEventsConf 画布实时同步。
type CanvasEventsConf struct {
	// MaxConnsPerUser 每用户同时打开的 SSE 连接上限（跨实例，计数在会话 Redis）
	MaxConnsPerUser int `json:",default=5"`
	// HeartbeatSeconds 心跳间隔（同时做一次 revision 对账）
	HeartbeatSeconds int `json:",default=15"`
	// MaxLifetimeMinutes 单条 SSE 最长存活，到点发 bye 让客户端带 Last-Event-ID 重连（顺带重新校验归属）
	MaxLifetimeMinutes int `json:",default=30"`
	// PollIntervalMs 无 Redis 降级时建议的轮询间隔
	PollIntervalMs int `json:",default=5000"`
}
