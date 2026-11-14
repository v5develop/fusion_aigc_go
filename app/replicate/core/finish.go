package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/mediasvc"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 成片合成：多段拼接（去掉衔接重叠）、失败段占位预览版、文字叠加（店名 / 价格等，生成后由系统叠加，不交给 AI 画）。
//
// 钱与合成无关：每段的扣除 / 退回在段级已完成，合成只读成功段的视频、调媒体服务（不计费），失败也不动账。
// 结果写 afv_replicate_job.result_document（ResultDoc）；段被重做成功后「依据」（各段视频指纹）变化 → 重新合成。
// ---------------------------------------------------------------------------------------------

// Composer 媒体服务的合成算子（*mediasvc.Client 直接满足）。
type Composer interface {
	Cut(ctx context.Context, sourceRid string, t0, t1 float64, idem string) (string, error)
	ConcatItems(ctx context.Context, items []mediasvc.ConcatItem, p mediasvc.ConcatParams, idem string) (mediasvc.ConcatResult, error)
	OverlayText(ctx context.Context, rid string, slots []mediasvc.TextSlot, idem string) (mediasvc.Resource, error)
}

// WithComposer 成片合成（拼接 / 占位 / 叠字）。不装配时多段任务只有逐段视频，没有整条成片。
func WithComposer(c Composer) Option { return func(s *Service) { s.composer = c } }

// 成片状态（ResultDoc.Status）。
const (
	ResultReady       = "ready"       // 成片可用
	ResultRetry       = "retry"       // 合成失败，轮询器稍后重试（≤ resultMaxAttempts 次）
	ResultFailed      = "failed"      // 合成多次失败：只交付逐段视频
	ResultUnavailable = "unavailable" // 没装配媒体服务：只交付逐段视频

	resultMaxAttempts = 3
)

// 成片种类（ResultDoc.Kind）。
const (
	ResultFull    = "full"    // 全部段成功的整条成片
	ResultPreview = "preview" // 有失败段：失败处是占位画面（预览版），重做成功后自动替换为完整成片
	ResultPartial = "partial" // 用户不继续 / 到期结束：只拼已做好的段（没做的段不占位）
)

// 默认占位文案（EXPERIENCE：失败段「这一段还在重做」）；没配中文字体时用英文，避免方块字。
const (
	DefaultPlaceholderText   = "这一段还在重做"
	placeholderTextNoFontFmt = "Segment %d is being redone"
)

// ResultDoc afv_replicate_job.result_document。
type ResultDoc struct {
	Status   string                 `json:"status"`
	Kind     string                 `json:"kind,omitempty"`
	VideoRid string                 `json:"videoRid,omitempty"`
	CoverRid string                 `json:"coverRid,omitempty"` // 封面：第一个成功段的首帧（网关结果里有才有）
	Timeline []mediasvc.ConcatEntry `json:"timeline,omitempty"`
	Missing  []int                  `json:"missing,omitempty"` // 占位的段号
	Texts    int                    `json:"texts"`             // 叠加了几处文字
	Basis    string                 `json:"basis"`             // 合成依据：各段视频指纹；变了就重新合成
	Attempts int                    `json:"attempts"`
	Error    string                 `json:"error,omitempty"`
	Warnings []string               `json:"warnings,omitempty"`
	Segments []resultSegmentContrib `json:"segments,omitempty"`
}

type resultSegmentContrib struct {
	Index    int    `json:"index"`
	VideoRid string `json:"videoRid,omitempty"`
	Kind     string `json:"kind"` // source | placeholder | omitted
}

// ResultView JobView.result：前端据 status / kind 决定播放器与文案。
type ResultView struct {
	Status   string `json:"status"`
	Kind     string `json:"kind,omitempty"`
	VideoRid string `json:"videoRid,omitempty"`
	CoverRid string `json:"coverRid,omitempty"`
	// VideoURL / CoverURL 现签的播放 / 下载地址（有效期见 JobView.urlExpiresAt；过期后重拉 job）。没装配签名时为空。
	VideoURL string `json:"videoUrl,omitempty"`
	CoverURL string `json:"coverUrl,omitempty"`
	Missing  []int  `json:"missing,omitempty"`
	Text     string `json:"text,omitempty"`
}

