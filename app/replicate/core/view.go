package core

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/quote"
	"aigc-local-life/app/replicate/rules/segment"
)

// 本文件：对外视图（方案卡、数值口径、成片规则）。所有用户文案取自 EXPERIENCE §4，数字全部由后端字段驱动。

// LimitsView GET /limits。
type LimitsView struct {
	segment.Limits
	SegTargetSec      float64 `json:"segTargetSec"`
	LongformEnabled   bool    `json:"longformEnabled"` // 多段（> 单段上限）是否开放：依赖媒体服务区间切段
	MaxInFlight       int     `json:"maxInFlight"`
	ContinueHours     int     `json:"continueHours"`
	RedoMaxPerSegment int     `json:"redoMaxPerSegment"`
	// DefaultModel 推荐模型；Models 开放的模型组（详情 / 单价走 GET /models）；WriterEnabled 确认后是否由写手逐段改写提示词
	DefaultModel  string   `json:"defaultModel"`
	Models        []string `json:"models"`
	WriterEnabled bool     `json:"writerEnabled"`
	// ConfirmThresholdPoints 发送前二次确认阈值（与确认接口的闸门同一配置）：方案总价超过它时确认必须带 expectedPoints；0 = 不启用
	ConfirmThresholdPoints int64 `json:"confirmThresholdPoints"`
}

// Policy 成片规则（水印 / 会员 / AI 标识），付费前在方案卡与确认条里说清（EXPERIENCE 0.1 #6）。
type Policy struct {
	Watermark   bool   `json:"watermark"`
	RemovableBy string `json:"removableBy,omitempty"` // recharge
	Text        string `json:"text"`
}

// PolicyResolver 按用户给成片规则（生产可接 pkg/usertier；本切片默认 DefaultPolicy）。
type PolicyResolver interface {
	Resolve(ctx context.Context, userID int64) Policy
}

// DefaultPolicy 规则未定时第一期一律不带品牌水印（产品方案 §5「水印与会员告知」）。
// 「AI 生成」角标待法务确认、交付侧也还没实现，这里不承诺。
type DefaultPolicy struct{}

func (DefaultPolicy) Resolve(context.Context, int64) Policy {
	return Policy{Watermark: false, Text: "成片：不带水印"}
}

// PlanView 方案卡（S4）。
type PlanView struct {
	PlanNo     string       `json:"planNo"`
	PlanHash   string       `json:"planHash"`
	AnalysisNo string       `json:"analysisNo"` // 方案出自哪条分析（同一源视频的方案 analysisNo 相同）
	Status     string       `json:"status"`
	Title      string       `json:"title"`
	Overview   string       `json:"overview"`
	Changes    []DimLine    `json:"changes"`
	Kept       []DimLine    `json:"kept"`
	Risks      []string     `json:"risks"`
	Timeline   TimelineView `json:"timeline"`
	ModelGroup string       `json:"modelGroup"`
	// Dialect 段提示词按哪家方言写的（prompts 预览即模板稿；开了写手时出片前还会逐段改写，失败回落这一稿）
	Dialect       string         `json:"dialect"`
	Cost          CostBlock      `json:"cost"`
	StageQuotes   StageQuotes    `json:"stageQuotes"`
	ETA           ETAView        `json:"eta"`
	Policy        Policy         `json:"policy"`
	ConfirmButton string         `json:"confirmButton"`
	Copyright     string         `json:"copyright"`
	Prompts       []string       `json:"prompts"` // 高级区「查看每段提示词」
	ExpiresAt     time.Time      `json:"expiresAt"`
	JobID         int64          `json:"jobId,omitempty"`
	Analysis      *AnalysisBrief `json:"analysis,omitempty"`
	// Moderation 内容检查（P0-4）：rejected 时方案不可确认（确认接口回 1003 MODERATION_REJECTED），text 为可读原因
	Moderation *ModerationView `json:"moderation,omitempty"`

	// 以下为示例 / 改标注 / 我的方案（Go_V1.0.41）补充：方案的输入与来源，前端据此回填编辑区
	Annotations  *Annotations `json:"annotations,omitempty"`
	RefImages    []RefImage   `json:"refImages,omitempty"`
	Resolution   string       `json:"resolution,omitempty"`
	AspectRatio  string       `json:"aspectRatio,omitempty"`
	SampleID     int64        `json:"sampleId,omitempty"`
	ParentPlanNo string       `json:"parentPlanNo,omitempty"` // 由哪张方案改标注而来
	SupersededBy string       `json:"supersededBy,omitempty"` // status=STALE 时：被哪张新方案取代
}

