package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 换声音 / 改台词（CONTRACT §P）：annotations.audio=revoice + annotations.voice。
//
// 用原片的台词（分析的 ASR 台词与时间）按所选音色重新配音，可逐句改台词（把商品名 / 价格改成自己的），
// 成片拼接完成后（同「保留原声」的位置）逐句调网关 tts.synthesize，把每句铺到原句在成片时间轴上的起点，
// 合成一条音轨再替换成片音轨。配音免费，每人每天 N 条（确认时占用，失败退回）；任何一步失败都回落到未换声的成片，不拦交付。
//
// 台词原文不进模型上下文（H6）：智能体只知道「几句、每句字数上限」，用户在表单里看原句并逐句填写。
// ---------------------------------------------------------------------------------------------

// 换声音相关的常量。
const (
	// AudioRevoice 声音策略：换个声音（用原片台词按所选音色重新配音）
	AudioRevoice = "revoice"
	// AudioResultRevoiced ResultDoc.Audio：已换成新配音（失败仍用 AudioResultFailed）
	AudioResultRevoiced = "revoiced"

	// ReasonVoiceQuotaExhausted 今天的换声音条数用完（确认时返回，此时还没动钱）
	ReasonVoiceQuotaExhausted = "VOICE_QUOTA_EXHAUSTED"
	// reasonVoiceUnavailable 音色不存在 / 已停用 / 换声音没开放
	reasonVoiceUnavailable = "VOICE_UNAVAILABLE"

	voiceCharsPerSec  = 5.0 // 每秒约 5 个字：每句字数上限 = ceil(该句秒数 × 5)
	voiceMinSpeed     = 0.5
	voiceMaxSpeed     = 1.5
	voiceMaxSpeedUp   = 1.3  // 句子比时间窗长时语速最多再上调到 1.3 倍
	voiceFitTolerance = 0.05 // 秒：比时间窗长不到这么多算放得下
	voiceMaxLines     = 80   // 一条视频最多配音几句（超过的分析不开放换声音）
	voiceMinSlotSec   = 0.3
)

// VoiceEdit annotations.voice：音色、整体语速与逐句改台词。未用时 JSON 里不出现，plan_hash 与旧版一致。
type VoiceEdit struct {
	// VoiceID 音色编号（replicate_list_voices 返回的 voiceId）
	VoiceID int64 `json:"voiceId"`
	// Speed 整体语速（0.5~1.5，缺省 = 1.0 正常语速）
	Speed float64 `json:"speed,omitempty"`
	// Lines 逐句改台词：index 从 1 起（与分析结果 lines 的顺序一致）；没列出的句沿用原片台词
	Lines []VoiceLineEdit `json:"lines,omitempty"`
}

// VoiceLineEdit 改一句台词。
type VoiceLineEdit struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// VoiceJob 确认时冻结进 job.input 的换声音参数（之后改音色目录 / 配置都不影响在途任务）。
type VoiceJob struct {
	VoiceID    int64       `json:"voiceId"`
	Voice      string      `json:"voice"` // 网关 tts 的音色参数值
	Name       string      `json:"name,omitempty"`
	Speed      float64     `json:"speed,omitempty"`
	ModelGroup string      `json:"modelGroup"`
	Lines      []VoiceLine `json:"lines"`
}

// VoiceLine 一句要合成的台词：原句在原片上的时间窗与最终要说的文字（改过取改后的，没改取原句）。
type VoiceLine struct {
	Index int     `json:"index"`
	T0    float64 `json:"t0"`
	T1    float64 `json:"t1"`
	Text  string  `json:"text"`
}

// VoiceInfo 音色目录里的一个音色（生产实现读 afv_digital_human_voice，只读复用；只返回启用的）。
type VoiceInfo struct {
	ID         int64
	Name       string
	Voice      string // 网关 tts 的音色参数值，不对用户展示
	Gender     string // female / male
	Style      string
	PreviewURL string
}

