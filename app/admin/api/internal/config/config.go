package config

import (
	"aigc-local-life/app/plaza/core"
	"aigc-local-life/app/plaza/headline/service"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/dbpool"
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
	DataSource string `json:",optional"`
	// DB 连接池上限（可选；不配用 pkg/dbpool 的服务默认值，见 docs/api/db-pool.md）
	DB              dbpool.Config `json:",optional"`
	CopywritingRpc  zrpc.RpcClientConf
	DigitalHumanRpc zrpc.RpcClientConf
	NoteRpc         zrpc.RpcClientConf
	ImageGenRpc     zrpc.RpcClientConf
	// StudioRpc studio 运营后台（/api/v1/studio/admin）与 origin_ref 前缀 tyo/pse/simg 的重做、强制失败
	StudioRpc zrpc.RpcClientConf
	// AssetLibRpc 资产库后台（/api/v1/asset-library/admin）：标签字典、分类统计
	AssetLibRpc zrpc.RpcClientConf
	// CanvasRpc 画布 / 提示词库后台（/api/v1/canvas/admin、/api/v1/prompt-library/admin）
	CanvasRpc zrpc.RpcClientConf
	// Plaza 广场模板域后台（/api/v1/plaza/admin）：行业、模板、渠道、示例素材、行业建议、搜索缺口。库用上面的 DataSource。
	Plaza core.Config `json:",optional"`
	// PlazaHeadline 本周头条运营端（/api/v1/plaza/admin/headlines/**）与后台任务（摄入 / 刷新 / 校准 / 清理）。
	// 后台任务只在 admin-api 跑，PlazaHeadline.Jobs 默认 false，上线时打开（多副本靠行租约选主）。库用上面的 DataSource。
	PlazaHeadline service.Conf `json:",optional"`

	// ReplicateAdmin 视频做同款后台（/api/v1/replicate/admin/**）。库用上面的 DataSource。
	// Billing 必须与 gateway-api 的 Replicate.Billing 一致（同为 on），否则人工退点 / 强制收尾拒绝（避免把真实冻结的单误判为不计费）。
	ReplicateAdmin struct {
		Billing billing.Conf `json:",optional"`
		// TaskCenterEnabled 强制收尾后触发任务中心同步（须与 gateway-api 的 Replicate.TaskCenterEnabled 一致，默认同为 true）
		TaskCenterEnabled bool `json:",default=true"`
		// EnabledDefault 视频做同款总开关的部署默认值（须与 gateway-api 的 Replicate.Enabled 一致，部署工具同时写两处）。
		// 后台开关页据此显示「跟随部署配置」时的实际状态；库里 replicate.enabled 存在时以库为准。
		EnabledDefault bool `json:",default=false"`
		// ShareReward 用户投稿分享奖励规则的部署默认值（CONTRACT §U）；库里 replicate.share_reward.* 存在且合法时逐项以库为准。
		// Points 1–1000、MonthlyLimit 0–100（0 = 不限），非法值启动告警并退回内置默认 true / 30 / 3。
		// 用指针：整段缺省时 go-zero 不会套子字段 default，nil = 用内置默认（显式写 0 / false 则原样保留）。
		ShareReward struct {
			Enabled      *bool `json:",optional"`
			Points       *int  `json:",optional"`
			MonthlyLimit *int  `json:",optional"`
		} `json:",optional"`
		// Media 示例素材的上传 / 预览（媒体服务）。没配 BaseURL 时上传与解析接口回「媒体服务未配置」，粘贴编号的老路径不受影响。
		Media struct {
			BaseURL string `json:",optional"`
			Token   string `json:",optional"`
		} `json:",optional"`
	} `json:",optional"`
}

// ShareRewardDefaults 部署配置里的分享奖励规则（缺省项用内置默认 true / 30 / 3）。
func (c Config) ShareRewardDefaults() (enabled bool, points, monthlyLimit int) {
	enabled, points, monthlyLimit = true, 30, 3
	r := c.ReplicateAdmin.ShareReward
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	if r.Points != nil {
		points = *r.Points
	}
	if r.MonthlyLimit != nil {
		monthlyLimit = *r.MonthlyLimit
	}
	return
}
