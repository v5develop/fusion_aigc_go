package config

import (
	"aigc-local-life/pkg/dbpool"
	"strings"
	"time"

	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/colorfixsolver"
	"aigc-local-life/pkg/modelgroup"
	"aigc-local-life/pkg/storage"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库（afv_studio_* 新表 + 计费/任务中心/素材库共享表）
	DataSource string
	// DB 连接池上限（可选；不配用 pkg/dbpool 的服务默认值，见 docs/api/db-pool.md）
	DB dbpool.Config `json:",optional"`
	// Billing 计费：off 不计费（默认）/ shadow 只算价记日志 / on 真扣。由 deploy.env 的 BILLING_MODE 写入。
	// 每个子任务一张预留单；入口码见 EntryCodes。
	Billing billing.Conf
	// Storage 须与网关一致：输入图只接受本存储签发的地址（或素材库公有桶），成品转存公有桶
	Storage storage.Conf
	// InputURLPrefixes 除本存储签发的地址外，额外允许的输入图地址前缀（素材库公有桶域名，如 https://cdn.example.com/）。
	// pkg/storage 只认本服务前缀下的对象（storage.Owned），素材库里 Java 写的图不在该前缀下，所以在这里白名单放行。
	InputURLPrefixes []string `json:",optional"`
	// Asset 成品写进素材库（afv_asset）：每张图一行。EntryCode 不在这里配，按 kind 取 EntryCodes
	Asset asset.Conf
	// AssetReconcile 付费产物入库对账：定时扫近 7 天成功且已确认扣费、素材库没有对应行的子任务并幂等补入（pkg/asset.Reconciler）
	AssetReconcile asset.ReconcileConf
	// Library 付费成品转存素材库（resources/copy）移出交付路径：成品先以私有引用 mr://{rid}.{ext} 进交付中、结算、置成功、
	// 对用户可见（读时现签），转存在后台按 outbox（afv_studio_copy_job，Go_V1.0.30）完成：入素材库（只写公有地址）后把子任务成品
	// 换成素材库公有地址。免费档（mrw:// 水印视图）不受影响。
	Library struct {
		// AsyncCopy 关掉则回到同步转存（转存完才进交付中）。落 outbox 失败（如表还没建）时本次自动退回同步转存
		AsyncCopy bool `json:",default=true"`
		// Workers 本进程同时执行的转存任务数（即时转存 + 扫描接手共用）
		Workers int `json:",default=8"`
		// MaxAttempts 最多尝试几轮，之后记永久失败（成品仍以 mr:// 可看可下载，library_status=failed，不在素材库）
		MaxAttempts int `json:",default=10"`
		// BackoffSeconds 第 n 轮失败后等第 n 项秒数（超出取最后一项）。不配用 5,15,30,60,120,300,600,1800（累计约 50 分钟）
		BackoffSeconds []int `json:",optional"`
		// ClaimSeconds 认领一轮的时长：这么久没写回结果视为执行方崩溃，由扫描接手（同幂等键重做，不会重复落桶）
		ClaimSeconds int `json:",default=300"`
		// SweepBatch 每轮扫描最多接手几行
		SweepBatch int `json:",default=50"`
		// RetryCooldownSeconds 用户「重新存入素材库」的冷却：转存任务进永久失败不足这么多秒时不允许再次重置（每子任务约每分钟至多一次）
		RetryCooldownSeconds int `json:",default=60"`
		// AutoRequeueMax 转存永久失败（library_status=failed）后自动重投的代数上限（0 = 不自动重投，只留用户一键重试）：
		// 每重投一次进下一代（attempt 跳到下一个整千），本代再按 MaxAttempts / BackoffSeconds 跑满；用户一键重试同样进一代、计入上限。
		// 用尽后打 ERROR check=ASSET_LIBRARY_RETRY_EXHAUSTED
		AutoRequeueMax int `json:",default=5"`
		// AutoRequeueBackoffSeconds 第 n 代永久失败后等第 n 项秒数再自动重投（超出取最后一项）。不配用 1800,3600,7200,21600,43200
		AutoRequeueBackoffSeconds []int `json:",optional"`
		// AutoRequeueBatch 每轮最多自动重投几行（重投扫描每分钟一次）
		AutoRequeueBatch int `json:",default=20"`
	}
	// EntryCodes 三个入口码：计费 offer、素材库分类归属（Go_V1.0.21 seed）、任务中心 entry_code 三处同名
	EntryCodes struct {
		Tryon    string `json:",default=studio_tryon"`
		Pose     string `json:",default=studio_pose"`
		ImageGen string `json:",default=studio_imagegen"`
	}
	// TaskIDPrefix 子任务号与任务中心坐标（origin_ref / taskcenter.Ref）的前缀，按 kind 各一个。
	// 只能字母数字、不含下划线；不能与现有来源重复（img 已被海报/长图占用，所以图片生成用 simg）。
	TaskIDPrefix struct {
		Tryon    string `json:",default=tyo"`
		Pose     string `json:",default=pse"`
		ImageGen string `json:",default=simg"`
	}
	Poll struct {
		IntervalSeconds int `json:",default=5"`
		// Concurrency 每个批次同时在 gateway 生成中的子任务上限（s1.1 验收口径：生图并发 3）
		Concurrency int `json:",default=3"`
		// BatchConcurrency 同时编排（下载 / 切图 / 反推）的批次数。只作 KindBatchConcurrency.Tryon / Pose 未配时的回退值
		BatchConcurrency int `json:",default=2"`
		// KindBatchConcurrency 按 kind 分开的编排名额（各自一个池子，互不挤占）：商拍切图 + 反推一批要几分钟，
		// 共用一个池子时会把只需几秒（参考图入库 + 可选润色）的图片生成饿住。<=0 回退：Tryon / Pose → BatchConcurrency，ImageGen → 4
		KindBatchConcurrency struct {
			Tryon    int `json:",optional"`
			Pose     int `json:",optional"`
			ImageGen int `json:",optional"`
		} `json:",optional"`
		// Workers 同时长轮询 gateway 的子任务数（全进程）。每个生成中的子任务占一个名额持续长轮询直到终态，
		// 所以生效值取 max(Workers, MaxRunning)：名额少于 MaxRunning 时多派发的子任务只能等下一轮扫描，白等 Poll 间隔
		Workers int `json:",default=64"`
		// BatchSize 每轮最多推进多少个批次 / 子任务
		BatchSize int `json:",default=100"`
		// MaxRunning 全进程（多实例时各实例按库里总数判断）同时在 gateway 生成中的子任务上限，达到后不再派发新的
		MaxRunning int `json:",default=64"`
		// HoldConcurrency 提交时逐张冻结（Hold + 写计费镜像）的并发度。任一张失败：已冻结的全部退回（各退一次）。
		// 同一用户的冻结在账户行锁上仍是串行的，并发省的是报价查询与往返；<=0 按 4
		HoldConcurrency int `json:",default=4"`
		// PreviewCacheCleanMinutes 清理配对预览缓存（afv_studio_prompt_cache.batch_id < 0）的间隔（分钟），每个实例各自按此间隔跑，
		// 删除幂等、并发安全；<= 0 关闭。PreviewCacheRetainHours 只删早于这么多小时的行（须明显大于复用窗口 2h）；
		// PreviewCacheCleanBatch 每条 DELETE 的行数上限。
		PreviewCacheCleanMinutes int `json:",default=10"`
		PreviewCacheRetainHours  int `json:",default=24"`
		PreviewCacheCleanBatch   int `json:",default=500"`
	}
	// Moderation 内容审核：keyword-关键词过滤（与 AI文案共用 BLOCKWORD 词库）、off-不审核
	Moderation struct {
		Mode string `json:",default=keyword,options=keyword|off"`
	}
	// AiGateway 生图与提示词润色（/v1/calls，HMAC 七头签名，调用方 fusion-go）。BaseURL 为空时提交返回「暂未开放」。
	AiGateway struct {
		BaseURL string `json:",optional"`
		KeyID   string `json:",optional"` // 如 fusion-go-v1，对应 ai-gateway AIG_CALLER_KEYS
		Secret  string `json:",optional"`
		// LongPollWait GET /v1/calls/{id}?wait= 的长轮询秒数
		LongPollWait   int `json:",default=20"`
		TimeoutSeconds int `json:",default=30"` // 单次 HTTP 请求超时（不含长轮询等待）
		// MaxCostCNY 每次生图调用的供应商成本上限（limits.max_cost，人民币十进制字符串；网关软上限，只在新尝试前检查）
		MaxCostCNY string `json:",default=5.00"`
	}
	// Rewrite 图片生成 rewrite_prompt=true 时经 gateway chat.completion 润色一次；失败按原提示词继续
	Rewrite struct {
		// ModelGroup 无参考图时的润色模型组（纯文本）
		ModelGroup string `json:",default=llm.deepseek-flash"`
		// VisionModelGroup 有参考图时的润色模型组：llm.* 传图会被网关拒（route.no_capable_deployment），只有 vlm.* 带视觉
		VisionModelGroup string `json:",default=vlm.qwen-3.8-flash"`
	}
	// Reverse 反推 sidecar（Python，只监听内网）：crop-plan / crop-check / analyze-batch / pose source / pose prompt
	Reverse struct {
		BaseURL     string `json:",default=http://127.0.0.1:8090"`
		Timeout     int    `json:",default=200"` // 秒；单次 HTTP（sidecar 读图单次最长 180s，与 pkg/reverse.DefaultTimeout 一致）
		Concurrency int    `json:",default=6"`   // analyze-batch 并发（sidecar 允许 1–6）；姿态反推并发
		// PreviewConcurrency 姿态配对预览（免费）全进程同时在途的 sidecar 读图上限。预览每次读图先占一个预览名额
		// 再进客户端共享信号量，所以最多占 Concurrency 里的这么多个，付费编排始终保有 ≥ Concurrency-PreviewConcurrency 个；
		// 生效值夹到 [1, Concurrency-1]（Concurrency=1 时为 1，与编排共用）
		PreviewConcurrency int `json:",default=2,range=[1:6]"`
	}
	// ColorFixSolver 参考图驱动的自动校色求解服务（tools/colorfix-solver，Python，只返回五个滑杆值）。Endpoint 为空 = 功能关闭，
	// POST /api/v1/studio/colorfix/auto 回 7003 · COLORFIX_AUTO_DISABLED。求解端自己下载成品 / 参考图 / 蒙版，
	// 它的 COLORFIX_FETCH_ALLOW_HOSTS 要放行 OSS 域名（或集群内开 COLORFIX_ALLOW_PRIVATE_FETCH）。
	ColorFixSolver colorfixsolver.Conf `json:",optional"`
	// Media 共享媒体服务：下载、压缩（conform ≤1600px）、裁图、组图、resources/copy 转存。
	// 配了 NacosServerAddr 订阅实例，BaseURL 只作回退；都留空则切图不可用（商拍提交返回「暂未开放」）。
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
	}
	// ModelGroups 前端模型档 → ai-gateway model group 码。「通用」= 运营配置的默认组。
	ModelGroups map[string]string `json:",optional"`
	// Timeouts 各步骤超时（秒），对齐 s1.1：切图 / 反推 120s，生图 300s
	Timeouts struct {
		Crop     int `json:",default=120"`
		Analyze  int `json:",default=120"`
		Generate int `json:",default=300"`
		// DeliverGrace 交付中（已出图、待结算）最多停留多久：超期仍结算不了就告警并按预留单状态置失败 / 退款，保证有出口
		DeliverGrace int `json:",default=1800"`
		// VendorSentWait 生成超时要取消、但网关以「已发给供应商」拒绝取消（CANCEL_NOT_ALLOWED）的子任务，超时后最多再等多久（秒）。
		// 受理即扣费、取消省不下钱，所以不超时失败退款，而是等调用到终态照常处理（成功交付结算 / 失败退款 / 未知交回收器）；
		// 过了「开始生成 + Generate + VendorSentWait」仍未终态才按超时失败退款，打 ERROR（check=studio.vendor_sent_wait_exceeded）。
		// 默认 600 秒：已受理的供应商通常几分钟内出结果；预留单由续期保持有效。<=0 按 600。
		VendorSentWait int `json:",default=600"`
	}
	// TryonPrompt 商拍反推提示词版本灰度：sidecar prompt_version=new 实验版（2026-11-07.2 PROMPT_VERSIONS：
	// Seedream 结构化短版小幅提升、Nano 短版偏低），所以只灰度 Seedream，Nano 及其它模型一律稳定版。
	TryonPrompt struct {
		// NewPercent 0–100：模型组为 Seedream 的商拍批次按批次号哈希分桶，桶号 < NewPercent 的走
		// prompt_version=new + target_model=seedream；0 = 关闭（默认）。同一批次（含重做）结果固定。
		// 运行时覆盖：afv_system_config 有 studio_tryon_new_prompt_percent 行且值为 0–100 整数时以它为准（60s 内生效，不用重启）。
		NewPercent int `json:",default=0,range=[0:100]"`
	}
	// Limits 服务端复查的上限（与前端一致，改了要同步前端）
	Limits struct {
		MaxPromptLen int `json:",default=3000"`
	}
	// FreeWatermark 免费档图片的读时参数水印（与 Java app.free-watermark 同一套语义，见 docs/api/api-studio.md「免费档水印」）。
	// Enabled 且 Process 合法时，免费档用户（pkg/usertier.WatermarkedImage）的新成品不转存公有素材库桶：原件留在私有桶，
	// result_url 与素材库行写水印视图 mrw://{rid}.{ext}，每次读都签成带 x-oss-process 水印参数的地址（参数在签名内）。
	// 关掉 → 新任务照旧转存公有桶；已落库的 mrw:// 仍按 Process 签水印地址（Process 为空则输出空，绝不签干净原件）。
	FreeWatermark FreeWatermarkConf
}

