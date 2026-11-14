package core

import (
	"context"
	"encoding/json"
	"strings"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/segment"
	"aigc-local-life/pkg/xerr"
)

// 出片模型目录（做活不做死：默认推荐一个，用户可选；运营用 afv_system_config.replicate.models / replicate.default_model 收放）。
// 硬限制取自网关 adapters/video_specs.py（时长、清晰度、画幅）；单价不写死，按用户实时报价（pkg/billing，按模型组分价）。

// ModelSpec 一个可选出片模型。
type ModelSpec struct {
	Group             string
	Name              string
	Features          []string
	MinDurationSec    int
	MaxDurationSec    int
	Resolutions       []string
	AspectRatios      []string
	DefaultResolution string
}

var allRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9"}

// ModelCatalog 四家（顺序即页面展示顺序）。
var ModelCatalog = []ModelSpec{
	{Group: "vid.seedance-2.5", Name: "Seedance 2.5", Features: []string{"画质与一致性最好", "动作还原细", "适合人物 / 商品替换"},
		MinDurationSec: 4, MaxDurationSec: 30, Resolutions: []string{"480p", "720p", "1080p"}, AspectRatios: allRatios, DefaultResolution: "720p"},
	{Group: "vid.seedance-2", Name: "Seedance 2.0", Features: []string{"性价比高", "稳定", "适合常规换商品"},
		MinDurationSec: 4, MaxDurationSec: 15, Resolutions: []string{"480p", "720p", "1080p"}, AspectRatios: allRatios, DefaultResolution: "720p"},
	{Group: "vid.minimax-h3", Name: "MiniMax H3", Features: []string{"运镜流畅", "人物表情自然", "清晰度为 768p 档"},
		MinDurationSec: 4, MaxDurationSec: 15, Resolutions: []string{"480p", "768p", "1080p"}, AspectRatios: allRatios, DefaultResolution: "768p"},
	{Group: "vid.wan-3.0", Name: "万相 3.0", Features: []string{"分镜控制强", "场景重建好", "不支持 21:9"},
		MinDurationSec: 2, MaxDurationSec: 30, Resolutions: []string{"480p", "720p", "1080p"}, AspectRatios: []string{"16:9", "9:16", "1:1", "4:3", "3:4"}, DefaultResolution: "720p"},
}

// ModelGroups 内置模型组码（兼容旧引用；校验以 enabledModels 为准）。
var ModelGroups = []string{"vid.seedance-2.5", "vid.seedance-2", "vid.minimax-h3", "vid.wan-3.0"}

func specOf(group string) (ModelSpec, bool) {
	for _, m := range ModelCatalog {
		if m.Group == group {
			return m, true
		}
	}
	return ModelSpec{}, false
}

// enabledModels 运营开放的模型（Config.Models 为空 = 全部；未知码忽略）。
func (s *Service) enabledModels() []ModelSpec {
	if len(s.cfg.Models) == 0 {
		return ModelCatalog
	}
	var out []ModelSpec
	for _, g := range s.cfg.Models {
		if m, ok := specOf(strings.TrimSpace(g)); ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return ModelCatalog
	}
	return out
}

func (s *Service) modelEnabled(group string) (ModelSpec, bool) {
	for _, m := range s.enabledModels() {
		if m.Group == group {
			return m, true
		}
	}
	return ModelSpec{}, false
}

// DefaultModel 推荐模型：配置的默认（须已开放），否则第一个开放的。
func (s *Service) DefaultModel() string {
	if m, ok := s.modelEnabled(s.cfg.DefaultModel); ok {
		return m.Group
	}
	return s.enabledModels()[0].Group
}

// ModelsReq GET /api/v1/replicate/models。
type ModelsReq struct {
	UserID      int64
	AnalysisNo  string // 可选：带上就按这条视频的分段算「是否支持该时长」与预计点数
	SampleID    int64
	Resolution  string
	AspectRatio string
}