// VoiceCatalog 音色目录。
type VoiceCatalog interface {
	// ListVoices 启用中的音色（按运营排序）。
	ListVoices(ctx context.Context) ([]VoiceInfo, error)
	// GetVoice 一个启用中的音色；不存在 / 已停用 / 已删除返回 (nil, nil)。
	GetVoice(ctx context.Context, id int64) (*VoiceInfo, error)
}

// VoiceStore 换声音的额度占用与退回（生产实现 app/replicate/repo；不装配时换声音不可用）。
type VoiceStore interface {
	// AcquireVoiceQuota 给任务占一次当日额度：同一个任务重复调用算占到（不重复 +1）；已用 ≥ total 返回 ok=false。
	AcquireVoiceQuota(ctx context.Context, userID, jobID int64, day string, total int) (used int, ok bool, err error)
	// ReleaseVoiceQuota 退回任务占用的那一次（只退一次；没占过 / 已退过返回 false）。
	ReleaseVoiceQuota(ctx context.Context, jobID int64) (released bool, err error)
	VoiceQuotaUsed(ctx context.Context, userID int64, day string) (int, error)
}

// WithVoice 装配换声音（额度流水 + 音色目录）。不装配时方案里选换声音会得到 VOICE_UNAVAILABLE。
func WithVoice(store VoiceStore, cat VoiceCatalog) Option {
	return func(s *Service) { s.voices, s.voiceCat = store, cat }
}

// VoiceMixer 合成器的换声音能力：把若干句配音铺成一条音轨（*mediasvc.Client 满足），再用 AudioMuxer.MuxAudio 替换成片音轨。
type VoiceMixer interface {
	AudioMuxer
	AudioTimeline(ctx context.Context, clips []mediasvc.AudioClip, total float64, idem string) (mediasvc.Resource, error)
}

// voiceLineLimit 一句台词的字数上限：ceil(该句秒数 × 5)，至少 1。
func voiceLineLimit(t0, t1 float64) int {
	return max(1, int(math.Ceil((t1-t0)*voiceCharsPerSec-1e-9)))
}

func (s *Service) voiceEnabled() bool {
	return !s.cfg.VoiceOff && s.voices != nil && s.voiceCat != nil && s.gw != nil
}

