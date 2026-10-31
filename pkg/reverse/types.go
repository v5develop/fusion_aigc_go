package reverse

import "encoding/json"

// 以下类型按 tools/prompt-reverse/serve_reverse.py 的实际 JSON 建模（版本 2026-10-30.merged）。
// 图片字段一律是 data URL（data:image/...;base64,...）；sidecar 会静默丢弃非 data URL 的图，
// 本客户端在发送前就拒绝，避免下标错位。

// Status GET /api/status。
type Status struct {
	Service       string `json:"service"` // 固定 prompt-reverse
	Version       string `json:"version"`
	Mock          bool   `json:"mock"`
	LLMConfigured bool   `json:"llm_configured"` // sidecar 是否已配好 ai-gateway 地址与 secret（不返回凭据本身）
	Model         string `json:"model"`
	ModelLabel    string `json:"model_label"`
	Vision        bool   `json:"vision"` // 模型名含 flash 视为支持读图
}

// Usage DeepSeek usage 原样透传（字段随上游变化，按需 Decode）。
type Usage = json.RawMessage

// PoseMeta pose/source、pose/prompt 的 meta。
type PoseMeta struct {
	Model     string `json:"model"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Usage     Usage  `json:"usage,omitempty"`
}

// PoseSourceData POST /api/pose/source 返回的 source（_norm_source）。全部是字符串 / 字符串数组。
//
// 回传 pose/prompt 时用 PoseSource.Raw 原样发回（sidecar 会再走一遍 _norm_source），
// 不要用本结构体重新序列化，避免字段增减时信息丢失。
type PoseSourceData struct {
	HasPerson   bool     `json:"has_person"`
	GenderAge   string   `json:"gender_age"`
	Face        string   `json:"face"`
	Skin        string   `json:"skin"`
	Hair        string   `json:"hair"`
	Makeup      string   `json:"makeup"`
	Body        string   `json:"body"`
	Expression  string   `json:"expression"`
	Outfit      []string `json:"outfit"`
	Accessories []string `json:"accessories"`
	Props       string   `json:"props"`
	Scene       string   `json:"scene"`
	Background  string   `json:"background"`
	Light       string   `json:"light"`
	Camera      string   `json:"camera"`
	Framing     string   `json:"framing"` // 近景 / 半身 / 七分身 / 全身 / 空
	Style       string   `json:"style"`
	Pose        string   `json:"pose"`
	Note        string   `json:"note"`
}

// PoseSource POST /api/pose/source 的响应。Raw 是 source 对象的原始 JSON（缓存与回传都用它）。
type PoseSource struct {
	Source PoseSourceData  `json:"source"`
	Meta   PoseMeta        `json:"meta"`
	Raw    json.RawMessage `json:"-"`
}

// PoseData POST /api/pose/prompt 返回的 pose（_norm_pose）。
// 注意 orientation_deg 是五档中文字符串（正面0° / 前侧45° / 正侧90° / 背侧135° / 背面180°）或空串，不是数字；
// confidence 是 高 / 中 / 低。
type PoseData struct {
	HasPerson      bool   `json:"has_person"`
	Orientation    string `json:"orientation"`
	OrientationDeg string `json:"orientation_deg"`
	Facing         string `json:"facing"`
	Stance         string `json:"stance"`
	Gait           string `json:"gait"`
	Legs           string `json:"legs"`
	Arms           string `json:"arms"`
	Hands          string `json:"hands"`
	Spine          string `json:"spine"`
	Shoulders      string `json:"shoulders"`
	Head           string `json:"head"`
	FaceVisible    string `json:"face_visible"`
	NearSide       string `json:"near_side"`
	Gaze           string `json:"gaze"`
	Outline        string `json:"outline"`
	Brows          string `json:"brows"`
	Eyes           string `json:"eyes"`
	Mouth          string `json:"mouth"`
	Jaw            string `json:"jaw"`
	Expression     string `json:"expression"`
	Mood           string `json:"mood"`
	Dynamic        string `json:"dynamic"`
	Framing        string `json:"framing"`
	Occluded       string `json:"occluded"`
	Confidence     string `json:"confidence"`
	Note           string `json:"note"`
}

// PosePrompt POST /api/pose/prompt 的响应。
type PosePrompt struct {
	Prompt string   `json:"prompt"`
	Pose   PoseData `json:"pose"`
	Meta   PoseMeta `json:"meta"`
}

// PosePromptReq POST /api/pose/prompt 的请求体。Mode=short 走短版，其余长版（默认长版）。
type PosePromptReq struct {
	Source json.RawMessage `json:"source"`
	Image  string          `json:"image"`
	Notes  string          `json:"notes,omitempty"`
	Mode   string          `json:"mode,omitempty"`
}

// ---------------------------------------------------------------- analyze

// 反推图片角色（images[].role）。主图的 role 是单品中文名（上衣 / 下装 …）或「整套」。
const (
	RoleDetail = "局部细节"
	RoleModel  = "人物模特"
)

// AnalyzeImage analyze / analyze-batch 的 images[]。
type AnalyzeImage struct {
	Role  string `json:"role"`
	Label string `json:"label,omitempty"`
	Data  string `json:"data"`
}

// PaletteColor / Palette 可选的配色提示（palettes[]{role, colors[]{name,pct}}）。
type PaletteColor struct {
	Name string  `json:"name"`
	Pct  float64 `json:"pct"`
}

type Palette struct {
	Role   string         `json:"role,omitempty"`
	Colors []PaletteColor `json:"colors"`
}

// AnalyzeReq POST /api/reverse/analyze。
type AnalyzeReq struct {
	Category      string            `json:"category"`
	Items         []string          `json:"items,omitempty"`
	Images        []AnalyzeImage    `json:"images"`
	SetMode       string            `json:"set_mode,omitempty"` // one / two
	Fabric        string            `json:"fabric,omitempty"`   // 按件写，原样透传
	Notes         string            `json:"notes,omitempty"`
	LengthClass   string            `json:"length_class,omitempty"`
	SleeveClass   string            `json:"sleeve_class,omitempty"`
	Lengths       map[string]string `json:"lengths,omitempty"`
	Sleeves       map[string]string `json:"sleeves,omitempty"`
	LookLengths   map[string]string `json:"look_lengths,omitempty"`
	Palettes      []Palette         `json:"palettes,omitempty"`
	Deep          bool              `json:"deep,omitempty"`
	PromptVersion string            `json:"prompt_version,omitempty"` // new 走实验版，默认 old
	TargetModel   string            `json:"target_model,omitempty"`
}

// AnalyzeMeta analyze 的 meta。garment_match / garment_type 只在读图成功时出现。
// elapsed_ms 只在单个 analyze 的响应里有；analyze-batch 每项的 meta 没有。
type AnalyzeMeta struct {
	Category     string `json:"category"`
	Model        string `json:"model"`
	Degraded     bool   `json:"degraded"` // 读图失败降级为文字 / 模板
	Vision       bool   `json:"vision"`
	Images       int    `json:"images"`
	GarmentMatch *bool  `json:"garment_match,omitempty"`
	GarmentType  string `json:"garment_type,omitempty"`
	ElapsedMs    int64  `json:"elapsed_ms,omitempty"`
}

// AnalyzeResp POST /api/reverse/analyze 的响应（总是 200，失败走降级）。
type AnalyzeResp struct {
	Prompt  string      `json:"prompt"`
	Meta    AnalyzeMeta `json:"meta"`
	Warning string      `json:"warning"`
}

// BatchModel analyze-batch 的 models[]。
type BatchModel struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Data  string `json:"data"`
}

// AnalyzeBatchReq POST /api/reverse/analyze-batch：同一组服装按多个模特分别反推。
// images 里不要放 role=人物模特 的图（sidecar 会把它们从 base 里剔除；models 为空时才拿它们当模特）。
type AnalyzeBatchReq struct {
	AnalyzeReq
	Models      []BatchModel `json:"models"`
	Concurrency int          `json:"concurrency,omitempty"` // 1–6，默认 3
}

// AnalyzeBatchResult results[]。Index 是 models[] 下标；Error 非空表示该模特失败（不影响其它）。
// sidecar 线程异常时会给出 index=-1 的项。
type AnalyzeBatchResult struct {
	Index   int         `json:"index"`
	ModelID string      `json:"model_id"`
	Prompt  string      `json:"prompt"`
	Meta    AnalyzeMeta `json:"meta"`
	Warning string      `json:"warning"`
	Error   string      `json:"error,omitempty"`
}

type AnalyzeBatchMeta struct {
	Models        int    `json:"models"`
	Concurrency   int    `json:"concurrency"`
	PromptVersion string `json:"prompt_version"`
	TargetModel   string `json:"target_model"`
	ElapsedMs     int64  `json:"elapsed_ms"`
}

type AnalyzeBatchResp struct {
	Results []AnalyzeBatchResult `json:"results"`
	Meta    AnalyzeBatchMeta     `json:"meta"`
}

// ---------------------------------------------------------------- crop

// CropCandidate 候选细节框（客户端预先切好的候选表）。Box 为归一化 [x,y,w,h]。
type CropCandidate struct {
	ID  string     `json:"id"`
	Box [4]float64 `json:"box"`
}

// CropPlanImage crop-plan 的 images[]。Role 用单品中文名 / 英文 key / 「整套」；
// Angle 为用户手选角度（front/back/side/detail/flat 会覆盖模型判断）。
// Candidates + SheetData（候选表拼图的 data URL）只在候选表模式（look / 单张 compose）里用到。
type CropPlanImage struct {
	Data       string          `json:"data"`
	Angle      string          `json:"angle,omitempty"`
	Role       string          `json:"role,omitempty"`
	Candidates []CropCandidate `json:"candidates,omitempty"`
	SheetData  string          `json:"sheet_data,omitempty"`
}

// CropPlanReq POST /api/reverse/crop-plan。sidecar 只处理前 6 张，CropPlan 会自动分批。
type CropPlanReq struct {
	Category string          `json:"category"`
	Items    []string        `json:"items,omitempty"`
	Images   []CropPlanImage `json:"images"`
	Fabric   string          `json:"fabric,omitempty"`
	Notes    string          `json:"notes,omitempty"`
}

// Crop 一个细节框。Box 为归一化 [x,y,w,h]（sidecar 已裁到 0–1、w,h≥0.04、面积≤0.75，保留 4 位小数）。
// Label 在 per-item 模式下带「单品·」前缀；keypoints / 兜底模式不带（由编排补）。
type Crop struct {
	Label      string    `json:"label"`
	Box        []float64 `json:"box"`
	Confidence float64   `json:"confidence"`
}

// CropPlan plans[]。Index 在 CropPlan 返回值里已回映为原请求 images[] 的下标。
type CropPlan struct {
	Index      int       `json:"index"`
	Angle      string    `json:"angle"`
	GarmentBox []float64 `json:"garment_box,omitempty"`
	Crops      []Crop    `json:"crops"`
}

// crop-plan 的 meta.mode。sidecar 候选表模式不写 mode，客户端补成 ModeCandidates。
const (
	ModeKeypoints  = "keypoints"
	ModePerItem    = "per-item"
	ModeCandidates = "candidates"
)

type CropPlanMeta struct {
	Images    int    `json:"images"`
	Plans     int    `json:"plans"`
	Mode      string `json:"mode,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// CropPlanResp 合并后的响应。Meta 为汇总（images/plans 求和，elapsed_ms 取各批最大值，
// mode 各批去重后用逗号连接）；Batches 为每批原始 meta（顺序同分批）。
type CropPlanResp struct {
	Plans   []CropPlan     `json:"plans"`
	Meta    CropPlanMeta   `json:"meta"`
	Batches []CropPlanMeta `json:"batches,omitempty"`
}

// CropCheckItem crop-check 的 items[]。
type CropCheckItem struct {
	Data  string `json:"data"`
	Label string `json:"label"`
}

// CropCheckResult results[]。Index 在 CropCheck 返回值里已回映为原 items[] 下标。
// 模型漏判的项不会出现在结果里（视为通过）。
type CropCheckResult struct {
	Index      int     `json:"index"`
	OK         bool    `json:"ok"`
	Confidence float64 `json:"confidence"`
}

type cropCheckResp struct {
	Results []CropCheckResult `json:"results"`
	Meta    struct {
		Items     int   `json:"items"`
		ElapsedMs int64 `json:"elapsed_ms"`
	} `json:"meta"`
}
