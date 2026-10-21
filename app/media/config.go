package media

// Config 媒体服务配置。服务只做视频的封面、水印、合成三种无状态处理，不碰业务表与存储：
// 输入是公网地址，输出是字节流，由调用方自己落库、自己转存。
type Config struct {
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
	// MaxInputBytes 单个输入视频的体积上限
	// OSS 对象存储（生产）。Enabled 为 true 时落桶走 OSS，否则走 ObjectDir 下的本地目录（只用于开发）
	OSS OSSConfig `json:",optional"`
	// ObjectDir 本地对象目录（OSS 未启用时的落桶位置，内容寻址）
	ObjectDir     string `json:",default=./data/media-objects"`
	MaxInputBytes int64  `json:",default=209715200"` // 200M
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
	// MaxConcurrent 同时执行的计算（ffmpeg / ffprobe / 图像运算）数；下载与上传另有输入名额（4 倍）。
	// 排队超过 min(OpTimeoutSeconds, 120s) 返回 503 让调用方稍后重试
	MaxConcurrent int `json:",default=4"`
	// FetchTimeoutSeconds 单个输入下载的总时长上限
	FetchTimeoutSeconds int `json:",default=300"`
	// OpTimeoutSeconds 单次 ffmpeg 处理的时长上限
	OpTimeoutSeconds int `json:",default=900"`
	// AllowPrivateHosts 允许访问内网 / 回环地址。仅本地联调用，线上必须关闭（防 SSRF）
	AllowPrivateHosts bool `json:",optional"`
	// InputCacheMinutes 下载下来的输入视频最后一次使用后保留多久（分钟），期间同一 URL 不再重复下载
	InputCacheMinutes int `json:",default=5"`
	// IdempotencyMinutes 写操作幂等结果的保留时长（分钟）。同一个 Idempotency-Key 在此期间重放得到同一结果
	IdempotencyMinutes int `json:",default=120"`
	// InputCacheMaxBytes 输入缓存的配置上限（单实例，默认 10 GiB）。实际上限还要不超过磁盘可用空间的 30%（见 effectiveCacheCap）。
	// 超过时淘汰空闲且最早过期的文件；正在使用的不删，可能暂时超出
	InputCacheMaxBytes int64 `json:",default=10737418240"` // 10 GiB
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
