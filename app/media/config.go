package media

import (
	"errors"
	"fmt"
	"strings"
)

// Config 媒体服务配置。服务只做视频的封面、水印、合成三种无状态处理，不碰业务表与存储：
// 输入是公网地址，输出是字节流，由调用方自己落库、自己转存。
//
// 默认值（json:",default=…"）与生产部署一致：deploy/docker/docker-compose.media.yml、
// deploy/docker/media.env.example、fusion-aigc 的 compose 都以同样的数值为默认。
// 只有 yaml 里完全不出现的键才会用这里的默认值；yaml 里的 ${VAR} 留空会得到空字符串，见 Validate。
type Config struct {
	// Listen 监听地址。必填：留空时 Go 会监听 :80（全部网卡），因此 Validate 拒绝空值
	Listen string `json:",default=0.0.0.0:18090"`
	// Registry 注册中心：配置后启动时自注册，退出时注销（调用方据此发现实例）
	Registry RegistryConfig `json:",optional"`
	// InstanceID 本实例的稳定 ID（6 位小写字母或数字），部署时生成并持久化，不随 IP 变化；资源 ID 嵌入它
	InstanceID string
	// IdentitySecret 派生资源 ID 与对象键的 HMAC 密钥（至少 32 字节）。丢失则已有资源的对象键无法重算，必须备份
	IdentitySecret string
	// Token 调用方必须在 X-Media-Token 头里带上；为空时服务拒绝启动（不做无鉴权的开放服务）
	Token string
	// OpsToken 运维定位接口（POST /v1/mediasvc/ops/locate）的令牌，与业务令牌分开；为空则该接口不开放。
	// 业务接口的响应不含对象键与桶信息；只有持这个令牌的运维能查到它们。
	OpsToken string `json:",optional"`
	// FFmpeg / FFprobe 留空用 PATH 里的同名命令
	FFmpeg  string `json:",optional"`
	FFprobe string `json:",optional"`
	// LogoPath 水印 logo（PNG）；为空或文件不存在时水印接口返回 logo_unavailable
	LogoPath string `json:",optional"`
	// TempDir 工作目录根，留空用系统临时目录
	TempDir string `json:",optional"`
	// OSS 对象存储（生产）。Enabled 为 true 时落桶走 OSS，否则走 ObjectDir 下的本地目录（只用于开发）
	OSS OSSConfig `json:",optional"`
	// ObjectDir 本地对象目录（OSS 未启用时的落桶位置，内容寻址）
	ObjectDir string `json:",default=./data/media-objects"`
	// MaxInputBytes 单个输入（下载或 purpose=user_input 的字节上传）的体积上限，默认 1 GiB（与 MEDIA_MAX_INPUT_BYTES 默认值一致）
	MaxInputBytes int64 `json:",default=1073741824"`
	// StoreLocalMaxBytes 我方产物经字节入口上传（purpose 非 user_input，如 Java 本地加工的文件）的体积上限。
	// 默认 2 GiB，与 Java 侧 GATEWAY_RESULT_MAX_BYTES 对齐；用户输入仍受 MaxInputBytes 约束。
	StoreLocalMaxBytes int64 `json:",default=2147483648"`
	// IngestHeaderHosts 允许接收调用方请求头（source.headers，如 Authorization）的素材主机，逗号分隔。
	// 默认为空：不向任何主机发送调用方的凭证，带 headers 的 ingest 一律拒绝（403）。只列素材站自己的主机。
	IngestHeaderHosts string `json:",optional"`
	// MaxSourcePixels 单个输入的像素上限（宽 × 高）：防解码炸弹。默认与 Java 侧 v1 的 SOURCE_MAX_PIXELS 一致（1 亿）
	MaxSourcePixels int64 `json:",default=100000000"`
	// MaxComposeInputs 合成一次最多拼几段
	MaxComposeInputs int `json:",default=50"`
	// 名额分三条通道，互不排队（见 lanes.go）：视频算力 / 轻算力 / I/O。排队超过 min(OpTimeoutSeconds, QueueWaitSeconds)
	// 返回 503 transient 让调用方稍后重试。三个值都为 0（不配）时按机器核数推导。
	//
	// VideoCompute 视频 ffmpeg（转码 / 合成 / 重构图 / 抽帧 / 抽音轨 / 视频解码检查）并发数。
	// 默认 max(4, 有效核数 / FFmpegThreads)（有效核数取 min(GOMAXPROCS, cgroup 配额)；不低于旧默认 4）。
	VideoCompute int `json:",optional"`
	// LightCompute 轻算力（ffprobe 与图片 ffmpeg / 像素处理）并发数。默认 有效核数 * 2。
	LightCompute int `json:",optional"`
	// LightThreads 轻通道里每个 ffmpeg 进程的线程数，默认 1（图片单帧处理多线程没有收益，且轻通道名额多，避免 CPU 超订）。
	LightThreads int `json:",optional"`
	// IO 下载、请求体读取、对象存储读写与服务端复制的并发数。默认 32。
	IO int `json:",optional"`
	// MaxConcurrent 旧配置项，向后兼容：仅当 VideoCompute 未配置时作为 VideoCompute 的别名；对 LightCompute / IO 不再有影响。
	MaxConcurrent int `json:",optional"`
	// QueueWaitSeconds 排队等名额的时长上限（秒），默认 120；实际取它与 OpTimeoutSeconds 的较小值。超时返回 503 transient
	QueueWaitSeconds int `json:",optional"`
	// FetchTimeoutSeconds 单个输入下载的总时长上限，默认 1800（生产值，大视频下载慢）。必须为正数：0 会让下载不设时限（Validate 拒绝）
	FetchTimeoutSeconds int `json:",default=1800"`
	// OpTimeoutSeconds 单次请求（下载之外的处理，含 ffmpeg 与校验）的时长上限，也是 deadline_ms 的上限，默认 1800（生产值）。
	// 必须为正数（Validate 拒绝）。调用方（Java app.media-service.timeout-seconds，默认 3600）必须大于它，推荐至少 2 倍：排队等名额也计在内
	OpTimeoutSeconds int `json:",default=1800"`
	// AllowPrivateHosts 允许访问内网 / 回环地址。仅本地联调用，线上必须关闭（防 SSRF）
	AllowPrivateHosts bool `json:",optional"`
	// InputCacheMinutes 下载下来的输入视频最后一次使用后保留多久（分钟），期间同一 URL 不再重复下载
	InputCacheMinutes int `json:",default=5"`
	// IdempotencyMinutes 写操作幂等结果的保留时长（分钟）。同一个 Idempotency-Key 在此期间重放得到同一结果
	IdempotencyMinutes int `json:",default=120"`
	// Idempotency 幂等记录的存储（Idempotency.Store: redis | memory，默认 redis，多实例共享）。见 idem_store.go
	Idempotency IdempotencyConfig `json:",optional"`
	// InputCacheMaxBytes 输入缓存的配置上限（单实例，默认 10 GiB）。实际上限还要不超过磁盘可用空间的 30%（见 effectiveCacheCap）。
	// 超过时淘汰空闲且最早过期的文件；正在使用的不删，可能暂时超出
	InputCacheMaxBytes int64 `json:",default=10737418240"` // 10 GiB
	// ObjectCacheMaxBytes 对象缓存（按 rid 缓存读到或刚写入的对象字节，见 object_cache.go）的配置上限（单实例，默认 10 GiB）。
	// 实际上限还要不超过磁盘可用空间的 20%；0 关闭对象缓存（每次按 rid 读取都从对象存储下载）
	ObjectCacheMaxBytes int64 `json:",default=10737418240"` // 10 GiB
	// ObjectCacheMinutes 对象缓存的条目最后一次使用后保留多久（分钟），默认 30；0 关闭对象缓存
	ObjectCacheMinutes int `json:",default=30"`
	// FFmpegThreads 单个 ffmpeg 进程的线程数（与原 Java 的 FFMPEG_THREADS 同义）
	FFmpegThreads int `json:",default=2"`
	// DeleteGraceMinutes 软删除的宽限期（分钟），默认 10080（7 天）。delete 只打待删除标记，宽限期内 restore 可撤销，
	// 过期后由清理任务（sweep.go）物理删除。0 表示下一轮清理即删，只用于测试，线上不要设。
	DeleteGraceMinutes int `json:",default=10080"`
	// SweepIntervalMinutes 清理任务的执行间隔（分钟），默认 60。每个实例都会跑，清理操作幂等，多实例并跑无害。
	SweepIntervalMinutes int `json:",default=60"`
	// SubtitleFontName / SubtitleFontSize 烧录字幕的样式（容器里需装对应的 CJK 字体）
	SubtitleFontName string `json:",optional"` // 留空用 Noto Sans CJK SC
	SubtitleFontSize int    `json:",default=12"`
}