func resultView(raw json.RawMessage) *ResultView {
	if len(raw) == 0 {
		return nil
	}
	var d ResultDoc
	if json.Unmarshal(raw, &d) != nil || d.Status == "" {
		return nil
	}
	v := &ResultView{Status: d.Status, Kind: d.Kind, VideoRid: d.VideoRid, CoverRid: d.CoverRid, Missing: d.Missing}
	switch {
	case d.Status == ResultReady && d.Kind == ResultPreview:
		v.Text = fmt.Sprintf("预览版：第 %s 段还没做好，先用占位画面代替；重做成功后会自动换成完整成片", joinInts(d.Missing))
	case d.Status == ResultReady && d.Kind == ResultPartial:
		v.Text = "已拼好做完的部分"
	case d.Status == ResultReady:
		v.Text = "整条成片已拼好"
	case d.Status == ResultRetry:
		v.VideoRid, v.Text = "", "正在拼成一条完整视频…"
	default:
		v.VideoRid, v.Text = "", "整条成片暂时没拼好，可以先下载每一段"
	}
	return v
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprint(x)
	}
	return strings.Join(s, "、")
}

// TextOverlay 生成后叠加的一处文字（方案 annotations.texts；店名 / 价格 / 标题等，不交给 AI 画）。
type TextOverlay struct {
	Text  string            `json:"text"`
	Kind  string            `json:"kind,omitempty"` // shop_name | price | title | other
	Box   *mediasvc.TextBox `json:"box,omitempty"`  // 相对画面 0~1；缺省：底部居中横条
	T0    *float64          `json:"t0,omitempty"`   // 成片时间轴（秒）；都缺省 = 全程
	T1    *float64          `json:"t1,omitempty"`
	Color string            `json:"color,omitempty"` // #RRGGBB，默认白
}

const (
	maxTextOverlays = 4
	maxOverlayRunes = 30
)

var defaultTextBox = mediasvc.TextBox{X: 0.08, Y: 0.80, W: 0.84, H: 0.10}

// validateTexts 方案校验：文字叠加（gap 码 TEXT_TOO_MANY / TEXT_EMPTY / TEXT_TOO_LONG / TEXT_BOX / TEXT_TIME / TEXT_COLOR）。
func validateTexts(in []TextOverlay, add func(code, field, msg string)) []TextOverlay {
	if len(in) > maxTextOverlays {
		add("TEXT_TOO_MANY", "texts", fmt.Sprintf("最多叠加 %d 处文字", maxTextOverlays))
		return nil
	}
	out := make([]TextOverlay, 0, len(in))
	for i, t := range in {
		f := fmt.Sprintf("texts[%d]", i)
		t.Text = strings.TrimSpace(t.Text)
		switch {
		case t.Text == "":
			add("TEXT_EMPTY", f+".text", "要叠加的文字是空的，填上或删掉这一处")
			continue
		case len([]rune(t.Text)) > maxOverlayRunes:
			add("TEXT_TOO_LONG", f+".text", fmt.Sprintf("文字写短一点（%d 字以内）", maxOverlayRunes))
			continue
		}
		if b := t.Box; b != nil && (b.W <= 0 || b.H <= 0 || b.X < 0 || b.Y < 0 || b.X+b.W > 1.0001 || b.Y+b.H > 1.0001) {
			add("TEXT_BOX", f+".box", "文字位置超出画面，请重新摆放")
			continue
		}
		if (t.T0 != nil && *t.T0 < 0) || (t.T0 != nil && t.T1 != nil && *t.T1 <= *t.T0) {
			add("TEXT_TIME", f, "文字的显示时间不对，结束要晚于开始")
			continue
		}
		if t.Color != "" && !isHexColor(t.Color) {
			add("TEXT_COLOR", f+".color", "颜色格式不对")
			continue
		}
		switch t.Kind {
		case "shop_name", "price", "title", "other":
		case "":
			t.Kind = "other"
		default:
			t.Kind = "other"
		}
		out = append(out, t)
	}
	return out
}

func isHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// segOutcome 一段的最终贡献。
type segOutcome struct {
	seg     JobSegment
	rid     string
	cover   string
	durMs   int64
	failed  bool // 缺段（没成功、也不是用户不继续）：占位
	omitted bool // 用户不继续 / 到期结束（NOT_CONTINUED）：不占位
}

func (s *Service) outcomes(j *model.Job, in *JobInput, steps []*model.Step) []segOutcome {
	out := make([]segOutcome, 0, len(in.Segments))
	for _, sg := range in.Segments {
		o := segOutcome{seg: sg}
		chain := segChain(steps, sg.Index)
		for _, st := range chain {
			if st.Status != model.StepSucceeded {
				continue
			}
			var v struct {
				VideoRid   string `json:"videoRid"`
				CoverRid   string `json:"coverRid"`
				DurationMs int64  `json:"durationMs"`
			}
			if json.Unmarshal(st.Output, &v) == nil && v.VideoRid != "" {
				o.rid, o.cover, o.durMs = v.VideoRid, v.CoverRid, v.DurationMs // 链按 id 升序：取最后一次成功（重做后的）
			}
		}
		if o.rid == "" {
			// segChain 已跳过预留没成功的重做步骤；只有「用户不继续 / 到期」才不占位，其余缺段一律失败占位（不静默省略）
			var last *model.Step
			if n := len(chain); n > 0 {
				last = chain[n-1]
			}
			o.omitted = last != nil && last.Status == model.StepCanceled &&
				(deref(last.ErrorCode) == codeNotContinued || deref(last.ErrorCode) == codeCancelRest) // 用户不继续 / 不做剩下的段：不占位
			o.failed = !o.omitted
		}
		out = append(out, o)
	}
	return out
}