// FreeWatermarkConf 免费档水印配置。
type FreeWatermarkConf struct {
	// Enabled 总开关（默认关），对应 Java FREE_WATERMARK_ENABLED。
	Enabled bool `json:",default=false"`
	// Process 与 Java 同一个 OSS 处理串（Java 启动日志 check=free_watermark.process 的值 / FREE_WATERMARK_OSS_PROCESS），
	// 也与 assetlib FreeWatermark.Process 相同。只允许 [A-Za-z0-9_./,-]，不合法按未配置处理。
	Process string `json:",optional"`
	// SignedTTLSeconds 水印地址签名有效期（秒，最少 60），对应 Java app.free-watermark.signed-ttl-seconds。
	SignedTTLSeconds int `json:",default=600"`
	// UserTierDefault / DisplayWatermarkDefault afv_system_config 没有 user_tier.enabled / watermark.display_enabled 行时的默认值，
	// 对应 Java USER_TIER_ENABLED / WATERMARK_ENABLED（默认都是 true）。
	UserTierDefault         bool `json:",default=true"`
	DisplayWatermarkDefault bool `json:",default=true"`
	// PromoteMinutes 付费转存扫描间隔（分钟，<=0 关闭）：已付费用户的 mrw:// 成品复制到公有素材库桶并改写引用。
	PromoteMinutes int `json:",default=10"`
	// CleanupEnabled 到期清理开关（默认关，对应 Java free-original.cleanup-enabled）：免费档用户超过保留期的水印视图成品
	// （仅终态成功、且用户仍未付费的未删除子任务）删掉私有原件、素材库行与子任务（逻辑删除）。
	CleanupEnabled bool `json:",default=false"`
	// RetentionDays 保留天数（从子任务结束时间算，默认 90，对应 Java free-original.retention-days）。
	RetentionDays int `json:",default=90"`
	// CleanupMinutes 到期清理扫描间隔（分钟，<=0 关闭）。
	CleanupMinutes int `json:",default=60"`
}