// validateVoiceEdit 方案校验：换声音的结构（不查音色目录，目录在 voiceCatalogGaps）。返回规范化后的 voice：
// 逐句按 index 升序、去掉与原句相同的改动、语速 1.0 记为缺省，同一输入得到同一份 JSON（plan_hash 稳定）。
// gap 码：VOICE_AUDIO_MODE / VOICE_MISSING / VOICE_NO_LINES / VOICE_TOO_MANY_LINES / VOICE_SPEED / VOICE_LINE_INDEX /
// VOICE_LINE_DUPLICATE / VOICE_LINE_EMPTY / VOICE_LINE_TOO_LONG。
func validateVoiceEdit(in *VoiceEdit, audio string, an *AnalysisResult, add func(code, field, msg string)) *VoiceEdit {
	if audio != AudioRevoice {
		if in != nil {
			add("VOICE_AUDIO_MODE", "voice", "要换声音，先把声音选成「换个声音」")
		}
		return nil
	}
	if in == nil || in.VoiceID <= 0 {
		add("VOICE_MISSING", "voice.voiceId", "先选一个配音音色")
		return nil
	}
	if len(an.Lines) == 0 {
		add("VOICE_NO_LINES", "audio", "这条视频里没有检测到人说话的台词，没有可以换声音的内容。可以选「重新配乐」，或者在「台词」里写一段新台词")
		return nil
	}
	if len(an.Lines) > voiceMaxLines {
		add("VOICE_TOO_MANY_LINES", "audio", fmt.Sprintf("这条视频的台词太多了（超过 %d 句），暂时不能换声音", voiceMaxLines))
		return nil
	}
	out := &VoiceEdit{VoiceID: in.VoiceID}
	switch {
	case in.Speed == 0 || math.Abs(in.Speed-1) < 1e-9:
	case math.IsNaN(in.Speed) || in.Speed < voiceMinSpeed || in.Speed > voiceMaxSpeed:
		add("VOICE_SPEED", "voice.speed", fmt.Sprintf("语速只能在 %.1f 到 %.1f 倍之间", voiceMinSpeed, voiceMaxSpeed))
	default:
		out.Speed = math.Round(in.Speed*100) / 100
	}
	seen := map[int]bool{}
	for i, l := range in.Lines {
		field := fmt.Sprintf("voice.lines[%d]", i)
		switch {
		case l.Index < 1 || l.Index > len(an.Lines):
			add("VOICE_LINE_INDEX", field+".index", fmt.Sprintf("视频里一共 %d 句台词，没有第 %d 句", len(an.Lines), l.Index))
			continue
		case seen[l.Index]:
			add("VOICE_LINE_DUPLICATE", field+".index", fmt.Sprintf("第 %d 句重复了，每句只能改一次", l.Index))
			continue
		}
		seen[l.Index] = true
		text := strings.TrimSpace(l.Text)
		orig := an.Lines[l.Index-1]
		if text == "" {
			add("VOICE_LINE_EMPTY", field+".text", fmt.Sprintf("第 %d 句的台词是空的，写上内容，或者删掉这一句的修改（沿用原片台词）", l.Index))
			continue
		}
		if limit := voiceLineLimit(orig.T0, orig.T1); len([]rune(text)) > limit {
			add("VOICE_LINE_TOO_LONG", field+".text", fmt.Sprintf("第 %d 句最多 %d 个字（原句约 %.1f 秒），现在是 %d 个字，请缩短一点",
				l.Index, limit, orig.T1-orig.T0, len([]rune(text))))
			continue
		}
		if text == strings.TrimSpace(orig.Text) {
			continue // 和原句一样 = 没改
		}
		out.Lines = append(out.Lines, VoiceLineEdit{Index: l.Index, Text: text})
	}
	sort.Slice(out.Lines, func(i, j int) bool { return out.Lines[i].Index < out.Lines[j].Index })
	return out
}

// voiceCatalogGaps 音色必须存在且启用，换声音必须已开放（没装配 / 运行时关闭 / 没配网关）。
func (s *Service) voiceCatalogGaps(ctx context.Context, v *VoiceEdit) []Gap {
	if v == nil {
		return nil
	}
	if !s.voiceEnabled() {
		return []Gap{{Code: "VOICE_UNAVAILABLE", Field: "audio", Message: "换声音暂未开放，可以先选「重新配乐」或「保留原声」"}}
	}
	info, err := s.voiceCat.GetVoice(ctx, v.VoiceID)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 查音色失败 判据=VOICE_LOOKUP_FAILED voice=%d: %v", v.VoiceID, err)
		return []Gap{{Code: "VOICE_UNAVAILABLE", Field: "voice.voiceId", Message: "音色列表暂时读不出来，请稍后再试"}}
	}
	if info == nil {
		logx.WithContext(ctx).Infof("replicate: 方案拒收 判据=VOICE_UNKNOWN voice=%d", v.VoiceID)
		return []Gap{{Code: "VOICE_UNKNOWN", Field: "voice.voiceId", Message: "这个音色不存在或已下线，请重新选择"}}
	}
	return nil
}

