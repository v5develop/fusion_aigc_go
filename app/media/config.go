package media

// Config 媒体服务配置。服务只做视频的封面、水印、合成三种无状态处理，不碰业务表与存储：
// 输入是公网地址，输出是字节流，由调用方自己落库、自己转存。
type Config struct {
	Listen string `json:",default=0.0.0.0:18090"`
	// Registry 注册中心：配置后启动时自注册，退出时注销（调用方据此发现实例）
	Registry RegistryConfig `json:",optional"`
	// Token 调用方必须在 X-Media-Token 头里带上；为空时服务拒绝启动（不做无鉴权的开放服务）
	Token string
	// FFmpeg / FFprobe 留空用 PATH 里的同名命令
	FFmpeg  string `json:",optional"`
	FFprobe string `json:",optional"`
	// LogoPath 水印 logo（PNG）；为空或文件不存在时水印接口返回 logo_unavailable
	LogoPath string `json:",optional"`
	// TempDir 工作目录根，留空用系统临时目录
	TempDir string `json:",optional"`
	// MaxInputBytes 单个输入视频的体积上限
	MaxInputBytes int64 `json:",default=209715200"` // 200M
	// MaxComposeInputs 合成一次最多拼几段
	MaxComposeInputs int `json:",default=50"`
	// MaxConcurrent 同时处理的请求数，超过返回 503 让调用方稍后重试
	MaxConcurrent int `json:",default=4"`
	// FetchTimeoutSeconds 单个输入下载的总时长上限
	FetchTimeoutSeconds int `json:",default=300"`
	// OpTimeoutSeconds 单次 ffmpeg 处理的时长上限
	OpTimeoutSeconds int `json:",default=900"`
	// AllowPrivateHosts 允许访问内网 / 回环地址。仅本地联调用，线上必须关闭（防 SSRF）
	AllowPrivateHosts bool `json:",optional"`
	// InputCacheMinutes 下载下来的输入视频最后一次使用后保留多久（分钟），期间同一 URL 不再重复下载
	InputCacheMinutes int `json:",default=5"`
	// InputCacheMaxBytes 输入缓存的总字节上限（单实例）。超过时淘汰空闲且最早过期的文件；正在使用的不删，可能暂时超出
	InputCacheMaxBytes int64 `json:",default=2147483648"` // 2G
	// FFmpegThreads 单个 ffmpeg 进程的线程数（与原 Java 的 FFMPEG_THREADS 同义）
	FFmpegThreads int `json:",default=2"`
	// SubtitleFontName / SubtitleFontSize 烧录字幕的样式（容器里需装对应的 CJK 字体）
	SubtitleFontName string `json:",optional"` // 留空用 Noto Sans CJK SC
	SubtitleFontSize int    `json:",default=12"`
}
