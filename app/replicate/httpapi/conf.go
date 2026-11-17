// Package httpapi 视频做同款（/api/v1/replicate/**）的 gateway-api 接线：HTTP 路由、后台轮询器（pkg/lease 行租约，
// 多实例安全）、afv_system_config 的 replicate.* 运行参数。业务全部在 app/replicate/core，本包只做装配与参数转换。
// 契约：docs/api/api-replicate.md。
//
// 挂载（gateway-api main）：
//
//	rp := httpapi.MustBuild(c.Replicate, conn, httpapi.WithStorage(store)) // conn 为 DataSource 的 sqlx 连接；conn 为 nil 返回 nil；
//	                                                                       // store 为 gateway 的 storage.Store（校验 http(s) 输入归属，不传则用 Replicate.Storage）
//	httpapi.Register(server, ctx.Auth, rp)               // rp 为 nil 时所有接口回 7003「功能暂未开放」
//	go rp.Poller.Start(); defer rp.Poller.Stop()
package httpapi

import (
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/storage"
)

// Conf gateway-api 配置 Replicate（全部可选）。配了 DataSource 就装配、启动轮询器；Enabled 只决定默认是否接新提交。
type Conf struct {
	// Enabled 总开关的默认值（默认关）。数据库 > 文件配置：afv_system_config.replicate.enabled 存在且合法时以库为准（60 秒内生效），
	// 库里没有 / 值非法 / 读库失败才用这里。关着时写接口回 7003，只读接口与轮询器照常（在途任务要能收尾、退点）。
	Enabled bool `json:",default=false"`
	// LongformEnabled 多段视频（> 单段上限）是否开放：装配媒体服务区间切段（mediasvc.Client.Cut）。默认开（配了 Media 才生效）；
	// 运行时 afv_system_config.replicate.longform_enabled 存在时以它为准（应急关闭用）。
	LongformEnabled bool `json:",default=true"`
	// PollIntervalMs 轮询器每轮间隔；BatchSize 每轮最多推进多少条分析 / job；LeaseSeconds 行租约 TTL（每 TTL/3 续一次）。
	PollIntervalMs int `json:",default=2000"`
	BatchSize      int `json:",default=20"`
	LeaseSeconds   int `json:",default=120"`
	// Billing 分段预留（pkg/billing）：off 不计费（默认）/ shadow 只算价记日志 / on 真扣。
	Billing billing.Conf `json:",optional"`
	// AiGateway 台词（asr.transcribe）、画面理解（chat.completion）、出片（video.generate）。BaseURL 为空 = 分析 / 出片不可用。
	AiGateway struct {
		BaseURL        string `json:",optional"`
		KeyID          string `json:",optional"`
		Secret         string `json:",optional"`
		LongPollWait   int    `json:",default=20"` // 秒
		TimeoutSeconds int    `json:",default=30"`
	} `json:",optional"`
	// Media 共享媒体服务（落桶 / 探测 / 抽帧 / 联系表 / 区间切段）。配了 NacosServerAddr 订阅实例，BaseURL 只作回退。
	Media struct {
		NacosServerAddr  string   `json:",optional"`
		NacosNamespace   string   `json:",optional"`
		NacosGroup       string   `json:",default=DEFAULT_GROUP"`
		NacosServiceName string   `json:",default=aigc-media"`
		NacosUsername    string   `json:",optional"`
		NacosPassword    string   `json:",optional"`
		BaseURL          string   `json:",optional"`
		BaseURLs         []string `json:",optional"`
		Token            string   `json:",optional"`
	} `json:",optional"`
	// ASRModelGroup 台词转写模型组（空 = 跳过台词）；VisionModelGroup 画面理解（空 = 内核默认 vlm.qwen-3.8-flash）。
	// 运行时 afv_system_config.replicate_vision 存在时覆盖 VisionModelGroup。
	ASRModelGroup    string `json:",optional"`
	VisionModelGroup string `json:",optional"`
	// WriterEnabled 写手（确认后逐段改写出片提示词，失败回落方言模板）默认开；WriterModelGroup 空 = 用画面理解的模型组。
	// 运行时 afv_system_config.replicate.writer.enabled / replicate_writer 覆盖。
	WriterEnabled    bool   `json:",default=true"`
	WriterModelGroup string `json:",optional"`
	// Models 开放的出片模型组（空 = 四家全部）；DefaultModel 推荐模型。运行时 replicate.models（逗号分隔）/ replicate.default_model 覆盖。
	Models       []string `json:",optional"`
	DefaultModel string   `json:",optional"`
	// KeyPrefix 网关 / 媒体服务幂等键的环境前缀（如 "test-"），多个环境共用同一网关 / 媒体服务时避免 job id 撞键。
	// 默认空（与旧键一致）；只允许 [A-Za-z0-9._-]、≤32 字符，非法值拒收并告警（按空处理）。确认 / 分析开始时冻结，改它不影响在途键。
	KeyPrefix string `json:",optional"`
	// Storage 本站存储（校验 http(s) 源视频 / 参考图的归属：本存储签发、上传目录属于当前用户）。须与 gateway-api 顶层 Storage 一致；
	// 推荐 gateway 直接用 MustBuild(..., WithStorage(svcCtx.Storage)) 传入同一个实例，那时这里不生效。
	Storage *storage.Conf `json:",optional"`
	// InputURLPrefixes 另认的本站地址前缀（scheme+host+路径前缀，如素材库公有桶 / 媒体服务签名域名）。只放自有域名，不放第三方。
	InputURLPrefixes []string `json:",optional"`
	// URLTTLSeconds 任务视图里播放 / 下载地址的签名有效期（默认 3600，上限 86400）。
	URLTTLSeconds int `json:",default=3600"`
	// TaskCenterEnabled 视频做同款 job 同步进任务中心（pkg/taskcenter 新同步源，见 taskcenter.go）。默认开（Replicate 本身默认关）。
	TaskCenterEnabled bool `json:",default=true"`
	// Preview 定妆图（免费预览第 1 帧，agent 工具 replicate_preview_frame；需配 AiGateway）。
	// ModelGroup 出图的网关模型组（image.edit，默认与 studio 通用档一致）；DailyQuota 每人每个自然日（Asia/Shanghai）免费次数；WaitSeconds 等网关出图的上限。
	Preview struct {
		Enabled     bool   `json:",default=true"`
		ModelGroup  string `json:",default=img.gpt-image-2.5-sunburst"`
		DailyQuota  int    `json:",default=3"`
		WaitSeconds int    `json:",default=60"`
	} `json:",optional"`
	// Copy 发布文案（CONTRACT §N，免费；agent 工具 replicate_publish_copy；需配 AiGateway）。ModelGroup 写文案的网关 chat 模型组（空 = 用写手模型组，
	// 再回落画面理解模型组）；DailyQuota 每人每个自然日（Asia/Shanghai）次数（默认 20）；WaitSeconds 等网关的上限。
	// 运行时 replicate.copy.enabled / daily_quota 覆盖。
	Copy struct {
		Enabled     bool   `json:",default=true"`
		ModelGroup  string `json:",optional"`
		DailyQuota  int    `json:",default=20"`
		WaitSeconds int    `json:",default=60"`
	} `json:",optional"`
	// DecisionReminder 待决策提醒（CONTRACT §O2）：第 1 段做好后一直没处理，离自动结束（Cfg.ContinueWindow，默认 24 小时）还剩 LeadHours（默认 4）
	// 小时时站内通知提醒一次。运行时 replicate.decision_reminder.enabled / lead_hours 覆盖。
	DecisionReminder struct {
		Enabled   bool `json:",default=true"`
		LeadHours int  `json:",default=4"`
	} `json:",optional"`
	// BadOutput 坏片判定（J4）：段视频落桶后、确认扣点前检查黑屏 / 定格 / 解不开，命中按失败退点且不自动重试。
	// BlackRatio / FreezeRatio 为占视频时长的比例阈值（≥ 即判坏片）。运行时 replicate.bad_output.enabled / black_ratio / freeze_ratio 覆盖。
	BadOutput struct {
		Enabled     bool    `json:",default=true"`
		BlackRatio  float64 `json:",default=0.6"`
		FreezeRatio float64 `json:",default=0.8"`
	} `json:",optional"`
	// Asset 成片 / 单段入素材库（afv_replicate_delivery 发件箱 → 媒体服务复制到公有桶 → pkg/asset）。需配 Media。
	Asset struct {
		Enabled   bool   `json:",default=true"`
		EntryCode string `json:",default=video_replicate"` // afv_asset.entry_code（素材库分类归属）
	} `json:",optional"`
}