// voiceJob 把方案里的换声音冻结成 job 参数：音色再查一次（确认时已下线 → VOICE_UNAVAILABLE），每句取改后的或原句。
func (s *Service) voiceJob(ctx context.Context, ann Annotations, an *AnalysisResult) (*VoiceJob, error) {
	if ann.Audio != AudioRevoice || ann.Voice == nil {
		return nil, nil
	}
	if !s.voiceEnabled() {
		return nil, xerr.NewReason(xerr.NotReady, reasonVoiceUnavailable, "换声音暂未开放，可以先改成「重新配乐」或「保留原声」")
	}
	info, err := s.voiceCat.GetVoice(ctx, ann.Voice.VoiceID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, xerr.NewReason(xerr.Conflict, reasonVoiceUnavailable, "这个音色已经下线了，请回到方案重新选一个音色")
	}
	edits := map[int]string{}
	for _, l := range ann.Voice.Lines {
		edits[l.Index] = l.Text
	}
	vj := &VoiceJob{VoiceID: info.ID, Voice: info.Voice, Name: info.Name, Speed: ann.Voice.Speed, ModelGroup: s.cfg.VoiceModelGroup}
	for i, l := range an.Lines {
		text := strings.TrimSpace(l.Text)
		if e, ok := edits[i+1]; ok {
			text = e
		}
		if text == "" || !(l.T1 > l.T0) {
			continue
		}
		vj.Lines = append(vj.Lines, VoiceLine{Index: i + 1, T0: l.T0, T1: l.T1, Text: text})
	}
	if len(vj.Lines) == 0 {
		return nil, xerr.Param("这条视频没有可以配音的台词，不能换声音")
	}
	return vj, nil
}

// voiceLipRisk 方案卡 / 同意卡上的口型提示（CONTRACT §P 产品口径）。
const voiceLipRisk = "换声音后口型可能对不上"

// voicePlanLines 方案卡上的配音设置（大白话）：换声音 = 一项改动（音色 + 语速）；逐句改了台词再加一项，没改就在「保持不变」里说台词沿用原片。
// 价格不变（配音免费，不进报价）；音色名现查目录，查不到只说「所选音色」。
func (s *Service) voicePlanLines(ctx context.Context, v *PlanView, ann Annotations, an *AnalysisResult) {
	if ann.Audio != AudioRevoice || ann.Voice == nil {
		return
	}
	name := "所选音色"
	if s.voiceCat != nil {
		if info, err := s.voiceCat.GetVoice(ctx, ann.Voice.VoiceID); err == nil && info != nil {
			name = "「" + info.Name + "」"
		}
	}
	n := len(an.Lines)
	note := "用" + name + "给原片的台词重新配音"
	if sp := ann.Voice.Speed; sp > 0 {
		note += fmt.Sprintf("，语速 %.2g 倍", sp)
	}
	v.Changes = append(v.Changes, DimLine{Key: "voice", Label: "配音", Mode: ModeReplace, ModeLabel: "换个声音",
		Current: fmt.Sprintf("原片有 %d 句台词", n), Note: note})
	if len(ann.Voice.Lines) > 0 {
		idx := make([]string, len(ann.Voice.Lines))
		for i, l := range ann.Voice.Lines {
			idx[i] = fmt.Sprint(l.Index)
		}
		v.Changes = append(v.Changes, DimLine{Key: "voiceLines", Label: "台词", Mode: ModeDescribe, ModeLabel: "按我写的改",
			Current: fmt.Sprintf("原片有 %d 句台词", n), Note: fmt.Sprintf("改了第 %s 句，其余沿用原片台词", strings.Join(idx, "、"))})
		return
	}
	v.Kept = append(v.Kept, DimLine{Key: "voiceLines", Label: "台词", Mode: ModeKeep, ModeLabel: "不改",
		Current: fmt.Sprintf("原片有 %d 句台词", n), Note: "台词沿用原片，只换声音"})
}