// ModelView 模型列表的一项。
type ModelView struct {
	ModelGroup         string   `json:"modelGroup"`
	Name               string   `json:"name"`
	Features           []string `json:"features"`
	Recommended        bool     `json:"recommended"`
	Dialect            string   `json:"dialect"`
	Resolutions        []string `json:"resolutions"`
	AspectRatios       []string `json:"aspectRatios"`
	DefaultResolution  string   `json:"defaultResolution"`
	MinDurationSec     int      `json:"minDurationSec"`
	MaxDurationSec     int      `json:"maxDurationSec"`
	Resolution         string   `json:"resolution"`      // 单价按哪个清晰度算的（请求的清晰度不支持时为模型默认）
	PointsPerSecond    float64  `json:"pointsPerSecond"` // 10 秒报价 / 10
	PriceText          string   `json:"priceText,omitempty"`
	EstimatedPoints    int64    `json:"estimatedPoints,omitempty"` // 带视频时：按分段的合计点数
	SupportsDuration   bool     `json:"supportsDuration"`
	SupportsAspect     bool     `json:"supportsAspect"`
	SupportsResolution bool     `json:"supportsResolution"`
	Available          bool     `json:"available"`
	UnavailableText    string   `json:"unavailableText,omitempty"`
}

// ModelList 模型列表。
type ModelList struct {
	DefaultModel string      `json:"defaultModel"`
	Items        []ModelView `json:"items"`
}

// ListModels 可选模型（名称、特点、单价/秒、是否支持该时长与画幅）。报价失败（如未配价）的模型标为不可选，不报错。
func (s *Service) ListModels(ctx context.Context, req ModelsReq) (*ModelList, error) {
	var segs []segment.Segment
	if req.AnalysisNo != "" || req.SampleID > 0 {
		an, err := s.analysisFor(ctx, req.UserID, req.AnalysisNo, req.SampleID)
		if err != nil {
			return nil, err
		}
		if sg, err := segment.Plan(an.DurationS, candidates(an), s.cfg.Limits); err == nil {
			segs = sg
		}
	}
	res := strings.ToLower(strings.TrimSpace(req.Resolution))
	ratio := strings.TrimSpace(req.AspectRatio)
	def := s.DefaultModel()
	out := &ModelList{DefaultModel: def}
	for _, m := range s.enabledModels() {
		v := ModelView{ModelGroup: m.Group, Name: m.Name, Features: m.Features, Recommended: m.Group == def, Dialect: DialectFor(m.Group).Code,
			Resolutions: m.Resolutions, AspectRatios: m.AspectRatios, DefaultResolution: m.DefaultResolution,
			MinDurationSec: m.MinDurationSec, MaxDurationSec: m.MaxDurationSec, SupportsDuration: true, Available: true}
		v.SupportsResolution = res == "" || contains(m.Resolutions, res)
		v.SupportsAspect = ratio == "" || contains(m.AspectRatios, ratio)
		v.Resolution = m.DefaultResolution
		if res != "" && v.SupportsResolution {
			v.Resolution = res
		}
		for _, sg := range segs {
			if sg.GenDuration < m.MinDurationSec || sg.GenDuration > m.MaxDurationSec {
				v.SupportsDuration = false
			}
		}
		if pts, err := s.ledger.Quote(ctx, s.charge(req.UserID, "", m.Group, v.Resolution, 10)); err != nil {
			v.Available, v.UnavailableText = false, "这个模型暂时不可用"
		} else {
			v.PointsPerSecond = float64(pts) / 10
			v.PriceText = formatPPS(v.PointsPerSecond) + " 点/秒（" + v.Resolution + "）"
			if len(segs) > 0 {
				if q, err := s.quoteSegments(ctx, req.UserID, m.Group, v.Resolution, segs); err == nil {
					v.EstimatedPoints = q.Total
				}
			}
		}
		if !v.SupportsDuration {
			v.Available, v.UnavailableText = false, "这条视频的分段时长这个模型做不了"
		} else if !v.SupportsAspect {
			v.Available, v.UnavailableText = false, "这个模型不支持所选画幅"
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

func formatPPS(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func (s *Service) analysisFor(ctx context.Context, userID int64, no string, sampleID int64) (*AnalysisResult, error) {
	var a *model.Analysis
	if sampleID > 0 {
		sa, _, err := s.sampleAnalysis(ctx, sampleID)
		if err != nil {
			return nil, err
		}
		a = sa
	} else {
		ua, err := s.store.GetAnalysisByNo(ctx, userID, no)
		if err != nil {
			return nil, err
		}
		if ua == nil || len(ua.Result) == 0 {
			return nil, xerr.New(xerr.NotFound, "找不到这条视频的分析结果，请重新上传")
		}
		a = ua
	}
	an, err := s.ResolveAnalysis(a)
	if err != nil {
		return nil, xerr.New(xerr.ServerError, "分析数据异常，请重新上传")
	}
	return an, nil
}