func basisOf(outs []segOutcome, texts []TextOverlay, font string) string {
	parts := make([]string, 0, len(outs)+2)
	for _, o := range outs {
		switch {
		case o.rid != "":
			parts = append(parts, o.rid)
		case o.failed:
			parts = append(parts, "ph")
		default:
			parts = append(parts, "-")
		}
	}
	tb, _ := json.Marshal(texts)
	parts = append(parts, string(tb), font)
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}

// finalize 终态（成功 / 部分成功）的 job 合成整条成片。幂等：依据不变且已成功（或已放弃）就不再做；媒体调用带幂等键。
func (s *Service) finalize(ctx context.Context, jobID int64) error {
	j, err := s.store.GetJob(ctx, jobID)
	if err != nil || j == nil {
		return err
	}
	if j.Status != model.JobSucceeded && j.Status != model.JobPartial {
		return nil
	}
	var in JobInput
	if err := json.Unmarshal(j.Input, &in); err != nil || len(in.Segments) == 0 {
		return err
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return err
	}
	outs := s.outcomes(j, &in, steps)
	font := strings.TrimSpace(s.cfg.FontRid)
	basis := basisOf(outs, in.Texts, font)
	var prev ResultDoc
	_ = json.Unmarshal(j.ResultDocument, &prev)
	if prev.Basis == basis && (prev.Status == ResultReady || prev.Status == ResultFailed ||
		(prev.Status == ResultUnavailable && s.composer == nil)) {
		// 合成已有定论：只补入已扣费段（后台人工退点 / 补扣后段的扣费状态可能变了；幂等，已有的不动）
		return s.enqueueOnJobDone(ctx, j, &prev) // 带上已有成片：按 rid 去重要看到它
	}
	doc := ResultDoc{Basis: basis, Attempts: 1}
	if prev.Basis == basis {
		doc.Attempts = prev.Attempts + 1
	}
	built, berr := s.compose(ctx, j, &in, outs, font, basis, &doc)
	switch {
	case berr == nil:
		doc.Status = ResultReady
		doc.VideoRid, doc.Timeline = built.RID, built.Timeline
	case s.composer == nil:
		doc.Status = ResultUnavailable
	case doc.Attempts >= resultMaxAttempts:
		doc.Status, doc.Error = ResultFailed, trimRunes(berr.Error(), 200)
		logx.WithContext(ctx).Errorf("replicate: 成片合成放弃 判据=ATTEMPTS_EXHAUSTED job=%d 次数=%d: %v", j.ID, doc.Attempts, berr)
	default:
		doc.Status, doc.Error = ResultRetry, trimRunes(berr.Error(), 200)
		logx.WithContext(ctx).Infof("replicate: 成片合成失败，稍后重试 job=%d 第 %d 次: %v", j.ID, doc.Attempts, berr)
	}
	// 已扣费的段与可用成片（含预览版）先入队入素材库，再写合成结果：入队失败本轮报错，下一轮重新合成（媒体幂等键，同一依据同一 rid）再入队，
	// 不会出现「成片写好了、入库却永远没入队」；入库本身由发件箱异步做，不阻塞 job 收尾（delivery.go）
	if err := s.enqueueOnJobDone(ctx, j, &doc); err != nil {
		return fmt.Errorf("成片入素材库入队失败: %w", err)
	}
	b, _ := json.Marshal(doc)
	_, err = s.store.TransitJob(ctx, j.ID, []int8{j.Status}, j.Status, map[string]any{"result_document": json.RawMessage(b)})
	return err
}

type composed struct {
	RID      string
	Timeline []mediasvc.ConcatEntry
}

var errNoComposer = fmt.Errorf("没装配媒体服务合成算子")

