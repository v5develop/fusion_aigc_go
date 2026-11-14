package config

import (
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/dbpool"
	"aigc-local-life/pkg/linkparse"
	"aigc-local-life/pkg/storage"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库
	DataSource string
	// DB 连接池上限（可选；不配用 pkg/dbpool 的服务默认值，见 docs/api/db-pool.md）
	DB dbpool.Config `json:",optional"`
	// Billing 计费：off 不计费（默认）/ shadow 只算价记日志 / on 真扣。由 deploy.env 的 BILLING_MODE 写入
	Billing billing.Conf
	// Aigc AIGC 执行面（海报 78、长图 76）。Endpoint 为空时提交返回「暂未开放」。
	Aigc aigc.Conf
	// Storage 须与网关一致：素材只接受本存储签发的上传地址
	Storage storage.Conf
	// Asset 成品写进素材库；Type/EntryCode 按 kind 取下面 Poster / LongImage 的配置
	Asset asset.Conf
	// AssetReconcile 付费产物入库对账：定时扫近 7 天成功且已扣费、素材库没有对应行的记录并幂等补入（pkg/asset.Reconciler）
	AssetReconcile asset.ReconcileConf
	Poster         KindConf
	// ActionTransfer 动作迁移：人物图 + 动作视频 → 视频（执行面 9，seedance 2.0 会路由到 86）
	ActionTransfer KindConf
	// LongImage 长图：模板必选（76 的 template_url 必填），比例固定 1:3（产品定），Model 留空不传（76 没有 model 参数）
	LongImage KindConf
	// ExploreShop 探店视频：72 出剧本 → 73 出片（缺省 model 走云端 81）
	ExploreShop VideoKindConf
	// ShortDrama 带货短剧：87 出剧本 → 88 出片（缺省 model 走云端 89）
	ShortDrama VideoKindConf
	// VideoRemake 爆款视频复刻：70 解析 → 71 复刻
	VideoRemake VideoRemakeConf
	// VideoLimits 两个视频模块的文本长度上限
	VideoLimits struct {
		MaxUserTextLen      int `json:",default=500"`
		MaxSellingPointsLen int `json:",default=500"`
	}
	// Voiceover 数字人口播：口播文案 + 人物图（+ 背景图）→ 视频（执行面 92，lite 路由到 91）
	Voiceover    KindConf
	TaskIDPrefix string `json:",default=AiCreationLife"`
	Limits       struct {
		MaxContentLen int `json:",default=500"`
		MaxMaterials  int `json:",default=3"`
		MaxImages     int `json:",default=4"`
	}
	// DigitalHumanRpc 解析人物图来源（上传 / 公共数字人 / AI定制形象）
	DigitalHumanRpc zrpc.RpcClientConf
	// CopywritingRpc 取店铺档案拼好的文本
	CopywritingRpc zrpc.RpcClientConf
	Poll           struct {
		IntervalSeconds int `json:",default=5"`
		TimeoutMinutes  int `json:",default=10"`
	}
	Moderation struct {
		Mode string `json:",default=keyword,options=keyword|off"`
	}
}

// KindConf 一种成品（海报 / 长图）的生成与归档参数。
type KindConf struct {
	// Module / Slot 模板在模板素材投放位里的位置（biz_type 见 BizType）
	BizType string `json:",default=localLife"`
	Module  string
	Slot    string `json:",default=template"`
	// Model 出图模型档位；留空则不传，用执行面默认
	Model string `json:",optional"`
	// DefaultSize 未选比例时使用
	DefaultSize string
	// TemplateRequired 模板是否必选
	TemplateRequired bool `json:",default=false"`
	// AssetType / AssetEntryCode 写进素材库时的取值
	AssetType      string `json:",optional"`
	AssetEntryCode string `json:",optional"`
	// TimeoutMinutes 生成超时；0 用 Poll.TimeoutMinutes（视频类出片慢，单独放宽）
	TimeoutMinutes int `json:",optional"`
	// Resolution 视频分辨率（数字人口播，仅云端 92 生效）；留空不传
	Resolution string `json:",optional"`
}

// MediaSvcConf 共享媒体服务的地址与鉴权（app/media）。
type MediaSvcConf struct {
	// BaseURL / BaseURLs 静态实例地址（单个或多个）；注册中心不可用时的回退
	BaseURL  string   `json:",optional"`
	BaseURLs []string `json:",optional"`
	// NacosServerAddr 注册中心（r-nacos）HTTP 地址 host:port。配置后按服务名订阅实例，静态地址只作回退
	NacosServerAddr  string `json:",optional"`
	NacosNamespace   string `json:",optional"`
	NacosGroup       string `json:",default=DEFAULT_GROUP"`
	NacosServiceName string `json:",default=aigc-media"`
	NacosUsername    string `json:",optional"`
	NacosPassword    string `json:",optional"`
	Token            string `json:",optional"`
}

// VideoRemakeConf 爆款视频复刻。源视频的限制见设计稿：仅支持 3 分钟、50M 以内。
type VideoRemakeConf struct {
	MaxVideoBytes   int64    `json:",default=52428800"` // 50M
	MaxVideoSeconds int      `json:",default=180"`      // 3 分钟
	Formats         []string `json:",optional"`         // 允许的扩展名，留空用内置白名单
	// MediaSvc 共享媒体服务（抽代表帧封面等）；BaseURL 留空则复刻成片不生成封面
	MediaSvc MediaSvcConf
	// LinkParse 链接解析服务（抖音/快手/小红书/bilibili 分享链接 → 可下载直链 + 标题 + 封面）。
	// BaseURL 留空表示「链接查视频」暂未开放，App 端只显示「上传视频」。
	LinkParse linkparse.Conf
	// FetchTimeoutMinutes 链接来源：下载直链 + 转存到我们的桶的整体超时
	FetchTimeoutMinutes int    `json:",default=10"`
	ParseTimeoutMinutes int    `json:",default=15"`
	GenTimeoutMinutes   int    `json:",default=40"`
	AssetType           string `json:",optional"`
	AssetEntryCode      string `json:",optional"`
}

// VideoKindConf 探店视频 / 带货短剧的生成与归档参数。
type VideoKindConf struct {
	// DefaultModel 不选时用哪个档位；DefaultSize / DefaultDuration 同理
	DefaultModel    string `json:",default=seedance2.5"`
	DefaultSize     string `json:",default=9:16"`
	DefaultDuration int    `json:",default=15"`
	// Resolution 仅云端（81 / 89）生效
	Resolution string `json:",optional"`
	// PlanTimeoutMinutes 出剧本超时；GenTimeoutMinutes 出成片超时
	PlanTimeoutMinutes int    `json:",default=10"`
	GenTimeoutMinutes  int    `json:",default=40"`
	AssetType          string `json:",optional"`
	AssetEntryCode     string `json:",optional"`
}

func (c *KindConf) Timeout(defMinutes int) int {
	if c.TimeoutMinutes > 0 {
		return c.TimeoutMinutes
	}
	return defMinutes
}
