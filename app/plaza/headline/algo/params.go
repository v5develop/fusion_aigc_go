// Package algo 「本周头条」推荐算法：纯函数，无 IO、无全局时钟（now 一律由调用方注入）。
//
// 设计依据：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 第 14 节。
// 同样的输入（计数桶、参数、上一版快照、规则、时钟）必得同样的输出——刷新流程、试算接口、单测共用这一份实现。
package algo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ScopeAll 后台全局总榜的保留范围（大写，行业 code 规则要求小写字母开头，永不冲突）。用户侧任何接口都取不到。
const ScopeAll = "ALL"

// ScopeAny 规则 / 抑制的「所有适用行业」：在每个包含该模板的行业范围生效。
const ScopeAny = "*"

// 分数归一化方式（14.5.7 score-normalize）。
const (
	NormalizeNone     = "none"
	NormalizeScopeMax = "scope_max"
)

// Velocity 增速信号参数。
type Velocity struct {
	MinBase float64 `json:"min-base"` // 最小基数（次/天）：前 6 天日均不足它时按它算，防「1 次到 3 次」被夸大
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
}

// Weights 各信号权重；新增信号在这里加一个键即可（未实现的键只告警忽略）。
type Weights struct {
	Decay    float64 `json:"decay"`
	Velocity float64 `json:"velocity"`
	Fresh    float64 `json:"fresh"`
	Users    float64 `json:"users"`
	Ctr      float64 `json:"ctr"`
}

// Params 一个范围生效的全部参数（全局值叠加行业覆盖后的结果）。JSON 键与设计文档 14.5.7 一致。
type Params struct {
	TopN                  int      `json:"top-n"`
	MaxVisible            int      `json:"max-visible"`
	HalfLifeHours         float64  `json:"half-life-hours"`
	Velocity              Velocity `json:"velocity"`
	FreshDays             int      `json:"fresh-days"`
	Weight                Weights  `json:"weight"`
	MinDecayed            float64  `json:"min-decayed"`
	MinUniqueUsers        int      `json:"min-unique-users"`
	RelMinRatio           float64  `json:"rel-min-ratio"`
	PerUserDailyCap       int      `json:"per-user-daily-cap"`
	RapidDedupeSeconds    int      `json:"rapid-dedupe-seconds"`
	MaxPerMedia           int      `json:"max-per-media"`
	HysteresisDelta       float64  `json:"hysteresis-delta"`
	MinDwellMinutes       int      `json:"min-dwell-minutes"`
	MaxRankMove           int      `json:"max-rank-move"`
	ColdStartFill         bool     `json:"cold-start-fill"`
	SnapshotRetentionDays int      `json:"snapshot-retention-days"`
	ScoreNormalize        string   `json:"score-normalize"`
	NormScale             float64  `json:"norm-scale"`
	GlobalTopN            int      `json:"global-top-n"`
	// DisplayMinUses 近 7 天做同款少于它时用户端不展示具体数字（只显示「热」标签和名次），推荐语也不带数字。
	DisplayMinUses int `json:"display-min-uses"`
}

// DefaultParams 设计文档 14.5.7 的默认值（yaml 未配置时使用）。
func DefaultParams() Params {
	return Params{
		TopN:          10,
		MaxVisible:    12,
		HalfLifeHours: 24,
		Velocity:      Velocity{MinBase: 3, Min: -1, Max: 2},
		FreshDays:     14,
		// users：去重用户数 ln(1+uniqueUsers)，产品方案 3.4「做过的不同用户数（避免一个人刷）」的打分项
		Weight:                Weights{Decay: 1.0, Velocity: 0.6, Fresh: 0.8, Users: 0.5, Ctr: 0},
		MinDecayed:            3.0,
		MinUniqueUsers:        3,
		RelMinRatio:           0.1,
		PerUserDailyCap:       3,
		RapidDedupeSeconds:    60,
		MaxPerMedia:           7,
		HysteresisDelta:       0.15,
		MinDwellMinutes:       120,
		MaxRankMove:           3,
		ColdStartFill:         true,
		SnapshotRetentionDays: 7,
		ScoreNormalize:        NormalizeNone,
		NormScale:             1,
		GlobalTopN:            20,
		DisplayMinUses:        20,
	}
}

