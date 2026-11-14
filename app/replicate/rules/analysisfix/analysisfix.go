// Package analysisfix 分析结果纠偏规则（纯函数、表驱动）。
//
// 分析阶段（台词 / 画面理解 / 镜头切分）的模型输出有几类已知的常见错误：时间起止颠倒、整体时长估错（时间轴被压缩或拉长）、
// 关键帧之间编出看不到的镜头、镜头数多于关键帧、转写重复、「有人声」却一句台词也没有 / 有台词却标成无声。
// 这里把它们做成一张规则表（Table），在读取分析结果时（core.ResolveAnalysisWith，对落库的画面理解原文重新规范化、夹紧时间之前）
// 按表顺序逐条应用，每条被纠偏的项记一条 Correction 放进读出的结果（corrections），后台详情可见、用户视图去掉。
//
// 可配置：afv_system_config.replicate.analysis_rules（JSON，见 Config）可关掉某条规则或调阈值。
// 版本：Version 是规则表的代码版本；VersionOf(cfg) = Version（默认配置）或 Version+"+"+配置指纹，写进读出结果的 rulesVersion。
// 不进分析缓存键（source_key）：规则或配置一变，已有分析在下一次读取时按新规则套用，不重新分析、不调网关。
//
// 本包不依赖 core（core 依赖它）：输入输出是本包自己的 Doc，core 负责与 AnalysisResult 互转。
package analysisfix

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Version 规则表版本：改了任何规则的判据 / 纠偏方式、增删规则、调默认阈值都要升这个值（读出结果的 rulesVersion 据此区分）。
const Version = "fix-2026-11-14.1"

// 规则码（Correction.Rule；Config.Disabled 用它关规则）。
const (
	RuleTimeSwapped       = "TIME_SWAPPED"         // 起止颠倒（t1 < t0）→ 交换
	RuleDurationScale     = "DURATION_SCALE"       // 画面理解的时间轴整体比原片长 / 短 → 按原片时长等比缩放
	RuleShotNoKeyframe    = "SHOT_NO_KEYFRAME"     // 镜头里一张关键帧都没有（关键帧之间编出来的）→ 并入相邻镜头
	RuleShotOverFrames    = "SHOT_OVER_FRAMES"     // 镜头数多于关键帧数 → 合并最短的镜头直到不多于关键帧
	RuleLineDuplicate     = "LINE_DUPLICATE"       // 相邻两句台词文字相同且时间相接 → 合并
	RuleSpeechNoLines     = "SPEECH_NO_LINES"      // 标了有人声（speech / mixed），但转写跑过且一句台词也没有 → 改为只有音乐、台词项置无
	RuleLinePresentEmpty  = "LINE_PRESENT_EMPTY"   // 台词项标了「有」，但没有任何台词也没有描述 → 台词项置无
	RuleLinesNoSpeechMark = "LINES_NO_SPEECH_MARK" // 转写出了台词，却标成无声 / 只有音乐 → 改为有人声（silent→speech，music→mixed）
)

// 分析阶段（规则归属，后台按它分组展示）：台词 / 画面理解 / 镜头切分。
const (
	StageLines      = "lines"
	StageUnderstand = "understand"
	StageShots      = "shots"
)

// Span 一个带时间的项（分镜之外的：服装逐套出现时段、画面文字等），只参与交换 / 缩放。
type Span struct {
	T0 float64
	T1 float64
}

// Section 一个镜头（分镜）。
type Section struct {
	T0, T1               float64
	Desc, Action, Camera string
}

// Line 一句台词。
type Line struct {
	T0, T1 float64
	Text   string
}

// Doc 待纠偏的分析结果（时间为模型 / 转写原值，未夹紧）。
type Doc struct {
	DurationS float64   // 原片时长（探测值；0 = 未知，时长类规则跳过）
	HasAudio  bool      // 原片有没有音轨
	Frames    []float64 // 关键帧时间点（秒，升序）；画面理解只看到这些帧
	// TranscriptRan 台词转写跑完且有结果（哪怕 0 句）；LinesFromTranscript 台词取自转写（否则取自画面理解的猜测）
	TranscriptRan       bool
	LinesFromTranscript bool
	AudioKind           string // silent / speech / music / mixed
	LinePresent         bool   // 台词项（dims.line.present）
	LineDesc            string // 台词项描述
	Sections            []Section
	Lines               []Line
	Others              []Span // 其它带时间的项，顺序由调用方保持，用于写回
}