// compose 去重叠 → 拼接（失败段占位）→ 叠字。只有一段且无叠字时直接用该段视频（不调媒体服务）。
func (s *Service) compose(ctx context.Context, j *model.Job, in *JobInput, outs []segOutcome, font, basis string, doc *ResultDoc) (composed, error) {
	idem := func(step string) string { return jobKey(in.KeyPrefix, j.ID, "final/"+basis+"/"+step) }
	var items []mediasvc.ConcatItem
	real, multi := 0, len(outs) > 1
	for _, o := range outs {
		switch {
		case o.rid != "":
			real++
			if doc.CoverRid == "" {
				doc.CoverRid = o.cover
			}
		case o.failed:
			doc.Missing = append(doc.Missing, o.seg.Index)
		}
	}
	switch {
	case real == 0:
		return composed{}, fmt.Errorf("没有成功的段")
	case len(doc.Missing) > 0:
		doc.Kind = ResultPreview
	case real < len(outs):
		doc.Kind = ResultPartial
	default:
		doc.Kind = ResultFull
	}
	if !multi && len(in.Texts) == 0 {
		doc.Segments = []resultSegmentContrib{{Index: outs[0].seg.Index, VideoRid: outs[0].rid, Kind: "source"}}
		return composed{RID: outs[0].rid}, nil
	}
	if s.composer == nil {
		return composed{}, errNoComposer
	}
	sources := 0
	for _, o := range outs {
		length := o.seg.T1 - o.seg.T0
		switch {
		case o.rid != "":
			rid := o.rid
			lead := o.seg.OverlapIn
			end := lead + length
			if o.durMs > 0 && end > float64(o.durMs)/1000 {
				end = float64(o.durMs) / 1000
			}
			// 多段：去掉衔接重叠与取整多生成的尾巴，让时间轴与原片对齐
			if multi && (lead > 0.01 || float64(o.seg.GenDuration) > length+0.05) && end > lead+0.1 {
				cut, err := s.composer.Cut(ctx, rid, lead, end, idem(fmt.Sprintf("cut/%02d", o.seg.Index)))
				if err != nil {
					return composed{}, fmt.Errorf("第 %d 段去重叠失败: %w", o.seg.Index, err)
				}
				rid = cut
			}
			items = append(items, mediasvc.ConcatItem{RID: rid})
			doc.Segments = append(doc.Segments, resultSegmentContrib{Index: o.seg.Index, VideoRid: rid, Kind: "source"})
			sources++
		case o.failed:
			ph := &mediasvc.Placeholder{Duration: length, FontRid: font}
			if font != "" {
				ph.Text = strings.TrimSpace(s.cfg.PlaceholderText)
				if ph.Text == "" {
					ph.Text = DefaultPlaceholderText
				}
			} else {
				ph.Text = fmt.Sprintf(placeholderTextNoFontFmt, o.seg.Index)
				doc.Warnings = appendOnce(doc.Warnings, "PLACEHOLDER_NO_FONT")
				logx.WithContext(ctx).Infof("replicate: WARN 占位段没配中文字体（afv_system_config.replicate.font_rid），用英文占位 job=%d 段=%d", j.ID, o.seg.Index)
			}
			items = append(items, mediasvc.ConcatItem{Placeholder: ph})
			doc.Segments = append(doc.Segments, resultSegmentContrib{Index: o.seg.Index, Kind: "placeholder"})
		default:
			doc.Segments = append(doc.Segments, resultSegmentContrib{Index: o.seg.Index, Kind: "omitted"})
		}
	}
	out := composed{}
	if len(items) == 1 && sources == 1 {
		out.RID = items[0].RID
	} else {
		res, err := s.composer.ConcatItems(ctx, items, mediasvc.ConcatParams{DropAudio: in.Audio == AudioMute}, idem("concat"))
		if err != nil {
			return composed{}, fmt.Errorf("拼接失败: %w", err)
		}
		out.RID, out.Timeline = res.RID, res.Timeline
	}
	if len(in.Texts) > 0 {
		if font == "" {
			doc.Warnings = appendOnce(doc.Warnings, "TEXT_NO_FONT")
			logx.WithContext(ctx).Infof("replicate: WARN 文字叠加没配中文字体（afv_system_config.replicate.font_rid），本次不叠字 job=%d", j.ID)
			return out, nil
		}
		slots := make([]mediasvc.TextSlot, 0, len(in.Texts))
		for _, t := range in.Texts {
			box := defaultTextBox
			if t.Box != nil {
				box = *t.Box
			}
			slots = append(slots, mediasvc.TextSlot{Text: t.Text, Box: box, FontRid: font, Color: t.Color, MaxLines: 2,
				Stroke: &mediasvc.TextStroke{Color: "#000000", Width: 2}, Start: t.T0, End: t.T1})
		}
		res, err := s.composer.OverlayText(ctx, out.RID, slots, idem("text"))
		if err != nil {
			return composed{}, fmt.Errorf("文字叠加失败: %w", err)
		}
		out.RID = res.RID
		doc.Texts = len(slots)
	}
	return out, nil
}