// ScopeOverride 行业级覆盖：只开放三项（14.5.7），nil = 用全局值。
type ScopeOverride struct {
	TopN           *int     `json:"top-n,omitempty"`
	MinDecayed     *float64 `json:"min-decayed,omitempty"`
	MinUniqueUsers *int     `json:"min-unique-users,omitempty"`
}

func (o ScopeOverride) empty() bool {
	return o.TopN == nil && o.MinDecayed == nil && o.MinUniqueUsers == nil
}

// Config 管理端可编辑的参数：{"global":{…},"overrides":{"<scope_code>":{…}}}。
type Config struct {
	Global    Params                   `json:"global"`
	Overrides map[string]ScopeOverride `json:"overrides,omitempty"`
}

// For 某范围生效的参数（全局叠加该范围覆盖）。ALL 不吃覆盖。
func (c Config) For(scope string) Params {
	p := c.Global
	if scope == ScopeAll {
		return p
	}
	if o, ok := c.Overrides[scope]; ok {
		if o.TopN != nil {
			p.TopN = *o.TopN
		}
		if o.MinDecayed != nil {
			p.MinDecayed = *o.MinDecayed
		}
		if o.MinUniqueUsers != nil {
			p.MinUniqueUsers = *o.MinUniqueUsers
		}
	}
	return p
}

// Hash 参数的 sha256（键排序后的规范 JSON），写入快照 params_hash，复盘「当时为什么是这个榜」。
func (c Config) Hash() string {
	b, _ := c.CanonicalJSON()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CanonicalJSON 规范 JSON：结构体字段顺序固定，覆盖表按 scope 排序（encoding/json 对 map 键本就排序）；空覆盖剔除。
func (c Config) CanonicalJSON() ([]byte, error) {
	cp := Config{Global: c.Global}
	for k, v := range c.Overrides {
		if v.empty() {
			continue
		}
		if cp.Overrides == nil {
			cp.Overrides = map[string]ScopeOverride{}
		}
		cp.Overrides[k] = v
	}
	return json.Marshal(cp)
}

// FieldError 参数校验失败的字段与原因（管理端据此标红）。
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e FieldError) Error() string { return e.Field + ": " + e.Reason }

// FieldErrors 多个字段错误。
type FieldErrors []FieldError

