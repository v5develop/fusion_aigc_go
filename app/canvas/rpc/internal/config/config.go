package config

import (
	"aigc-local-life/pkg/dbpool"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/modelgroup"
	"aigc-local-life/pkg/modellabel"
	"aigc-local-life/pkg/storage"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库（afv_canvas* / afv_prompt_library_item 新表 + 计费 / 任务中心 / 素材库共享表）
	DataSource string
	// DB 连接池上限（可选；不配用 pkg/dbpool 的服务默认值，见 docs/api/db-pool.md）
	DB dbpool.Config `json:",optional"`
	// Billing 计费：off 不计费（默认）/ shadow 只算价 / on 真扣。只有 gateway 后端（image / video / tool_i2p）由 Go 计费，
	// 每个 node run 一张预留单（RequestID = canvas-<task_id>）；Java 兼容工具与 studio 试穿由对方计费，Go 不 Hold。
	Billing billing.Conf
	// Storage 须与网关一致（目前只用于识别本服务签发的地址，保留与 studio 一致的配置面）
	Storage storage.Conf `json:",optional"`
	// FreeWatermark 免费档水印视图（mrw://）读时签名：Process 与 studio / assetlib / Java 同一个 OSS 处理串，
	// 只允许 [A-Za-z0-9_./,-]；没配或不合法时水印视图输出为空（绝不签干净原件）。
	FreeWatermark FreeWatermarkConf `json:",optional"`
	// Asset 成品写进素材库（afv_asset）：每个候选一行。EntryCode 按 kind 取 EntryCodes
	Asset asset.Conf
	// EntryCodes 入口码：计费 offer、素材库分类、任务中心 entry_code 三处同名（Go_V1.0.25 seed，audit.go Targets 已登记）
	EntryCodes struct {
		Image string `json:",default=canvas_image"`
		Video string `json:",default=canvas_video"`
		Crop  string `json:",default=canvas_tool_crop"`
		I2P   string `json:",default=canvas_tool_i2p"`
	}
	// TaskIDPrefix node run 任务号与任务中心坐标前缀（只能字母数字）；与现有来源不重复
	TaskIDPrefix string `json:",default=cv"`
	// AiGateway 生图 / 生视频 / 反推（/v1/calls，HMAC，调用方 fusion-go）。BaseURL 为空时 image / video / i2p 返回「暂未开放」
	AiGateway struct {
		BaseURL string `json:",optional"`
		KeyID   string `json:",optional"`
		Secret  string `json:",optional"`
		// LongPollWait 运行中 gateway run 的长轮询（GET /v1/calls/{id}?wait=）每次挂起的秒数（1–60，网关上限 60）
		LongPollWait   int `json:",default=20"`
		TimeoutSeconds int `json:",default=30"` // 单次 HTTP 超时（不含长轮询等待）
	}
	// Media 共享媒体服务：裁剪（image/crop）、产物转存素材库桶（resources/copy）、Java 结果与蒙版拉入（ingest）。
	// 配了 NacosServerAddr 订阅实例，BaseURL 只作回退；都留空则裁剪 / 重绘不可用，产物不转存（直接用上游地址，只用于开发）。
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
	// Java 主项目（fusion-aigc）的内网地址：调 creaft v1 兼容工具（/api/v1/tools/ai-image-upscaler 等）、
	// 任务查询（/api/v1/tasks/{taskUid}）与价目（/api/v1/features/*-model-configs），透传用户 Bearer。BaseURL 为空时三个 Java 工具返回「暂未开放」
	Java struct {
		BaseURL string `json:",optional"`
		Timeout int    `json:",default=30"` // 秒，单次 HTTP
	}
	// StudioRpc 试穿走 studio 商拍链路（SubmitTryon / GetBatch）。不配则试穿返回「暂未开放」
	StudioRpc zrpc.RpcClientConf `json:",optional"`
	// Models 画布模型表（显示名 / id → gateway model group、参考图上限、可选项、单价）。不配用 DefaultModels
	Models []ModelSpec `json:",optional"`
	// I2P 反推提示词：gateway chat.completion 的多模态模型组（须 supports_vision）
	I2P struct {
		ModelGroup string `json:",default=vlm.qwen-3.8-flash"`
		MaxTokens  int    `json:",default=1200"`
		Points     int64  `json:",default=0"` // catalog 展示用单价（实扣以计费为准）
	}
	Poll struct {
		// IntervalSeconds 兜底扫描间隔。运行中 gateway run 的完成检测主路径是每个 run 一个长轮询协程（Watchers），
		// 扫描只兜底：排队重提、Java / studio 轮询、长轮询协程没接住的 run（名额满 / 进程重启 / 租约被接管）
		IntervalSeconds int `json:",default=3"`
		BatchSize       int `json:",default=100"`
		// Watchers 同时长轮询 gateway 的 run 数（全进程）。名额满时新 run 交给扫描兜底（batchGet，慢一个扫描间隔）
		Watchers int `json:",default=64"`
		// LookupConcurrency 扫描里 Java 任务 / studio 批次查询的并发度（原先逐个串行）
		LookupConcurrency int `json:",default=8"`
		// FanoutConcurrency 单个 run 内的扇出并发度：参考图拉入媒体服务、N 张 gateway 提交、N 张产物转存素材库
		FanoutConcurrency int `json:",default=4"`
		// MaxSubmitAttempts 提交后端遇暂时性错误（429 / 5xx / 网络）时最多尝试几次，之后置失败退款
		MaxSubmitAttempts int `json:",default=5"`
		// JavaStopBackoffSeconds 停止 Java 任务（取消 / 找回任务号 / Poller 场景缺令牌）暂时失败后的重试退避（秒）：
		// 第 n 次失败后等第 n 项，用完仍失败则退化为停止跟踪。不配用 15,30,60,120,240（约 8 分钟）
		JavaStopBackoffSeconds []int `json:",optional"`
	}
	// Library 产物转存素材库（resources/copy）移出交付路径：run 先以私有引用 mr://{rid}.{ext} 置成功、结算、对用户可见，
	// 转存在后台按 outbox（afv_canvas_copy_job，Go_V1.0.29）完成，完成后把 run 的引用换成素材库地址并入素材库。
	Library struct {
		// AsyncCopy 关掉则回到同步转存（全部转存完才置成功）。落 outbox 失败（如表还没建）时本次自动退回同步转存
		AsyncCopy bool `json:",default=true"`
		// Workers 本进程同时执行的转存任务数（即时转存 + 扫描接手共用）
		Workers int `json:",default=8"`
		// MaxAttempts 最多尝试几轮，之后记永久失败（产物仍以 mr:// 可看可下载，library_status=failed）
		MaxAttempts int `json:",default=10"`
		// BackoffSeconds 第 n 轮失败后等第 n 项秒数（超出取最后一项）。不配用 5,15,30,60,120,300,600,1800（累计约 50 分钟）
		BackoffSeconds []int `json:",optional"`
		// ClaimSeconds 认领一轮的时长：这么久没写回结果视为执行方崩溃，由扫描接手（同幂等键重做，不会重复落桶）
		ClaimSeconds int `json:",default=300"`
		// SweepBatch 每轮扫描最多接手几行
		SweepBatch int `json:",default=50"`
		// RetryCooldownSeconds 用户「重新存入素材库」的冷却：转存任务进永久失败不足这么多秒时不允许再次重置（每 run 约每分钟至多一次）
		RetryCooldownSeconds int `json:",default=60"`
	}
	// Timeouts 各类 run 的总时限（秒），超时置失败（gateway 后端退款并取消调用；studio 取消批次；Java 调取消接口由 Java 退款，无有效登录态时退化为停止跟踪）
	//
	// gateway 后端（image / video / i2p）另有硬上限 *HardCap（秒，从 run 创建算起）：到了软时限时，只要还有调用已被供应商受理
	// （受理后供应商照常扣费、取消也省不下钱），就不取消、继续等到硬上限；没有受理的调用按软时限照旧失败退款并取消。
	// 硬上限小于软时限时按软时限；网关调用的 deadline_at = 提交时刻 + 硬上限，网关不会先于我们把调用判超时。
	Timeouts struct {
		Image int `json:",default=300"`
		Video int `json:",default=900"`
		Tool  int `json:",default=600"`
		I2P   int `json:",default=180"`
		// ImageHardCap / VideoHardCap / I2PHardCap gateway 调用已受理时最多等到 run 创建后这么久（秒）
		ImageHardCap int `json:",default=900"`
		VideoHardCap int `json:",default=1800"`
		I2PHardCap   int `json:",default=600"`
	}
	Limits struct {
		MaxSnapshotBytes  int `json:",default=2097152"` // 2MB
		MaxNodes          int `json:",default=2000"`
		MaxPromptLen      int `json:",default=3000"`
		MaxPromptsPerUser int `json:",default=500"`
		MaxTitleLen       int `json:",default=100"`
	}
	// FrameModelGroup 带首尾帧（image1 / image2）的视频改用的模型组：vid.seedance-2.5 不支持首尾帧（gateway 审计 10 号文件）
	FrameModelGroup string `json:",default=vid.seedance-2"`
	// MaxCostCNY 每次 gateway 调用的费用软上限（limits.max_cost，人民币十进制字符串）
	MaxCostCNY struct {
		Image string `json:",default=5.00"`
		Video string `json:",default=60.00"`
		I2P   string `json:",default=1.00"`
	}
}