// Correction 一条纠偏记录（写进分析结果，后台查看）。
type Correction struct {
	Rule   string `json:"rule"`
	Stage  string `json:"stage"`
	Field  string `json:"field"`           // sections / lines / audioKind / dims.line / timeline
	Count  int    `json:"count,omitempty"` // 涉及几处
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Note   string `json:"note"`
}

// Config 可配置项（afv_system_config.replicate.analysis_rules，JSON）。零值 = 默认。
type Config struct {
	// Disabled 关掉的规则码
	Disabled []string `json:"disabled,omitempty"`
	// DurationTolerance 时间轴与原片时长的相对误差超过它才缩放（默认 0.15，范围 0.05~0.5）
	DurationTolerance float64 `json:"durationTolerance,omitempty"`
	// DurationMinDriftS 绝对误差至少这么多秒才缩放（默认 1.0，范围 0.2~10）
	DurationMinDriftS float64 `json:"durationMinDriftS,omitempty"`
	// FrameSlackS 判断「镜头里有没有关键帧」时边界的容差秒数（默认 0.05，范围 0~1）
	FrameSlackS float64 `json:"frameSlackS,omitempty"`
	// LineJoinGapS 相同台词相隔多久以内算重复（默认 0.3，范围 0~2）
	LineJoinGapS float64 `json:"lineJoinGapS,omitempty"`
}

const (
	defDurationTolerance = 0.15
	defDurationMinDriftS = 1.0
	defFrameSlackS       = 0.05
	defLineJoinGapS      = 0.3
)

// Normalize 补默认值（越界的回默认），Disabled 去重排序。
func (c Config) Normalize() Config {
	fix := func(v, lo, hi, def float64) float64 {
		if v == 0 || math.IsNaN(v) || v < lo || v > hi {
			return def
		}
		return v
	}
	c.DurationTolerance = fix(c.DurationTolerance, 0.05, 0.5, defDurationTolerance)
	c.DurationMinDriftS = fix(c.DurationMinDriftS, 0.2, 10, defDurationMinDriftS)
	if c.FrameSlackS < 0 || c.FrameSlackS > 1 || math.IsNaN(c.FrameSlackS) || c.FrameSlackS == 0 {
		c.FrameSlackS = defFrameSlackS
	}
	if c.LineJoinGapS < 0 || c.LineJoinGapS > 2 || math.IsNaN(c.LineJoinGapS) || c.LineJoinGapS == 0 {
		c.LineJoinGapS = defLineJoinGapS
	}
	seen := map[string]bool{}
	var ds []string
	for _, d := range c.Disabled {
		d = strings.ToUpper(strings.TrimSpace(d))
		if d != "" && !seen[d] {
			seen[d] = true
			ds = append(ds, d)
		}
	}
	sort.Strings(ds)
	c.Disabled = ds
	return c
}

func (c Config) disabled(code string) bool {
	for _, d := range c.Disabled {
		if d == code {
			return true
		}
	}
	return false
}

// ParseConfig 解析运行参数（空串 = 默认）。未知规则码 / 非法 JSON 报错（调用方告警并沿用默认）。
func ParseConfig(raw string) (Config, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Config{}.Normalize(), nil
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, fmt.Errorf("不是合法的 JSON: %w", err)
	}
	for _, d := range c.Disabled {
		if !Known(strings.ToUpper(strings.TrimSpace(d))) {
			return Config{}, fmt.Errorf("未知规则码 %q", d)
		}
	}
	return c.Normalize(), nil
}

// VersionOf 规则版本（写进读出结果的 rulesVersion）：默认配置 = Version；否则 Version+"+"+配置指纹前 8 位。
func VersionOf(c Config) string {
	n := c.Normalize()
	if isDefault(n) {
		return Version
	}
	b, _ := json.Marshal(n)
	sum := sha256.Sum256(b)
	return Version + "+" + hex.EncodeToString(sum[:4])
}