func (es FieldErrors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

type rangeCheck struct {
	field    string
	v        float64
	min, max float64
}

func checkRanges(prefix string, cs []rangeCheck) FieldErrors {
	var errs FieldErrors
	for _, c := range cs {
		if math.IsNaN(c.v) || c.v < c.min || c.v > c.max {
			errs = append(errs, FieldError{Field: prefix + c.field, Reason: fmt.Sprintf("须在 %g~%g 之间，当前 %g", c.min, c.max, c.v)})
		}
	}
	return errs
}

// Validate 逐项范围 + 跨字段校验（14.5.7）。knownScopes 非 nil 时要求覆盖的 scope 是现存行业（且不能是 ALL / *）。
func (c Config) Validate(knownScopes map[string]bool) FieldErrors {
	p := c.Global
	errs := checkRanges("global.", []rangeCheck{
		{"top-n", float64(p.TopN), 3, 30},
		{"max-visible", float64(p.MaxVisible), float64(p.TopN), 50},
		{"half-life-hours", p.HalfLifeHours, 1, 168},
		{"velocity.min-base", p.Velocity.MinBase, 1, 100},
		{"velocity.min", p.Velocity.Min, -1, 0},
		{"velocity.max", p.Velocity.Max, 0, 10},
		{"fresh-days", float64(p.FreshDays), 0, 60},
		{"weight.decay", p.Weight.Decay, 0, 5},
		{"weight.velocity", p.Weight.Velocity, 0, 5},
		{"weight.fresh", p.Weight.Fresh, 0, 5},
		{"weight.users", p.Weight.Users, 0, 5},
		{"weight.ctr", p.Weight.Ctr, 0, 5},
		{"min-decayed", p.MinDecayed, 0, 1000},
		{"min-unique-users", float64(p.MinUniqueUsers), 1, 1000},
		{"rel-min-ratio", p.RelMinRatio, 0, 1},
		{"per-user-daily-cap", float64(p.PerUserDailyCap), 1, 20},
		{"rapid-dedupe-seconds", float64(p.RapidDedupeSeconds), 0, 3600},
		{"max-per-media", float64(p.MaxPerMedia), 1, float64(p.TopN)},
		{"hysteresis-delta", p.HysteresisDelta, 0, 0.5},
		{"min-dwell-minutes", float64(p.MinDwellMinutes), 0, 1440},
		{"max-rank-move", float64(p.MaxRankMove), 1, float64(p.TopN)},
		{"snapshot-retention-days", float64(p.SnapshotRetentionDays), 1, 90},
		{"norm-scale", p.NormScale, 0, 100},
		{"global-top-n", float64(p.GlobalTopN), 3, 100},
		{"display-min-uses", float64(p.DisplayMinUses), 0, 1000},
	})
	if p.ScoreNormalize != NormalizeNone && p.ScoreNormalize != NormalizeScopeMax {
		errs = append(errs, FieldError{Field: "global.score-normalize", Reason: "只能是 none 或 scope_max"})
	}
	scopes := make([]string, 0, len(c.Overrides))
	for s := range c.Overrides {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	for _, s := range scopes {
		o := c.Overrides[s]
		prefix := "overrides." + s + "."
		if s == ScopeAll || s == ScopeAny || s == "" {
			errs = append(errs, FieldError{Field: "overrides." + s, Reason: "保留范围不接受覆盖"})
			continue
		}
		if knownScopes != nil && !knownScopes[s] {
			errs = append(errs, FieldError{Field: "overrides." + s, Reason: "行业不存在"})
			continue
		}
		var cs []rangeCheck
		if o.TopN != nil {
			cs = append(cs, rangeCheck{"top-n", float64(*o.TopN), 3, 30})
			// 跨字段：覆盖后的 top-n 也要满足 max-visible ≥ top-n、max-per-media ≤ top-n
			if *o.TopN > p.MaxVisible {
				errs = append(errs, FieldError{Field: prefix + "top-n", Reason: fmt.Sprintf("不能大于 max-visible（%d）", p.MaxVisible)})
			}
			if p.MaxPerMedia > *o.TopN {
				errs = append(errs, FieldError{Field: prefix + "top-n", Reason: fmt.Sprintf("不能小于 max-per-media（%d）", p.MaxPerMedia)})
			}
		}
		if o.MinDecayed != nil {
			cs = append(cs, rangeCheck{"min-decayed", *o.MinDecayed, 0, 1000})
		}
		if o.MinUniqueUsers != nil {
			cs = append(cs, rangeCheck{"min-unique-users", float64(*o.MinUniqueUsers), 1, 1000})
		}
		errs = append(errs, checkRanges(prefix, cs)...)
	}
	return errs
}

// ParseConfig 解析控制行的 params_json，缺失项回退 defaults；JSON 非法或越界时整体沿用 defaults 并返回告警
// （刷新路径不因运营存了坏参数而停摆，14.5.7）。
func ParseConfig(raw []byte, defaults Params) (cfg Config, warn error) {
	cfg = Config{Global: defaults}
	if len(raw) == 0 || string(raw) == "null" {
		return cfg, nil
	}
	parsed := Config{Global: defaults}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return cfg, fmt.Errorf("params_json 非法，沿用默认值: %w", err)
	}
	if errs := parsed.Validate(nil); len(errs) > 0 {
		return cfg, fmt.Errorf("params_json 越界，沿用默认值: %w", errs)
	}
	return parsed, nil
}

// UnknownWeightKeys 返回 params_json 里 weight 下未实现的信号键（只告警忽略，14.5.2）。
func UnknownWeightKeys(raw []byte) []string {
	var probe struct {
		Global struct {
			Weight map[string]json.RawMessage `json:"weight"`
		} `json:"global"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil
	}
	var out []string
	for k := range probe.Global.Weight {
		if _, ok := signalRegistry[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