// ValidProcess 处理串可用（非空且只含 [A-Za-z0-9_./,-]，与媒体服务签名校验同一字符集）。
func (c FreeWatermarkConf) ValidProcess() bool {
	p := c.Process
	if p == "" {
		return false
	}
	for _, r := range p {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("_./,-", r)) {
			return false
		}
	}
	return true
}

// Active 新任务是否走水印视图（Java FreeImageWatermark.enabled()：开关开且处理串可用）。
func (c FreeWatermarkConf) Active() bool { return c.Enabled && c.ValidProcess() }

// PreviewPoseMethod / PreviewPoseTimeout 姿态配对预览要读原图 + 每张姿势图（sidecar 单次读图可达 60–140s），
// 远超 RPC 默认 30s：没在 MethodTimeouts 里单独配时由 EnsureMethodTimeouts 补上。gateway-api 侧同样按方法放宽调用超时。
const (
	PreviewPoseMethod  = "/studio.Studio/PreviewPose"
	PreviewPoseTimeout = 240 * time.Second
)

// EnsureMethodTimeouts 给长耗时方法补默认的服务端超时（已配置的不动）。
func (c *Config) EnsureMethodTimeouts() {
	for _, m := range c.MethodTimeouts {
		if m.FullMethod == PreviewPoseMethod {
			return
		}
	}
	c.MethodTimeouts = append(c.MethodTimeouts, zrpc.MethodTimeoutConf{FullMethod: PreviewPoseMethod, Timeout: PreviewPoseTimeout})
}