// voiceTexts 方案里用户改过的台词（过内容审核；原句不审：本来就是原片的）。
func voiceTexts(ann Annotations) []string {
	if ann.Voice == nil {
		return nil
	}
	out := make([]string, 0, len(ann.Voice.Lines))
	for _, l := range ann.Voice.Lines {
		if t := strings.TrimSpace(l.Text); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// 音色列表（工具 replicate_list_voices）
// ---------------------------------------------------------------------------------------------

// VoiceListReq 音色筛选：gender（female / male，也认 男 / 女）、style（风格关键词，包含匹配）。
type VoiceListReq struct{ Gender, Style string }

// VoiceView 一个音色（不含网关参数值）。
type VoiceView struct {
	VoiceID   int64  `json:"voiceId"`
	Name      string `json:"name"`
	Gender    string `json:"gender"`
	Style     string `json:"style,omitempty"`
	SampleURL string `json:"sampleUrl,omitempty"`
}

// VoiceList replicate_list_voices 的结果。
type VoiceList struct {
	Voices []VoiceView `json:"voices"`
}

func normGender(g string) string {
	switch strings.ToLower(strings.TrimSpace(g)) {
	case "female", "f", "女", "女声":
		return "female"
	case "male", "m", "男", "男声":
		return "male"
	}
	return strings.ToLower(strings.TrimSpace(g))
}

// ListVoices 可选的配音音色（只列启用的）。换声音没开放 → 7003。样音地址是音色目录里的试听地址（公有桶，原样返回，不需要签名）。
func (s *Service) ListVoices(ctx context.Context, req VoiceListReq) (*VoiceList, error) {
	if !s.voiceEnabled() {
		return nil, xerr.New(xerr.NotReady, "换声音暂未开放")
	}
	all, err := s.voiceCat.ListVoices(ctx)
	if err != nil {
		return nil, err
	}
	g, style := normGender(req.Gender), strings.TrimSpace(req.Style)
	out := &VoiceList{Voices: []VoiceView{}}
	for _, v := range all {
		if g != "" && normGender(v.Gender) != g {
			continue
		}
		if style != "" && !strings.Contains(v.Style, style) {
			continue
		}
		out.Voices = append(out.Voices, VoiceView{VoiceID: v.ID, Name: v.Name, Gender: v.Gender, Style: v.Style, SampleURL: v.PreviewURL})
	}
	return out, nil
}

// ---------------------------------------------------------------------------------------------
// 额度（确认时占用，失败退回）
// ---------------------------------------------------------------------------------------------

func voiceQuotaError(total int) error {
	return xerr.NewReason(xerr.Conflict, ReasonVoiceQuotaExhausted,
		fmt.Sprintf("今天的 %d 条换声音额度已经用完了，明天 0 点恢复。可以选「重新配乐」或「保留原声」直接生成", total))
}

// voiceQuotaPrecheck 确认最前面的快速检查（还没建任务、没动钱）：今天的额度已经满了就直接拒。真正的占用在建任务之后（原子）。
func (s *Service) voiceQuotaPrecheck(ctx context.Context, userID int64, need int) error {
	total := s.cfg.VoiceDailyQuota
	used, err := s.voices.VoiceQuotaUsed(ctx, userID, s.previewDay())
	if err != nil {
		return err
	}
	if used+need > total {
		logx.WithContext(ctx).Infof("replicate: 确认拒收 判据=VOICE_QUOTA_EXHAUSTED user=%d 今日已用=%d/%d 需要=%d", userID, used, total, need)
		return voiceQuotaError(total)
	}
	return nil
}

// acquireVoiceQuota 给刚建好的任务占一次额度（同任务重放不重复占）。额度满 → VOICE_QUOTA_EXHAUSTED。
func (s *Service) acquireVoiceQuota(ctx context.Context, j *model.Job) error {
	total := s.cfg.VoiceDailyQuota
	used, ok, err := s.voices.AcquireVoiceQuota(ctx, j.UserID, j.ID, s.previewDay(), total)
	if err != nil {
		return err
	}
	if !ok {
		logx.WithContext(ctx).Infof("replicate: 确认拒收 判据=VOICE_QUOTA_EXHAUSTED user=%d job=%d 今日已用=%d/%d", j.UserID, j.ID, used, total)
		return voiceQuotaError(total)
	}
	return nil
}

// releaseVoice 退回任务占用的换声音额度（幂等：只退一次；没占过什么都不做）。脱离请求上下文执行，失败只告警。
func (s *Service) releaseVoice(ctx context.Context, jobID int64, why string) {
	if s.voices == nil {
		return
	}
	dctx, cancel := s.detached(ctx)
	defer cancel()
	released, err := s.voices.ReleaseVoiceQuota(dctx, jobID)
	switch {
	case err != nil:
		logx.WithContext(ctx).Errorf("replicate: 换声音额度退回失败 判据=VOICE_QUOTA_RELEASE_FAILED job=%d 原因=%s: %v", jobID, why, err)
	case released:
		logx.WithContext(ctx).Infof("replicate: 换声音额度已退回 job=%d 原因=%s", jobID, why)
	}
}

// releaseVoiceIfAny 任务没成功时退回额度：先看冻结输入是不是换声音（不是就不碰库）。
func (s *Service) releaseVoiceIfAny(ctx context.Context, j *model.Job, why string) {
	if s.voices == nil {
		return
	}
	var in JobInput
	if err := json.Unmarshal(j.Input, &in); err != nil || in.Voice == nil {
		return
	}
	s.releaseVoice(ctx, j.ID, why)
}

// ---------------------------------------------------------------------------------------------
// 合成：按原句在成片时间轴上的位置铺成一条音轨
// ---------------------------------------------------------------------------------------------

// voicePlacement 一句台词在成片时间轴上的位置：起点与可用时间窗（到下一句起点 / 成片结尾，且不超过原句长度的 1.25 倍 + 0.5 秒，
// 不让一句话拖进别的画面）。
type voicePlacement struct {
	VoiceLine
	Start float64
	Slot  float64
}

// voicePlacements 原片时间 → 成片时间：成片是各段（去掉「用户不继续」的段，失败占位段照常占时间）按顺序首尾相接，
// 一句台词按它起点所在的段映射（起点落在没做的段里 = 这句不配）。返回按成片时间升序的位置与成片总时长。
func voicePlacements(outs []segOutcome, lines []VoiceLine) ([]voicePlacement, float64) {
	type win struct{ t0, t1, off float64 }
	var wins []win
	total := 0.0
	for _, o := range outs {
		if o.omitted {
			continue
		}
		l := o.seg.T1 - o.seg.T0
		if !(l > 0) {
			continue
		}
		wins = append(wins, win{o.seg.T0, o.seg.T1, total})
		total += l
	}
	var ps []voicePlacement
	for _, ln := range lines {
		for i, w := range wins {
			last := i == len(wins)-1
			if ln.T0 >= w.t0-1e-6 && (ln.T0 < w.t1-1e-6 || (last && ln.T0 <= w.t1+1e-6)) {
				ps = append(ps, voicePlacement{VoiceLine: ln, Start: w.off + math.Max(0, ln.T0-w.t0)})
				break
			}
		}
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Start < ps[j].Start })
	for i := range ps {
		end := total
		if i+1 < len(ps) {
			end = ps[i+1].Start
		}
		slot := end - ps[i].Start
		if lim := (ps[i].T1-ps[i].T0)*1.25 + 0.5; slot > lim {
			slot = lim
		}
		ps[i].Slot = math.Max(slot, voiceMinSlotSec)
	}
	return ps, total
}

// voiceClip 一句合成好的配音。
type voiceClip struct {
	rid     string
	durSec  float64 // 0 = 网关没给时长
	trimmed bool    // 提速后仍比时间窗长，截掉了尾巴
}

func voiceTTSKey(prefix string, jobID int64, idx int, group, voice, text string, speed float64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%.3f|%s", group, voice, speed, text)))
	return fmt.Sprintf("%srpl-%d:voice/%02d/%s", prefix, jobID, idx, hex.EncodeToString(sum[:5]))
}

// ttsOnce 合成一句：提交 tts.synthesize 并等到终态（最多 VoiceWait）。幂等键由（任务、句号、模型组、音色、语速、文字）确定，
// 重试 / 重新合成成片都命中网关上同一次调用，不会重复花钱。
func (s *Service) ttsOnce(ctx context.Context, j *model.Job, in *JobInput, ln VoiceLine, speed float64) (rid string, durSec float64, err error) {
	v := in.Voice
	input := map[string]any{"text": ln.Text, "voice": v.Voice, "format": "mp3"}
	if math.Abs(speed-1) > 1e-6 {
		input["speed"] = math.Round(speed*100) / 100
	}
	req := aigateway.CallRequest{Operation: opTTSSynthesize, ModelGroup: v.ModelGroup, Input: input,
		Labels:       map[string]string{"caller_biz": "replicate", "kind": "voice", "job": fmt.Sprint(j.ID)},
		MediaPurpose: aigateway.MediaPurposeIntermediate}
	if mc := strings.TrimSpace(in.MaxCostCNY); mc != "" {
		req.Limits = &aigateway.Limits{MaxCost: mc}
	}
	wctx, cancel := context.WithTimeout(ctx, s.cfg.VoiceWait)
	defer cancel()
	call, err := s.gw.Submit(wctx, req, aigateway.SubmitOptions{IdempotencyKey: voiceTTSKey(in.KeyPrefix, j.ID, ln.Index, v.ModelGroup, v.Voice, ln.Text, speed),
		Wait: min(s.cfg.VoiceWait, 60*time.Second), RetryFailed: true})
	if err != nil {
		return "", 0, fmt.Errorf("提交失败: %w", err)
	}
	for call != nil && !call.IsTerminal() {
		if wctx.Err() != nil {
			return "", 0, fmt.Errorf("等待超过 %s call=%s", s.cfg.VoiceWait, call.ID)
		}
		if call, err = s.gw.Get(wctx, call.ID, min(25*time.Second, s.cfg.VoiceWait)); err != nil {
			return "", 0, fmt.Errorf("查询失败: %w", err)
		}
	}
	if call == nil || !call.Succeeded() || call.Result == nil {
		st := "empty"
		if call != nil {
			st = call.Status
			if call.Failure != nil && call.Failure.Class != "" {
				st = call.Failure.Class
			}
		}
		return "", 0, fmt.Errorf("网关没有成功 状态=%s", st)
	}
	if a := call.Result.Audio; a != nil && a.Rid != "" {
		return a.Rid, float64(a.DurationMs) / 1000, nil
	}
	return "", 0, fmt.Errorf("call=%s 没有音频产物", call.ID)
}

const opTTSSynthesize = "tts.synthesize"

// synthLine 合成一句并让它放进时间窗：先按用户语速合成；比时间窗长就把语速上调（至多 1.3 倍）重新合成一次；
// 仍然超出就保留、由时间线截掉尾巴（trimmed）。时长未知（网关没给）时不提速，交给时间线按窗截断。
func (s *Service) synthLine(ctx context.Context, j *model.Job, in *JobInput, p voicePlacement) (voiceClip, error) {
	base := in.Voice.Speed
	if base <= 0 {
		base = 1
	}
	rid, dur, err := s.ttsOnce(ctx, j, in, p.VoiceLine, base)
	if err != nil {
		return voiceClip{}, err
	}
	if dur > p.Slot+voiceFitTolerance {
		faster := base * math.Min(voiceMaxSpeedUp, dur/p.Slot*1.03)
		if faster > base+0.01 {
			if rid2, dur2, err2 := s.ttsOnce(ctx, j, in, p.VoiceLine, faster); err2 != nil {
				logx.WithContext(ctx).Infof("replicate: 配音提速重做失败，沿用原速并截尾 job=%d 句=%d: %v", j.ID, p.Index, err2)
			} else {
				rid, dur = rid2, dur2
			}
		}
	}
	return voiceClip{rid: rid, durSec: dur, trimmed: dur > p.Slot+voiceFitTolerance}, nil
}

// synthAll 并发合成全部句子（并发 ≤ VoiceConcurrency）；任何一句失败整体失败（不交付半条配音）。
func (s *Service) synthAll(ctx context.Context, j *model.Job, in *JobInput, ps []voicePlacement) ([]voiceClip, error) {
	clips := make([]voiceClip, len(ps))
	errs := make([]error, len(ps))
	sem := make(chan struct{}, max(1, s.cfg.VoiceConcurrency))
	var wg sync.WaitGroup
	for i := range ps {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			clips[i], errs[i] = s.synthLine(ctx, j, in, ps[i])
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("第 %d 句: %w", ps[i].Index, err)
		}
	}
	return clips, nil
}

