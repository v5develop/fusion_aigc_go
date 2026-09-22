package aigc

import (
	"errors"
	"fmt"
)

// 任务类型
const (
	TypeMotionDrive  = 9  // 动作驱动
	TypeNoteScript   = 74 // 图文笔记文案
	TypeNoteImages   = 75 // 图文笔记出图
	TypeLongImage    = 76 // 长图
	TypePoster       = 78 // 海报
	TypeDigitalHuman = 85 // AI 数字人
	TypeVoiceover    = 92 // 数字人口播

	// 路由后才会出现的类型，只在返回值里看到
	TypeDigitalHumanLite = 45 // 85 + model=lite
	TypeMotionDriveV2    = 86 // 9 + model 缺省/非 lite（云端 Seedance 2.0）
	TypeVoiceoverLite    = 91 // 92 + model=lite（本地 GPU）
)

// DigitalHuman 85 AI 数字人：图+音频→视频。产物 GeneratedImagePath（mp4）。
type DigitalHuman struct {
	// ImagePath / AudioPath 公网 URL；Model=lite 时须为 OSS key
	ImagePath   string `json:"image_path"`
	AudioPath   string `json:"audio_path"`
	AspectRatio string `json:"aspectRatio"` // 9:16 / 16:9
	Resolution  string `json:"resolution"`  // 480p / 720p / 1080p
	Duration    int    `json:"duration"`    // 秒，4–30
	// Model 默认 doubao-seedance-2-5-260628；lite 路由到 45（≤25s）
	Model         string `json:"model,omitempty"`
	Prompt        string `json:"prompt,omitempty"`
	GenerateAudio *bool  `json:"generate_audio,omitempty"`
}

func (DigitalHuman) TaskType() int { return TypeDigitalHuman }

func (t DigitalHuman) Validate() error {
	return first(
		required("image_path", t.ImagePath),
		required("audio_path", t.AudioPath),
		oneOf("aspectRatio", t.AspectRatio, "9:16", "16:9"),
		oneOf("resolution", t.Resolution, "480p", "720p", "1080p"),
		rangeInt("duration", t.Duration, 4, 30),
	)
}