// StageQuotes 分阶段报价（与 cost.lines 同源）：先做第 1 段 / 其余段 / 合计。
type StageQuotes struct {
	First int64 `json:"first"`
	Rest  int64 `json:"rest"`
	Total int64 `json:"total"`
}

// ETAView 预计耗时区间（秒），文案以上限为准；first/rest 只在多段时有。
type ETAView struct {
	First *ETARange `json:"first,omitempty"`
	Rest  *ETARange `json:"rest,omitempty"`
	Total ETARange  `json:"total"`
	Prior bool      `json:"prior"` // true = 先验（没装配历史校准，或该桶历史样本不足）；false = 历史 P50 / P90（eta.go）
}

// ETARange 区间。
type ETARange struct {
	LowSec  int    `json:"lowSec"`
	HighSec int    `json:"highSec"`
	Text    string `json:"text"`
}

// AnalysisBrief 方案卡顶部的一句话概述。
type AnalysisBrief struct {
	Summary   string  `json:"summary"`
	DurationS float64 `json:"durationS"`
}

// DimLine 方案卡里的一项。
type DimLine struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Mode      string `json:"mode"`
	ModeLabel string `json:"modeLabel"`
	Current   string `json:"current"` // 「视频里现在是：…」
	Note      string `json:"note,omitempty"`
	Refs      []int  `json:"refs,omitempty"`
	Risk      string `json:"risk,omitempty"`
}

// TimelineView 分段时间线。
type TimelineView struct {
	Title    string        `json:"title"`
	Explain  string        `json:"explain"`
	Single   bool          `json:"single"`
	Segments []SegmentLine `json:"segments"`
	SeamTip  string        `json:"seamTip,omitempty"`
}

// SegmentLine 时间线上的一段。
type SegmentLine struct {
	Index       int     `json:"index"`
	T0          float64 `json:"t0"`
	T1          float64 `json:"t1"`
	GenDuration int     `json:"genDuration"`
	CutReason   string  `json:"cutReason"`
	CutLabel    string  `json:"cutLabel,omitempty"`
	Points      int64   `json:"points"`
}

// CostBlock 费用块。
type CostBlock struct {
	QuoteSnapshot
	FirstText   string `json:"firstText,omitempty"`
	RestText    string `json:"restText,omitempty"`
	TotalText   string `json:"totalText"`
	Explain     string `json:"explain"`
	OverlapText string `json:"overlapText,omitempty"`
	HoldText    string `json:"holdText"`
}

var modeLabel = map[string]string{ModeKeep: "不改", ModeReplace: "换成我的图", ModeDescribe: "按我写的改"}

var audioLabel = map[string]string{AudioRegenerate: "重新配乐", AudioKeep: "保留原声", AudioMute: "无声"}

// PlanViewExpired 视图派生的方案状态：READY 但已过 expires_at（库里不写这个值）。
const PlanViewExpired = "EXPIRED"

