package media

// 契约类型（app/media/api/media.api）。字段名即线上 JSON 字段，不要改。

// SourceRef 来源：url 与 key 二选一。
// SourceRef 输入来源，三选一：rid（系统内资源，首选）、url（外部入口）、key+store（历史路径，只读兼容）。
type SourceRef struct {
	Rid   string `json:"rid,omitempty"`
	Url   string `json:"url,omitempty"`
	Key   string `json:"key,omitempty"`
	Store string `json:"store,omitempty"`
	// Headers 下载请求头，仅 ingest 的 url 来源可用（白名单见 api_headers.go）。
	Headers map[string]string `json:"headers,omitempty"`
}

// RuleSpec 参考视频规整规则，字段与网关 VideoConformance 对齐。
type RuleSpec struct {
	StripAudio bool    `json:"strip_audio"`
	TargetFps  int     `json:"target_fps"`
	MinSide    int     `json:"min_side"`
	MaxSide    int     `json:"max_side"`
	MinPixels  int64   `json:"min_pixels"`
	MaxPixels  int64   `json:"max_pixels"`
	MinRatio   float64 `json:"min_ratio"`
	MaxRatio   float64 `json:"max_ratio"`
}

// ObjectRef 落桶后的对象引用。
type ObjectRef struct {
	RID       string  `json:"rid"`
	Class     string  `json:"class"`
	Store     string  `json:"store"`
	Key       string  `json:"key,omitempty"` // 仅内部与运维使用，业务响应不返回
	Sha256    string  `json:"sha256"`
	Size      int64   `json:"size"`
	Mime      string  `json:"mime"`
	DurationS float64 `json:"duration_s"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Fps       float64 `json:"fps"`
	HasAudio  bool    `json:"has_audio"`
}

// URLRef 可访问地址。
type URLRef struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// MediaResult 写操作统一结果。
type MediaResult struct {
	Object  ObjectRef `json:"object"`
	Url     URLRef    `json:"url"`
	Parents []string  `json:"parents,omitempty"`
}

// MediaMeta probe 结果（不落桶）。
type MediaMeta struct {
	Mime      string  `json:"mime"`
	Size      int64   `json:"size"`
	DurationS float64 `json:"duration_s"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Fps       float64 `json:"fps"`
	HasAudio  bool    `json:"has_audio"`
}

// FrameItem 一帧：时间点与落桶后的 JPEG 对象。
type FrameItem struct {
	TimestampS float64   `json:"timestamp_s"`
	Object     ObjectRef `json:"object"`
}

// FramesResp 抽帧结果。
type FramesResp struct {
	Frames []FrameItem `json:"frames"`
}

type ingestReq struct {
	Source  SourceRef `json:"source"`
	Purpose string    `json:"purpose"`
}

type probeReq struct {
	Source SourceRef `json:"source"`
}

type framesReq struct {
	Source SourceRef `json:"source"`
	Count  int       `json:"count"`
}

type coverReq struct {
	Source  SourceRef `json:"source"`
	Purpose string    `json:"purpose"`
}

type watermarkReq struct {
	Source  SourceRef `json:"source"`
	Purpose string    `json:"purpose"`
}

type trimReq struct {
	Source  SourceRef `json:"source"`
	Seconds float64   `json:"seconds"`
	Purpose string    `json:"purpose"`
}

type conformReq struct {
	Source  SourceRef `json:"source"`
	Rule    RuleSpec  `json:"rule"`
	Seconds float64   `json:"seconds"`
	Purpose string    `json:"purpose"`
}

// imageRuleReq 图片规整规则（与契约 ImageRule 对齐）。
type imageRuleReq struct {
	MinLongSide int      `json:"min_long_side"`
	MaxLongSide int      `json:"max_long_side"`
	MimeTypes   []string `json:"mime_types"`
}

type imageConformReq struct {
	Source  SourceRef    `json:"source"`
	Rule    imageRuleReq `json:"rule"`
	Purpose string       `json:"purpose"`
}

// imageResizeReq 精确尺寸缩放（契约 ImageResizeReq）。
type imageResizeReq struct {
	Source  SourceRef `json:"source"`
	Width   int       `json:"width"`
	Height  int       `json:"height"`
	Format  string    `json:"format"`
	Purpose string    `json:"purpose"`
}

type composeReq struct {
	Sources []SourceRef `json:"sources"`
	Purpose string      `json:"purpose"`
}

type extractAudioReq struct {
	Source  SourceRef `json:"source"`
	Purpose string    `json:"purpose"`
}

type adjustDurationReq struct {
	Source  SourceRef `json:"source"`
	Factor  float64   `json:"factor"`
	Purpose string    `json:"purpose"`
}

type deleteReq struct {
	Rid    string    `json:"rid,omitempty"`
	Object ObjectRef `json:"object"`
}

// deleteResp 删除结果。state: pending_delete 已进入待删除（宽限期内可 restore 撤销）；
// absent 对象不存在，什么也没做。deleted 为 true 表示对象已不可再读（待删除或已删除）。
type deleteResp struct {
	Deleted     bool   `json:"deleted"`
	State       string `json:"state"`
	DeleteAfter string `json:"delete_after,omitempty"`
}

// retentionDeleteReq 用户输入保留期删除：只收 user 类别的 rid。响应沿用 deleteResp。
type retentionDeleteReq struct {
	Rid string `json:"rid"`
}

type restoreReq struct {
	Rid string `json:"rid"`
}

// restoreResp restored=true 表示已撤销待删除；false 表示对象本来就不在待删除状态（幂等）。
type restoreResp struct {
	Rid      string `json:"rid"`
	Restored bool   `json:"restored"`
}