// revoiceTrack 逐句合成 → 铺成音轨 → 替换 videoRid 的音轨，返回换声后的视频 rid；失败返回 (why, err)，由调用方决定回落与额度。
// 首次出片（revoiceAudio）与免费重做声音（audio_retry.go）共用；doc 只用来记「被截尾的句子」与 warning。
func (s *Service) revoiceTrack(ctx context.Context, j *model.Job, in *JobInput, outs []segOutcome, videoRid string, idem func(string) string, doc *ResultDoc) (rid, why string, err error) {
	var m VoiceMixer
	if s.composer != nil {
		m, _ = s.composer.(VoiceMixer)
	}
	switch {
	case in.Voice == nil || len(in.Voice.Lines) == 0:
		return "", "任务里没有配音参数", errNoComposer
	case m == nil:
		return "", "没装配音轨合成算子", errNoComposer
	case s.gw == nil:
		return "", "没装配网关", errNoComposer
	}
	ps, total := voicePlacements(outs, in.Voice.Lines)
	if len(ps) == 0 || !(total > 0) {
		return "", "没有可配音的句子（台词都落在没做的段里）", errNoComposer
	}
	if len(ps) > mediasvc.MaxTimelineClips {
		return "", "句子过多", fmt.Errorf("%d 句", len(ps))
	}
	clips, err := s.synthAll(ctx, j, in, ps)
	if err != nil {
		return "", "合成配音失败", err
	}
	items := make([]mediasvc.AudioClip, len(ps))
	for i, p := range ps {
		items[i] = mediasvc.AudioClip{RID: clips[i].rid, Start: p.Start, MaxSeconds: p.Slot}
		if clips[i].trimmed {
			doc.VoiceTrimmed = append(doc.VoiceTrimmed, p.Index)
			doc.Warnings = appendOnce(doc.Warnings, fmt.Sprintf("VOICE_LINE_TRUNCATED:%d", p.Index))
			logx.WithContext(ctx).Infof("replicate: WARN 配音比时间窗长，提速后仍截掉尾巴 job=%d 句=%d 窗=%.2fs 时长=%.2fs", j.ID, p.Index, p.Slot, clips[i].durSec)
		}
	}
	track, err := m.AudioTimeline(ctx, items, total, idem("vtrack"))
	if err != nil {
		return "", "铺音轨失败", err
	}
	res, err := m.MuxAudio(ctx, videoRid, track.RID, []mediasvc.TimeRange{{Start: 0, End: total}}, idem("vmux"))
	if err != nil {
		return "", "替换成片音轨失败", err
	}
	return res.RID, "", nil
}