func (s *Service) planView(p *model.Plan, an *AnalysisResult, q QuoteSnapshot, autoContinue bool) (*PlanView, error) {
	var ann Annotations
	_ = json.Unmarshal(p.Annotations, &ann)
	var segs []PlanSegmentView
	_ = json.Unmarshal(p.Segments, &segs)
	var prompts []string
	_ = json.Unmarshal(p.Prompts, &prompts)
	n := len(segs)
	q.DefaultStage = quote.DefaultStage(n, autoContinue)
	v := &PlanView{PlanNo: p.PlanNo, PlanHash: p.PlanHash, Status: p.Status, Title: "这条方案会改什么", ModelGroup: p.ModelGroup, Dialect: DialectFor(p.ModelGroup).Code,
		Prompts: prompts, ExpiresAt: p.ExpiresAt, Copyright: "默认会改写原视频的台词和画面文字，只沿用结构和节奏",
		Policy: s.policy.Resolve(context.Background(), p.UserID)}
	if p.JobID != nil {
		v.JobID = *p.JobID
	}
	if p.Status == model.PlanReady && s.now().After(p.ExpiresAt) {
		v.Status = PlanViewExpired // 过期不落库，视图派生
	}
	if an != nil {
		v.Analysis = &AnalysisBrief{Summary: an.Summary, DurationS: an.DurationS}
	}
	var changeNames []string
	for _, k := range an.DimOrder {
		dim := an.Dims[k]
		e, edited := ann.Dims[k]
		line := DimLine{Key: k, Label: DimLabel[k], Mode: ModeKeep, Current: dim.Desc, Risk: dim.Risk}
		if edited {
			line.Mode, line.Note, line.Refs = e.Mode, e.Note, e.Refs
		}
		line.ModeLabel = modeLabel[line.Mode]
		if !dim.Present && line.Mode == ModeKeep {
			continue
		}
		if line.Mode == ModeKeep {
			v.Kept = append(v.Kept, line)
			continue
		}
		v.Changes = append(v.Changes, line)
		changeNames = append(changeNames, "换"+DimLabel[k])
		if dim.Risk == "high" {
			v.Risks = append(v.Risks, fmt.Sprintf("%s比较难换（涉及人脸或细节），效果可能和预期有出入", DimLabel[k]))
		}
		if k == "garment" && len(dim.Occurrences) > 1 {
			v.Risks = append(v.Risks, fmt.Sprintf("视频里的服装换了 %d 套，会按同一张图替换", len(dim.Occurrences)))
		}
	}
	if len(an.Subjects) > 1 {
		v.Risks = append(v.Risks, fmt.Sprintf("视频里有 %d 个人，替换只针对你选中的那位", len(an.Subjects)))
	}
	if ann.Audio == AudioKeep {
		v.Risks = append(v.Risks, "原声里的台词、店名、电话会原样保留；画面换了人或店之后，口型和内容可能对不上。原片的声音和音乐可能有版权，发布前请自行确认可用")
	}
	if len(an.OnScreenText) > 0 {
		v.Risks = append(v.Risks, "画面文字将在生成后由系统叠加，不会交给 AI 画")
	}
	dur := an.DurationS
	v.Overview = strings.Join(append(changeNames, audioLabel[ann.Audio], fmt.Sprintf("共 %d 秒", int(math.Round(dur))), fmt.Sprintf("%d 段", n)), " · ")

	pts := map[int]int64{}
	for _, l := range q.Lines {
		pts[l.Index] = l.Points
	}
	for _, sg := range segs {
		v.Timeline.Segments = append(v.Timeline.Segments, SegmentLine{Index: sg.Index, T0: sg.T0, T1: sg.T1, GenDuration: sg.GenDuration,
			CutReason: sg.CutReason, CutLabel: sg.CutLabel, Points: pts[sg.Index]})
	}
	lim := s.cfg.Limits
	if n <= 1 {
		v.Timeline.Single, v.Timeline.Title = true, "整条一次生成，没有拼接"
	} else {
		v.Timeline.Title = fmt.Sprintf("这条视频会拆成 %d 段分别生成，再拼成一条", n)
		v.Timeline.Explain = fmt.Sprintf("这条视频 %d 秒。AI 单次最长生成 %d 秒，所以拆成 %d 段。拆的位置选在镜头切换或说话停顿的地方，看起来自然",
			int(math.Round(dur)), lim.SegCapSec, n)
		v.Timeline.SeamTip = fmt.Sprintf("这里是两段的拼接处，会多留 %.1f 秒让画面接得更顺", lim.OverlapSec)
	}

	c := CostBlock{QuoteSnapshot: q, Explain: "按每段生成的秒数计点。哪一段没生成成功，就退哪一段的点数",
		HoldText: "确认后先预留点数（从可用余额里暂时扣出）；生成成功才真正扣除，没成功的段会退回"}
	if n > 1 {
		c.OverlapText = fmt.Sprintf("段与段之间多留的 %.1f 秒也按秒计点", lim.OverlapSec)
	}
	c.TotalText = fmt.Sprintf("合计 %d 点", q.Total)
	if q.DefaultStage == quote.StageFirst {
		c.FirstText = fmt.Sprintf("现在先做第 1 段：%d 点", q.First)
		c.RestText = fmt.Sprintf("满意之后再做剩下 %d 段：约 %d 点（现在不收，你点「继续」才预留）", q.RestSegments, q.Rest)
		c.TotalText = fmt.Sprintf("全部做完共约 %d 点", q.Total)
		v.ConfirmButton = fmt.Sprintf("先做第 1 段（%d 点）", q.First)
	} else {
		v.ConfirmButton = fmt.Sprintf("确认并生成（%d 点）", q.Total)
	}
	v.Cost = c
	v.StageQuotes = StageQuotes{First: q.First, Rest: q.Rest, Total: q.Total}
	v.ETA = s.eta(p.ModelGroup, segs)
	return v, nil
}