// CheckHold 启动自检：Timeouts 各项（秒）须小于对应的预留时长（Billing.Hold，分钟；video 用 VideoMinutes，其余用 DefaultMinutes）。
// 否则 run 还没超时预留单就可能先被回收器收走，成功后结算 ErrGone（续期只是兜底：进程停摆时预留单仍会按 Hold 到期）。
func (c Config) CheckHold() error {
	def, video := c.Billing.Hold.DefaultMinutes, c.Billing.Hold.VideoMinutes
	if def <= 0 {
		def = 30 // 与 billing.New 的兜底一致
	}
	if video <= 0 {
		video = 200
	}
	var bad []string
	for _, it := range []struct {
		name    string
		sec     int
		holdMin int
		holdKey string
	}{
		{"Timeouts.Image", c.Timeouts.Image, def, "Billing.Hold.DefaultMinutes"},
		{"Timeouts.I2P", c.Timeouts.I2P, def, "Billing.Hold.DefaultMinutes"},
		{"Timeouts.Tool", c.Timeouts.Tool, def, "Billing.Hold.DefaultMinutes"},
		{"Timeouts.Video", c.Timeouts.Video, video, "Billing.Hold.VideoMinutes"},
		{"Timeouts.ImageHardCap", c.Timeouts.ImageHardCap, def, "Billing.Hold.DefaultMinutes"},
		{"Timeouts.I2PHardCap", c.Timeouts.I2PHardCap, def, "Billing.Hold.DefaultMinutes"},
		{"Timeouts.VideoHardCap", c.Timeouts.VideoHardCap, video, "Billing.Hold.VideoMinutes"},
	} {
		if it.sec >= it.holdMin*60 {
			bad = append(bad, fmt.Sprintf("%s=%ds 不小于 %s=%d 分钟（%ds）", it.name, it.sec, it.holdKey, it.holdMin, it.holdMin*60))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("canvas 配置自检失败：%s；请调小 Timeouts 或调大 Billing.Hold", strings.Join(bad, "；"))
	}
	return nil
}

// PriceSpec catalog 展示用单价（实扣以计费为准）。
type PriceSpec struct {
	Size     string `json:",optional"`
	Duration string `json:",optional"`
	Points   int64
}

// ModelSpec 画布一个模型档。
type ModelSpec struct {
	Model   string // wire 值，= 节点 data.model（Image / Banana / SD5P / SD55 / Seedance 2.5 …）；不是给用户看的名字
	ModelID string // = data.modelId（image-v1 / banana-v1 / sd5p-v1 / seedream-5-5-v1 / seedance-2-5 …）
	// Label 用户可见的完整模型名（GPT Image 2.5 / Nano Banana 2.1 …）；空时按模型组 / 模型码经 pkg/modellabel 推导（见 DisplayName）
	Label   string   `json:",optional"`
	Aliases []string `json:",optional"` // 其它叫法（GPT / Nano Banana Pro / Seedream / 通用 …），大小写不敏感
	Kind    string   `json:",default=image,options=image|video"`
	Badge   string   `json:",optional"`
	// ModelGroup ai-gateway model group 码
	ModelGroup string
	// MaxImageRefs 参考图上限（配置值）；gateway 模型组有 feature_limits.max_reference_images 时以 gateway 为准（取小）
	MaxImageRefs       int         `json:",optional"`
	MaxReferenceVideos int         `json:",optional"`
	Ratios             []string    `json:",optional"`
	Sizes              []string    `json:",optional"`
	Qualities          []string    `json:",optional"`
	Durations          []string    `json:",optional"`
	Sound              bool        `json:",optional"`
	CountMax           int         `json:",default=4"`
	Prices             []PriceSpec `json:",optional"`
}

var imageRatios = []string{"1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2", "4:5", "5:4", "21:9"}

// seedream55Ratios Seedream 5.5（Atlas）只有 8 个比例的预设尺寸，没有 4:5 / 5:4（网关会拒）。
var seedream55Ratios = []string{"1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2", "21:9"}
var videoRatios = []string{"16:9", "9:16", "1:1"}

// DefaultModels Models 没配时的模型表（原型 CanvasModelNode 的四个图片 / 四个视频档）。顺序 = 目录 / 下拉顺序。
// 2026-11-09 图片三档换代（与 studio 同口径，GPT Image 2 全线下线）：
// Image → GPT Image 2.5（GPT Image 2 的接替，按张计费）、新增 GPT Image 2.6 档、Banana → Nano Banana 2.1。
// 2026-11-10 Seedream 5.5 独立成档（SD55，排在 Seedream 5.0 Pro 前），SD5P 回到 Seedream 5.0 Pro（img.seedream-5-pro，只出 2K）。
// 用户端一律显示 Label（完整模型名），Model 只是 wire 值。
// 2026-11-10 起应用侧不区分厂商 Dev 档：组码用规范组（pkg/modelgroup），部署由网关挑；旧 Dev 名只作输入别名。
// model / model_id 不变（已保存的画布节点照常解析），旧名留作别名；已落库的 run 记录各自的 model_group，不受影响。
var DefaultModels = []ModelSpec{
	// 「Image」（原 GPT Image 2 档）2026-11-09 起接 GPT Image 2.5：按张计费，
	// 1K / 2K / 4K；没有 quality 参数；参考图不计费（上限取网关 16）。model / model_id 不变，已保存的节点照常解析。
	// 别名里的 GPT25Dev / *Sunburst Developer / *-developer 是旧名，永久兼容旧草稿，不对外输出。
	{Model: "Image", ModelID: "image-v1", Label: "GPT Image 2.5", Aliases: []string{"GPT", "GPT Image 2.5", "GPT25", "gpt-image-2.5-sunburst", "gpt-image-2.5",
		"GPT25Dev", "GPT Image 2.5 Sunburst Developer", "gpt-image-2.5-sunburst-developer",
		"GPT Image 2", "gpt-image-2", "通用", "general"}, Kind: "image",
		ModelGroup: modelgroup.GPTImage25Sunburst, MaxImageRefs: 16, Ratios: imageRatios, Sizes: []string{"1K", "2K", "4K"}, CountMax: 4},
	// GPT Image 2.6（Sunburst）作为附加档：1K / 2K / 4K、可选质量。参考图上限 14（网关 16）：画布按 high + 2 张参考图的价收，
	// 编辑每多一张 2048² 参考图成本约 $0.056，14 张以内 1K 仍有 ≥30% 毛利（Java AtlasImagePricingMarginTest），16 张会亏。
	{Model: "GPT Image 2.6", ModelID: "gpt-image-2-6-v1", Label: "GPT Image 2.6", Aliases: []string{"GPT Image 2.6 Sunburst", "gpt-image-2.6-sunburst",
		"gpt-image-2.6", "GPT26"}, Kind: "image",
		ModelGroup: "img.gpt-image-2.6-sunburst", MaxImageRefs: 14, Ratios: imageRatios, Sizes: []string{"1K", "2K", "4K"},
		Qualities: []string{"low", "medium", "high"}, CountMax: 4},
	// 别名里的 Nano Banana 2.1 Dev / nano-banana-2.1-dev 是旧名，永久兼容旧草稿。
	{Model: "Banana", ModelID: "banana-v1", Label: "Nano Banana 2.1", Aliases: []string{"Nano Banana", "Nano Banana 2.1", "nano-banana-2.1",
		"Nano Banana 2.1 Dev", "nano-banana-2.1-dev", "Nano Banana Pro", "nano-banana"}, Kind: "image",
		Badge: "超强模型", ModelGroup: modelgroup.NanoBanana21, MaxImageRefs: 14, Ratios: imageRatios, Sizes: []string{"1K", "2K"}, CountMax: 4},
	// Seedream 5.5（Atlas）：1K / 2K / 4K（4K 落 Atlas 3K 最大档）、8 个比例、参考图 ≤10（网关 feature_limits 为准）。
	// 计费：组码 img.seedream-5.5 命中 canvas_image_seedream_5_5 档位（Java V1.0.215，载体 creaft-seedream-5.5）。
	// 「Seedream」别名跟 studio 的 Seedream 档同口径（= 5.5）。
	{Model: "SD55", ModelID: "seedream-5-5-v1", Label: "Seedream 5.5", Aliases: []string{"Seedream 5.5", "seedream-5.5", "seedream-5-5",
		"Seedream55", "Seedream"}, Kind: "image",
		ModelGroup: "img.seedream-5.5", MaxImageRefs: 10, Ratios: seedream55Ratios, Sizes: []string{"1K", "2K", "4K"}, CountMax: 4},
	// Seedream 5.0 Pro：1K 非 1:1 会被拒、4K 静默降 2K，只给 2K（受理时 normalizeImageSize 统一按 2K）。
	// 计费：组码 img.seedream-5-pro 命中 canvas_image_seedream_5_pro 档位（Java V1.0.224，载体 canvas-seedream-5-pro，68 点 / 张）；
	// 该档位未建的环境 pickVariant 退回 canvas_image_standard。已保存的 SD5P 节点若是 1K / 4K，受理时按 2K 归一并回告警。
	{Model: "SD5P", ModelID: "sd5p-v1", Label: "Seedream 5.0 Pro", Aliases: []string{"Seedream 5.0 Pro", "Seedream 5 Pro", "seedream-5-pro"}, Kind: "image",
		ModelGroup: "img.seedream-5-pro", MaxImageRefs: 10, Ratios: imageRatios, Sizes: []string{"2K"}, CountMax: 4},
	{Model: "Seedance 2.5", ModelID: "seedance-2-5", Label: "Seedance 2.5", Aliases: []string{"dreamina-seedance-2-5"}, Kind: "video",
		ModelGroup: "vid.seedance-2.5", MaxImageRefs: 3, MaxReferenceVideos: 1, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
	{Model: "Seedance 2.0", ModelID: "seedance-2-0", Label: "Seedance 2.0", Aliases: []string{"dreamina-seedance-2-0", "Seedance 2"}, Kind: "video",
		ModelGroup: "vid.seedance-2", MaxImageRefs: 3, MaxReferenceVideos: 1, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
	{Model: "Seedance Fast", ModelID: "seedance-2-0-fast", Label: "Seedance 2.0 Fast", Aliases: []string{"dreamina-seedance-2-0-fast", "Seedance 2.0 Fast"}, Kind: "video",
		ModelGroup: "vid.seedance-2-fast", MaxImageRefs: 3, MaxReferenceVideos: 1, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
	{Model: "Seedance Mini", ModelID: "seedance-2-0-mini", Label: "Seedance 2.0 Mini", Aliases: []string{"dreamina-seedance-2-0-mini", "Seedance 2.0 Mini"}, Kind: "video",
		ModelGroup: "vid.seedance-2-mini", MaxImageRefs: 2, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
}

// DisplayName 用户可见的完整模型名：Label > 模型组登记名 > 模型码映射（pkg/modellabel）。不会返回 Image / Banana / SD5P 这类 wire 值。
func (m ModelSpec) DisplayName() string {
	if l := strings.TrimSpace(m.Label); l != "" {
		return l
	}
	if l, ok := modellabel.Registered(m.ModelGroup); ok {
		return l
	}
	if l := modellabel.Of(m.ModelID); l != "" && !strings.EqualFold(l, m.ModelID) {
		return l
	}
	return modellabel.Of(m.Model)
}

// ModelTable 生效的模型表。
// 配置的 Models 里组码按 pkg/modelgroup 归一为规范组（yaml 还写旧 Dev 组码也照常工作，新 run 冻结规范组）。
func (c Config) ModelTable() []ModelSpec {
	if len(c.Models) == 0 {
		return DefaultModels
	}
	out := make([]ModelSpec, len(c.Models))
	for i, m := range c.Models {
		m.ModelGroup = modelgroup.Canonical(m.ModelGroup)
		out[i] = m
	}
	return out
}

// FindModel 按 model / model_id / 别名找模型档（大小写不敏感；model_id 优先）。kind 非空时只在该类里找。
func (c Config) FindModel(model, modelID, kind string) (ModelSpec, bool) {
	keys := []string{strings.TrimSpace(modelID), strings.TrimSpace(model)}
	for _, k := range keys {
		if k == "" {
			continue
		}
		for _, m := range c.ModelTable() {
			if kind != "" && m.Kind != kind {
				continue
			}
			if strings.EqualFold(m.ModelID, k) || strings.EqualFold(m.Model, k) || (m.ModelGroup != "" && modelgroup.Equal(m.ModelGroup, k)) {
				return m, true
			}
			for _, a := range m.Aliases {
				if strings.EqualFold(a, k) {
					return m, true
				}
			}
		}
	}
	return ModelSpec{}, false
}

// LongPollWait 长轮询每次挂起的时长（1–60 秒，<=0 按 20 秒）。
func (c Config) LongPollWait() time.Duration {
	s := c.AiGateway.LongPollWait
	if s <= 0 {
		s = 20
	}
	return time.Duration(min(s, 60)) * time.Second
}

// FreeWatermarkConf 免费档水印读时签名配置。
type FreeWatermarkConf struct {
	Process          string `json:",optional"`
	SignedTTLSeconds int    `json:",default=600"`
}

// ValidProcess 处理串可用（非空且只含 [A-Za-z0-9_./,-]）。
func (c FreeWatermarkConf) ValidProcess() bool {
	if c.Process == "" {
		return false
	}
	for _, r := range c.Process {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("_./,-", r)) {
			return false
		}
	}
	return true
}

// SignedTTL 水印地址签名有效期（最少 60 秒，默认 600 秒）。
func (c FreeWatermarkConf) SignedTTL() time.Duration {
	s := c.SignedTTLSeconds
	if s <= 0 {
		s = 600
	}
	return time.Duration(max(60, s)) * time.Second
}
