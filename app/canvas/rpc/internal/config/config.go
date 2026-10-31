package config

import (
	"fmt"
	"strings"

	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/storage"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库（afv_canvas* / afv_prompt_library_item 新表 + 计费 / 任务中心 / 素材库共享表）
	DataSource string
	// Billing 计费：off 不计费（默认）/ shadow 只算价 / on 真扣。只有 gateway 后端（image / video / tool_i2p）由 Go 计费，
	// 每个 node run 一张预留单（RequestID = canvas-<task_id>）；Java 兼容工具与 studio 试穿由对方计费，Go 不 Hold。
	Billing billing.Conf
	// Storage 须与网关一致（目前只用于识别本服务签发的地址，保留与 studio 一致的配置面）
	Storage storage.Conf `json:",optional"`
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
		BaseURL        string `json:",optional"`
		KeyID          string `json:",optional"`
		Secret         string `json:",optional"`
		LongPollWait   int    `json:",default=20"`
		TimeoutSeconds int    `json:",default=30"`
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
		IntervalSeconds int `json:",default=3"`
		BatchSize       int `json:",default=100"`
		// MaxSubmitAttempts 提交后端遇暂时性错误（429 / 5xx / 网络）时最多尝试几次，之后置失败退款
		MaxSubmitAttempts int `json:",default=5"`
	}
	// Timeouts 各类 run 的总时限（秒），超时置失败（gateway 后端退款并取消调用；studio 取消批次；Java 调取消接口由 Java 退款，无有效登录态时退化为停止跟踪）
	Timeouts struct {
		Image int `json:",default=300"`
		Video int `json:",default=900"`
		Tool  int `json:",default=600"`
		I2P   int `json:",default=180"`
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
	Model   string   // 显示名，= 节点 data.model（Image / Banana / SD5P / Seedance 2.5 …）
	ModelID string   // = data.modelId（image-v1 / banana-v1 / sd5p-v1 / seedance-2-5 …）
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
var videoRatios = []string{"16:9", "9:16", "1:1"}

// DefaultModels Models 没配时的模型表（原型 CanvasModelNode 的四个图片 / 四个视频档）。
var DefaultModels = []ModelSpec{
	{Model: "Image", ModelID: "image-v1", Aliases: []string{"GPT", "GPT Image 2", "gpt-image-2", "通用", "general"}, Kind: "image",
		ModelGroup: "img.gpt-image-2", MaxImageRefs: 16, Ratios: imageRatios, Sizes: []string{"1K", "2K", "4K"},
		Qualities: []string{"low", "medium", "high"}, CountMax: 4},
	{Model: "Banana", ModelID: "banana-v1", Aliases: []string{"Nano Banana", "Nano Banana Pro", "nano-banana", "nano-banana-2.1"}, Kind: "image",
		Badge: "超强模型", ModelGroup: "img.nano-banana-2.1", MaxImageRefs: 14, Ratios: imageRatios, Sizes: []string{"1K", "2K"}, CountMax: 4},
	{Model: "SD5P", ModelID: "sd5p-v1", Aliases: []string{"Seedream", "Seedream 5 Pro", "seedream-5-pro"}, Kind: "image",
		ModelGroup: "img.seedream-5-pro", MaxImageRefs: 10, Ratios: imageRatios, Sizes: []string{"2K"}, CountMax: 4}, // 1K 非 1:1 会被拒、4K 静默降 2K：只给 2K
	{Model: "Seedance 2.5", ModelID: "seedance-2-5", Aliases: []string{"dreamina-seedance-2-5"}, Kind: "video",
		ModelGroup: "vid.seedance-2.5", MaxImageRefs: 3, MaxReferenceVideos: 1, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
	{Model: "Seedance 2.0", ModelID: "seedance-2-0", Aliases: []string{"dreamina-seedance-2-0", "Seedance 2"}, Kind: "video",
		ModelGroup: "vid.seedance-2", MaxImageRefs: 3, MaxReferenceVideos: 1, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
	{Model: "Seedance Fast", ModelID: "seedance-2-0-fast", Aliases: []string{"dreamina-seedance-2-0-fast", "Seedance 2.0 Fast"}, Kind: "video",
		ModelGroup: "vid.seedance-2-fast", MaxImageRefs: 3, MaxReferenceVideos: 1, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
	{Model: "Seedance Mini", ModelID: "seedance-2-0-mini", Aliases: []string{"dreamina-seedance-2-0-mini", "Seedance 2.0 Mini"}, Kind: "video",
		ModelGroup: "vid.seedance-2-mini", MaxImageRefs: 2, Ratios: videoRatios, Sizes: []string{"720p", "1080p"},
		Durations: []string{"4s", "6s", "8s"}, Sound: true, CountMax: 1},
}

// ModelTable 生效的模型表。
func (c Config) ModelTable() []ModelSpec {
	if len(c.Models) > 0 {
		return c.Models
	}
	return DefaultModels
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
			if strings.EqualFold(m.ModelID, k) || strings.EqualFold(m.Model, k) || strings.EqualFold(m.ModelGroup, k) {
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