func isDefault(n Config) bool {
	return len(n.Disabled) == 0 && n.DurationTolerance == defDurationTolerance && n.DurationMinDriftS == defDurationMinDriftS &&
		n.FrameSlackS == defFrameSlackS && n.LineJoinGapS == defLineJoinGapS
}

// Rule 规则表的一行。
type Rule struct {
	Code  string
	Stage string
	Desc  string // 后台 / 文档用的一句话
	Apply func(d *Doc, c Config) []Correction
}

// Table 规则表（顺序即应用顺序：先修时间，再修镜头，最后修台词与声音标记）。
var Table = []Rule{
	{RuleTimeSwapped, StageUnderstand, "起止颠倒（t1<t0）的时间交换回来", applyTimeSwapped},
	{RuleDurationScale, StageUnderstand, "画面理解的时间轴整体比原片长或短，按原片时长等比缩放", applyDurationScale},
	{RuleShotNoKeyframe, StageShots, "一张关键帧都不含的镜头（关键帧之间推断不出的切点）并入相邻镜头", applyShotNoKeyframe},
	{RuleShotOverFrames, StageShots, "镜头数多于关键帧数时合并最短的镜头", applyShotOverFrames},
	{RuleLineDuplicate, StageLines, "相邻两句文字相同、时间相接的台词合并为一句", applyLineDuplicate},
	{RuleSpeechNoLines, StageLines, "标了有人声但转写一句台词也没有：改为只有音乐、台词项置无", applySpeechNoLines},
	{RuleLinePresentEmpty, StageLines, "台词项标了有，但既没有台词也没有描述：台词项置无", applyLinePresentEmpty},
	{RuleLinesNoSpeechMark, StageLines, "转写出了台词却标成无声 / 只有音乐：改为有人声", applyLinesNoSpeechMark},
}

// Known 是否是规则表里的规则码。
func Known(code string) bool {
	for _, r := range Table {
		if r.Code == code {
			return true
		}
	}
	return false
}

