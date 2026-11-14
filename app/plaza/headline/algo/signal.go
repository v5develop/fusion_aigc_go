package algo

import (
	"math"
	"sort"
)

// ScoreContext 打分时与范围相关的量（可选的按范围归一化要用）。
type ScoreContext struct {
	// MaxDecayed 该范围候选的最大 decayed（score-normalize=scope_max 时用）
	MaxDecayed float64
}

// SignalImpl 一个打分信号：score = Σ weight_k · f_k(stats)。新增信号 = 一个实现 + 一个权重键，不改刷新流程。
type SignalImpl interface {
	Key() string
	// Value 原始值；ok=false 表示该信号暂无数据（如 ctr 的曝光管道未落地），记 raw=null、贡献 0。
	Value(st Stats, p Params, sc ScoreContext) (v float64, ok bool)
	Weight(p Params) float64
}

type decaySignal struct{}

func (decaySignal) Key() string { return "decay" }
func (decaySignal) Value(st Stats, p Params, sc ScoreContext) (float64, bool) {
	v := math.Log1p(st.Decayed) // 压缩头部大数，避免单个爆款永久霸榜
	if p.ScoreNormalize == NormalizeScopeMax {
		if d := math.Log1p(sc.MaxDecayed); d > 0 {
			v = v / d * p.NormScale
		}
	}
	return v, true
}
func (decaySignal) Weight(p Params) float64 { return p.Weight.Decay }

type velocitySignal struct{}

func (velocitySignal) Key() string { return "velocity" }
func (velocitySignal) Value(st Stats, _ Params, _ ScoreContext) (float64, bool) {
	return st.Velocity, true
}
func (velocitySignal) Weight(p Params) float64 { return p.Weight.Velocity }

type freshSignal struct{}

func (freshSignal) Key() string { return "fresh" }
func (freshSignal) Value(st Stats, _ Params, _ ScoreContext) (float64, bool) {
	return st.Freshness, true
}
func (freshSignal) Weight(p Params) float64 { return p.Weight.Fresh }

// usersSignal 去重用户数：ln(1+uniqueUsers)。一个人反复刷只算一个用户，压住「单人刷榜」的分数。
type usersSignal struct{}

func (usersSignal) Key() string { return "users" }
func (usersSignal) Value(st Stats, _ Params, _ ScoreContext) (float64, bool) {
	return math.Log1p(float64(st.UniqueUsers)), true
}
func (usersSignal) Weight(p Params) float64 { return p.Weight.Users }

// ctrSignal 预留：曝光 / 点击事件管道落地后启用（第 11 节），当前恒无数据。
type ctrSignal struct{}

func (ctrSignal) Key() string                                       { return "ctr" }
func (ctrSignal) Value(Stats, Params, ScoreContext) (float64, bool) { return 0, false }
func (ctrSignal) Weight(p Params) float64                           { return p.Weight.Ctr }

var signalRegistry = map[string]SignalImpl{}
var signalOrder []string

func register(s SignalImpl) {
	signalRegistry[s.Key()] = s
	signalOrder = append(signalOrder, s.Key())
	sort.Strings(signalOrder)
}

func init() {
	register(decaySignal{})
	register(velocitySignal{})
	register(freshSignal{})
	register(usersSignal{})
	register(ctrSignal{})
}

// SignalDetail score_detail 里一项信号：原始值、权重、加权值。
type SignalDetail struct {
	Raw *float64 `json:"raw"`
	W   float64  `json:"w"`
	V   float64  `json:"v"`
}

// Score 总分与逐项明细（四位小数）。
func Score(st Stats, p Params, sc ScoreContext) (float64, map[string]SignalDetail) {
	detail := make(map[string]SignalDetail, len(signalOrder))
	total := 0.0
	for _, k := range signalOrder { // 固定顺序累加，浮点结果可复现
		s := signalRegistry[k]
		w := s.Weight(p)
		raw, ok := s.Value(st, p, sc)
		d := SignalDetail{W: w}
		if ok {
			r := round4(raw)
			d.Raw = &r
			d.V = round4(w * raw)
			total += w * raw
		}
		detail[k] = d
	}
	return round4(total), detail
}