// Validate 启动前校验：只查「留空或为 0 时会静默出错」且没有合法空值含义的项。
//
// 注意 go-zero v1.10.3 的行为：yaml 里 ${VAR} 展开为空时，int / bool 字段直接加载失败（type mismatch），
// 所以这里不重复检查它们；string 字段留空会得到 ""，这正是需要这里兜底的地方。
// 不在这里检查的项：IdentitySecret / InstanceID / Token 由 NewServer 校验；
// OSS 各桶由 NewOSSStore 校验；Registry.Advertise 留空按设计只跳过注册（不致命）；OpsToken / IngestHeaderHosts /
// LogoPath / Idempotency.Store（留空即 redis）/ Redis 密码留空都有明确的「关闭」含义。
//
// go-zero 的 conf.Load 会在解析后调用它，所以 cmd/main.go 的 MustLoad 即是启动时的拒绝点。
func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("media: Listen 为空（MEDIA_LISTEN 未设置）：空地址会监听 :80，拒绝启动")
	}
	if c.OpTimeoutSeconds <= 0 {
		return fmt.Errorf("media: OpTimeoutSeconds 必须为正数（当前 %d，对应 MEDIA_OP_TIMEOUT_SECONDS）：为 0 时每个请求都会立刻超时", c.OpTimeoutSeconds)
	}
	if c.FetchTimeoutSeconds <= 0 {
		return fmt.Errorf("media: FetchTimeoutSeconds 必须为正数（当前 %d，对应 MEDIA_FETCH_TIMEOUT_SECONDS）：为 0 时下载不设时限", c.FetchTimeoutSeconds)
	}
	return nil
}