// PreviewSidecarSlots 姿态配对预览全进程同时在途的 sidecar 读图上限：PreviewConcurrency 夹到 [1, Concurrency-1]，
// 保证付费编排至少留有 Concurrency-该值 个 sidecar 名额（Concurrency ≤1 时只能共用，返回 1）。
func (c Config) PreviewSidecarSlots() int {
	conc := c.Reverse.Concurrency
	if conc <= 0 {
		conc = 3 // 与 pkg/reverse.DefaultConcurrency 一致
	}
	n := c.Reverse.PreviewConcurrency
	if n <= 0 {
		n = 2
	}
	return max(1, min(n, conc-1))
}

// DefaultModelGroups ModelGroups 没配时的映射（设计方案 §4.4）。
// 2026-11-09 三个具名模型档换代：GPT → GPT Image 2.6 Sunburst、Banana → Nano Banana 2.1、Seedream → Seedream 5.5
// （网关 Atlas 组）；同日全线下线 GPT Image 2，「通用」改为按张计费的 GPT Image 2.5（1K / 2K / 4K）。
// 2026-11-10 起应用侧不区分厂商 Dev 档：组码一律用规范组（img.nano-banana-2.1 / img.gpt-image-2.5-sunburst），
// 网关挑部署；旧 Dev 组码在网关里是弃用别名，已入库任务上的旧组码按 pkg/modelgroup 等价读。
var DefaultModelGroups = map[string]string{
	"通用":       modelgroup.GPTImage25Sunburst,
	"GPT":      "img.gpt-image-2.6-sunburst",
	"Banana":   modelgroup.NanoBanana21,
	"Seedream": "img.seedream-5.5",
	// GPT25 GPT Image 2.5（2026-11-09 新增）：按张计费，1K / 2K / 4K。旧档名 GPT25Dev 只作输入别名。
	"GPT25": modelgroup.GPTImage25Sunburst,
}