// Apply 按表顺序应用未关闭的规则，原地修改 d，返回纠偏记录（没有纠偏返回 nil）。
func Apply(d *Doc, c Config) []Correction {
	if d == nil {
		return nil
	}
	c = c.Normalize()
	var out []Correction
	for _, r := range Table {
		if c.disabled(r.Code) {
			continue
		}
		for _, x := range r.Apply(d, c) {
			x.Rule, x.Stage = r.Code, r.Stage
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------- 时间

func applyTimeSwapped(d *Doc, _ Config) []Correction {
	var out []Correction
	n := 0
	for i := range d.Sections {
		if d.Sections[i].T1 < d.Sections[i].T0 {
			d.Sections[i].T0, d.Sections[i].T1 = d.Sections[i].T1, d.Sections[i].T0
			n++
		}
	}
	if n > 0 {
		out = append(out, Correction{Field: "sections", Count: n, Note: fmt.Sprintf("%d 个镜头的起止时间颠倒，已交换", n)})
	}
	n = 0
	for i := range d.Lines {
		if d.Lines[i].T1 < d.Lines[i].T0 {
			d.Lines[i].T0, d.Lines[i].T1 = d.Lines[i].T1, d.Lines[i].T0
			n++
		}
	}
	if n > 0 {
		out = append(out, Correction{Field: "lines", Count: n, Note: fmt.Sprintf("%d 句台词的起止时间颠倒，已交换", n)})
	}
	n = 0
	for i := range d.Others {
		if d.Others[i].T1 < d.Others[i].T0 {
			d.Others[i].T0, d.Others[i].T1 = d.Others[i].T1, d.Others[i].T0
			n++
		}
	}
	if n > 0 {
		out = append(out, Correction{Field: "timeline", Count: n, Note: fmt.Sprintf("%d 处其它时段（服装出现 / 画面文字）的起止颠倒，已交换", n)})
	}
	return out
}

// applyDurationScale 画面理解按关键帧估时间，常把时间轴整体压短（以为视频更短）或拉长（超出原片的部分会被夹紧成 0 长度而丢掉）。
// 以镜头的最晚结束时间为「模型以为的时长」，与原片时长的误差同时超过相对容差与绝对下限时，把画面理解给出的全部时间等比缩放到原片时长。
// 偏长时只在「有整个镜头落在原片之外」（某个镜头的开始 ≥ 原片时长，夹紧后会整段消失）才缩放；只是最后一个镜头的结束写超了，
// 夹紧就够了（缩放反而会把前面正确的切点全部挪走）。台词取自转写时不动台词（转写时间可信）。
func applyDurationScale(d *Doc, c Config) []Correction {
	if d.DurationS <= 0 || len(d.Sections) == 0 {
		return nil
	}
	end, lastStart := 0.0, 0.0
	for _, s := range d.Sections {
		end = math.Max(end, s.T1)
		lastStart = math.Max(lastStart, s.T0)
	}
	if end <= 0 {
		return nil
	}
	drift := math.Abs(end - d.DurationS)
	if drift <= c.DurationTolerance*d.DurationS || drift < c.DurationMinDriftS {
		return nil
	}
	if end > d.DurationS && lastStart < d.DurationS {
		return nil
	}
	k := d.DurationS / end
	sc := func(t float64) float64 { return math.Round(t*k*100) / 100 }
	for i := range d.Sections {
		d.Sections[i].T0, d.Sections[i].T1 = sc(d.Sections[i].T0), sc(d.Sections[i].T1)
	}
	for i := range d.Others {
		d.Others[i].T0, d.Others[i].T1 = sc(d.Others[i].T0), sc(d.Others[i].T1)
	}
	if !d.LinesFromTranscript {
		for i := range d.Lines {
			d.Lines[i].T0, d.Lines[i].T1 = sc(d.Lines[i].T0), sc(d.Lines[i].T1)
		}
	}
	return []Correction{{Field: "timeline", Before: fmt.Sprintf("%.1f 秒", end), After: fmt.Sprintf("%.1f 秒", d.DurationS),
		Note: fmt.Sprintf("画面理解的时间轴是 %.1f 秒，原片 %.1f 秒，已按 %.2f 倍等比缩放", end, d.DurationS, k)}}
}

// ---------------------------------------------------------------- 镜头

// sortSections 按开始时间排序（稳定），镜头类规则都在有序的镜头上做。
func sortSections(d *Doc) {
	sort.SliceStable(d.Sections, func(i, j int) bool { return d.Sections[i].T0 < d.Sections[j].T0 })
}

// mergeInto 把第 i 个镜头并入相邻镜头：有前一个并入前一个（延长其结束时间），否则并入后一个（提前其开始时间）。
func mergeInto(secs []Section, i int) []Section {
	if len(secs) < 2 {
		return secs
	}
	if i > 0 {
		secs[i-1].T1 = math.Max(secs[i-1].T1, secs[i].T1)
	} else {
		secs[1].T0 = math.Min(secs[1].T0, secs[0].T0)
	}
	return append(secs[:i], secs[i+1:]...)
}

// applyShotNoKeyframe 画面理解只看得到关键帧：两张相邻关键帧之间最多推断出一个切点。一个镜头的时段里一张关键帧都没有，
// 说明它是凭空编出来的（或切点放错了），并入相邻镜头。至少 2 张关键帧、2 个镜头才判。
func applyShotNoKeyframe(d *Doc, c Config) []Correction {
	if len(d.Frames) < 2 || len(d.Sections) < 2 {
		return nil
	}
	sortSections(d)
	before := len(d.Sections)
	has := func(s Section) bool {
		for _, f := range d.Frames {
			if f >= s.T0-c.FrameSlackS && f <= s.T1+c.FrameSlackS {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(d.Sections) && len(d.Sections) > 1; {
		if s := d.Sections[i]; s.T1 > s.T0 && !has(s) {
			d.Sections = mergeInto(d.Sections, i)
			continue
		}
		i++
	}
	if n := before - len(d.Sections); n > 0 {
		return []Correction{{Field: "sections", Count: n, Before: fmt.Sprintf("%d 个镜头", before), After: fmt.Sprintf("%d 个镜头", len(d.Sections)),
			Note: fmt.Sprintf("%d 个镜头里没有任何关键帧（关键帧之间看不到切换），已并入相邻镜头", n)}}
	}
	return nil
}

// applyShotOverFrames 镜头数不应多于关键帧数：多出来的从最短的镜头开始合并。
func applyShotOverFrames(d *Doc, _ Config) []Correction {
	if len(d.Frames) == 0 || len(d.Sections) <= len(d.Frames) {
		return nil
	}
	sortSections(d)
	before := len(d.Sections)
	for len(d.Sections) > len(d.Frames) {
		short := 0
		for i, s := range d.Sections {
			if s.T1-s.T0 < d.Sections[short].T1-d.Sections[short].T0 {
				short = i
			}
		}
		d.Sections = mergeInto(d.Sections, short)
	}
	return []Correction{{Field: "sections", Count: before - len(d.Sections), Before: fmt.Sprintf("%d 个镜头", before),
		After: fmt.Sprintf("%d 个镜头", len(d.Sections)), Note: fmt.Sprintf("镜头数（%d）多于关键帧数（%d），已合并最短的镜头", before, len(d.Frames))}}
}

// ---------------------------------------------------------------- 台词与声音标记

func applyLineDuplicate(d *Doc, c Config) []Correction {
	if len(d.Lines) < 2 {
		return nil
	}
	before := len(d.Lines)
	out := d.Lines[:1]
	for _, l := range d.Lines[1:] {
		p := &out[len(out)-1]
		if t := strings.TrimSpace(l.Text); t != "" && t == strings.TrimSpace(p.Text) && l.T0 <= p.T1+c.LineJoinGapS {
			p.T1 = math.Max(p.T1, l.T1)
			continue
		}
		out = append(out, l)
	}
	d.Lines = out
	if n := before - len(d.Lines); n > 0 {
		return []Correction{{Field: "lines", Count: n, Before: fmt.Sprintf("%d 句", before), After: fmt.Sprintf("%d 句", len(d.Lines)),
			Note: fmt.Sprintf("%d 句台词与上一句重复，已合并", n)}}
	}
	return nil
}

func hasLines(d *Doc) bool {
	for _, l := range d.Lines {
		if strings.TrimSpace(l.Text) != "" {
			return true
		}
	}
	return false
}

// applySpeechNoLines 关键帧形态下画面理解听不到声音，speech / mixed 只是猜测；转写跑过且一句台词都没有时以转写为准。
func applySpeechNoLines(d *Doc, _ Config) []Correction {
	if !d.TranscriptRan || hasLines(d) || (d.AudioKind != "speech" && d.AudioKind != "mixed") {
		return nil
	}
	before := d.AudioKind
	d.AudioKind = "music"
	if !d.HasAudio {
		d.AudioKind = "silent"
	}
	out := []Correction{{Field: "audioKind", Before: before, After: d.AudioKind, Note: "标了有人声，但转写一句台词也没有，按转写改正"}}
	if d.LinePresent {
		d.LinePresent, d.LineDesc = false, ""
		out = append(out, Correction{Field: "dims.line", Before: "有", After: "无", Note: "没有任何台词，台词项置为无"})
	}
	return out
}

func applyLinePresentEmpty(d *Doc, _ Config) []Correction {
	if !d.LinePresent || hasLines(d) || strings.TrimSpace(d.LineDesc) != "" {
		return nil
	}
	d.LinePresent = false
	return []Correction{{Field: "dims.line", Before: "有", After: "无", Note: "台词项标了有，但没有任何台词和描述，置为无"}}
}

func applyLinesNoSpeechMark(d *Doc, _ Config) []Correction {
	if !d.HasAudio || !d.LinesFromTranscript || !hasLines(d) {
		return nil
	}
	var to string
	switch d.AudioKind {
	case "silent", "":
		to = "speech"
	case "music":
		to = "mixed"
	default:
		return nil
	}
	before := d.AudioKind
	d.AudioKind = to
	return []Correction{{Field: "audioKind", Before: before, After: to, Note: "转写出了台词，声音类型改为有人声"}}
}