// MotionDrive 9 动作驱动：参考视频+参考图→视频。产物 GeneratedImagePath（mp4）。
type MotionDrive struct {
	// InputVideo / ReferenceImage 公网 URL 或 OSS key；走 86 时必须是公网 URL
	InputVideo     string `json:"input_video"`
	ReferenceImage string `json:"reference_image"`
	// Model lite → 本地 GPU 的 9；缺省或其它值 → 路由到 86（云端 Seedance 2.0）
	Model  string `json:"model,omitempty"`
	Prompt string `json:"prompt,omitempty"` // 默认 跳舞
	// 以下走 86 时生效，默认 15 / 720p / 9:16
	Duration    int    `json:"duration,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	AspectRatio string `json:"aspectRatio,omitempty"`
}

func (MotionDrive) TaskType() int { return TypeMotionDrive }

func (t MotionDrive) Validate() error {
	return first(
		required("input_video", t.InputVideo),
		required("reference_image", t.ReferenceImage),
	)
}

// NoteScript 74 图文笔记文案（LLM）。产物在 Result.Script。
type NoteScript struct {
	UserText     string   `json:"user_text"`
	NoteType     string   `json:"note_type,omitempty"`
	MaterialURLs []string `json:"material_urls,omitempty"` // ≤4
	PersonURL    string   `json:"person_url,omitempty"`
	ImageCount   int      `json:"image_count,omitempty"` // 默认 1
}

func (NoteScript) TaskType() int { return TypeNoteScript }

func (t NoteScript) Validate() error {
	return first(
		required("user_text", t.UserText),
		maxLen("material_urls", len(t.MaterialURLs), 4),
	)
}

// NoteImages 75 图文笔记出图：每项 Images 出一张。产物 Result.ImagePaths，顺序与 Images 一致。
//
// 参考图字段按最新对接文档是 image_urls（内容即 74 返回的 images_url，转存到我们目录后再传）。
type NoteImages struct {
	Images    []NoteImage `json:"images"`
	ImageURLs []string    `json:"image_urls,omitempty"`
	Model     string      `json:"model,omitempty"` // mini / plus / max(默认)
	Size      string      `json:"size,omitempty"`  // 默认 3:4
}

type NoteImage struct {
	Index       int    `json:"index,omitempty"` // 74 返回的序号，原样透传
	ImagePrompt string `json:"image_prompt"`
	// ReferenceMaterials image_urls 的 1 起下标；空 = 纯文生图，最多 4 张
	ReferenceMaterials []int `json:"reference_materials"`
}

func (NoteImages) TaskType() int { return TypeNoteImages }

func (t NoteImages) Validate() error {
	if len(t.Images) == 0 {
		return errors.New("缺少 images")
	}
	for i, img := range t.Images {
		if img.ImagePrompt == "" {
			return fmt.Errorf("images[%d] 缺少 image_prompt", i)
		}
		if len(img.ReferenceMaterials) > 4 {
			return fmt.Errorf("images[%d].reference_materials 最多 4 张", i)
		}
		for _, n := range img.ReferenceMaterials {
			if n < 1 || n > len(t.ImageURLs) {
				return fmt.Errorf("images[%d].reference_materials 下标 %d 超出 image_urls 范围", i, n)
			}
		}
	}
	return nil
}

// LongImage 76 长图。产物 GeneratedImagePath。
type LongImage struct {
	UserText     string   `json:"user_text"`
	TemplateURL  string   `json:"template_url"` // 必填
	MaterialURLs []string `json:"material_urls,omitempty"`
	Size         string   `json:"size,omitempty"` // 默认 1:4
}

func (LongImage) TaskType() int { return TypeLongImage }

func (t LongImage) Validate() error {
	n := len(t.MaterialURLs)
	if t.TemplateURL != "" {
		n++
	}
	return first(
		required("user_text", t.UserText),
		required("template_url", t.TemplateURL),
		maxLen("template_url + material_urls", n, 4),
	)
}

// Poster 78 海报。产物 GeneratedImagePath。
//
// Materials 对接文档写的是必填（1–3 张），但设计稿里「上传素材」是选填（产品已确认），
// 因此这里不强制；待 AIGC 同事确认不传时的行为。
type Poster struct {
	Template    string   `json:"template"`
	Materials   []string `json:"materials,omitempty"` // 1–3 张，超出取前 3
	UserText    string   `json:"user_text,omitempty"`
	PosterStyle string   `json:"poster_style,omitempty"`
	PosterType  string   `json:"poster_type,omitempty"`
	Size        string   `json:"size,omitempty"`  // 默认 2:3
	Model       string   `json:"model,omitempty"` // mini / plus / max(默认)
}

func (Poster) TaskType() int { return TypePoster }

func (t Poster) Validate() error {
	return first(
		required("template", t.Template),
		maxLen("materials", len(t.Materials), 3),
	)
}

// Voiceover 92 数字人口播：口播文案 + 人物图（+ 场景图）→ 视频。产物 GeneratedImagePath（mp4）。
// 剧本（分段提示词）由执行面内部生成，不对外。
type Voiceover struct {
	Text        string `json:"text"`
	CharURL     string `json:"CHAR_01_url"`           // 人物图公网 URL（锁定人物形象）
	EnvURL      string `json:"ENV_01_url,omitempty"`  // 场景图公网 URL（锁定背景）；不传由模型自行匹配
	Duration    int    `json:"duration"`              // 15 / 30
	AspectRatio string `json:"aspectRatio,omitempty"` // 9:16（默认）/ 16:9
	Resolution  string `json:"resolution,omitempty"`  // 仅云端 92 生效，默认 720p
	// Model 缺省 / seedance2.5 → 云端 92；lite → 路由到 91（本地 GPU）
	Model         string `json:"model,omitempty"`
	GenerateAudio *bool  `json:"generate_audio,omitempty"` // 默认 true
}

func (Voiceover) TaskType() int { return TypeVoiceover }

func (t Voiceover) Validate() error {
	if t.Duration != 15 && t.Duration != 30 {
		return fmt.Errorf("duration 须为 15 或 30，当前 %d", t.Duration)
	}
	return first(
		required("text", t.Text),
		required("CHAR_01_url", t.CharURL),
	)
}

func first(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func required(name, v string) error {
	if v == "" {
		return fmt.Errorf("缺少 %s", name)
	}
	return nil
}

func oneOf(name, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s 须为 %v 之一，当前 %q", name, allowed, v)
}

func rangeInt(name string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s 须在 %d–%d 之间，当前 %d", name, lo, hi, v)
	}
	return nil
}

func maxLen(name string, n, max int) error {
	if n > max {
		return fmt.Errorf("%s 最多 %d 个，当前 %d", name, max, n)
	}
	return nil
}