// legacyModelKeys 旧配置键 → 现档名（某些环境的 yaml 可能还写着旧键）。
var legacyModelKeys = map[string]string{"gpt25dev": "GPT25"}

// ModelGroupMap 生效的模型档映射。配置值按 pkg/modelgroup 归一为规范组码（yaml 里还写旧 Dev 组码也照常工作，
// 新提交冻结的是规范组）；旧键 GPT25Dev 映射为 GPT25（同时配了 GPT25 时以 GPT25 为准）。
func (c Config) ModelGroupMap() map[string]string {
	if len(c.ModelGroups) == 0 {
		return DefaultModelGroups
	}
	out := make(map[string]string, len(c.ModelGroups))
	for k, g := range c.ModelGroups {
		if nk, ok := legacyModelKeys[strings.ToLower(strings.TrimSpace(k))]; ok {
			k = nk
			if _, set := c.ModelGroups[nk]; set {
				continue
			}
		}
		out[k] = modelgroup.Canonical(g)
	}
	return out
}

// 编排名额（按 kind）的回退值。
const defaultImageGenBatchConcurrency = 4

// BatchSlots kind 的编排名额（tryon / pose / imagegen 各一个池子）。
func (c Config) BatchSlots(kind string) int {
	base := c.Poll.BatchConcurrency
	if base <= 0 {
		base = 2
	}
	k := c.Poll.KindBatchConcurrency
	switch kind {
	case "tryon":
		if k.Tryon > 0 {
			return k.Tryon
		}
	case "pose":
		if k.Pose > 0 {
			return k.Pose
		}
	case "imagegen":
		if k.ImageGen > 0 {
			return k.ImageGen
		}
		return max(base, defaultImageGenBatchConcurrency)
	}
	return base
}

// PollWorkers 长轮询名额：max(Workers, MaxRunning)（各自 <=0 按 64）。
func (c Config) PollWorkers() int {
	w, m := c.Poll.Workers, c.Poll.MaxRunning
	if w <= 0 {
		w = 64
	}
	if m <= 0 {
		m = 64
	}
	return max(w, m)
}