// revoiceAudio 方案 audio=revoice：逐句合成 → 铺成音轨 → 替换成片音轨，返回换声后的视频 rid。
// 任何失败（没装配 / 合成失败 / 媒体服务出错）都回落到传入的 videoRid（交付不丢；段视频没有音轨，这条成片是静音的），
// 结果标 failed（视图告诉用户没换成、可免费重做声音）、退回额度。
func (s *Service) revoiceAudio(ctx context.Context, j *model.Job, in *JobInput, outs []segOutcome, videoRid string, idem func(string) string, doc *ResultDoc) string {
	doc.AudioKind = AudioRevoice
	rid, why, err := s.revoiceTrack(ctx, j, in, outs, videoRid, idem, doc)
	if err != nil {
		doc.Audio = AudioResultFailed
		logx.WithContext(ctx).Errorf("replicate: 换声音失败，成片暂时没有声音 判据=revoice_failed job=%d 原因=%s: %v", j.ID, why, err)
		s.releaseVoice(ctx, j.ID, "换声音失败")
		return videoRid
	}
	doc.Audio = AudioResultRevoiced
	s.funnel(ctx, funnelEvent{Stage: StageRevoice, UserID: j.UserID, PlanID: j.PlanID, JobID: j.ID, Dedupe: fmt.Sprintf("rv:%d", j.ID)})
	return rid
}
