package billing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 按请求参数冻结（Java 迁移 V1.0.223 afv_ai_model.freeze_cap_json；与 Java ActualCostFreezeCap 同一条公式，两侧单测同一组向量）。
//
// 只对 settle_mode=ACTUAL_COST 且配了 freeze_cap_json 的载体生效；调用方没给 Charge.Usage、列不存在、JSON 非法、
// 档位 / 质量取不到 → 按价目表冻结（V1.0.222 的行为）。
//
//	档位   = 分辨率小写（1k / 2k / 4k），不在 tier_max_pixels 里 → 不封顶
//	质量   = 参考图 > 0 且配了 edit_quality → 它（网关对编辑固定 medium）；否则请求的质量在 quality 表里就用它，
//	         不在（空 / auto / xhigh …）→ default_quality（网关缺省 medium）；仍取不到 → 不封顶
//	输出   = quality[质量]；档位面积 > base_pixels 时 × 面积 / base_pixels，向上取到 8 位小数
//	每张   = 输出 + 参考图张数 × per_ref + aux_per_image（美元）
//	上限   = max(1, ceil(每张 × 汇率 × 加成 × 100))；汇率 / 加成与成功结算同一取值（rates），所以冻结 ≥ 实扣恒成立
//	单价   = min(价目表单价, 上限)，× 张数 —— 永远不超过价目表
//
// 例（7.2 × 1.3 = 936）：画布 1k medium 文生图 ceil(0.1061 × 936) = 100；1k high 文生图 218；1k medium + 14 张参考图 836。

// ImageUsage 本次每张出图的计费参数（只有按请求参数冻结的载体才用）。
type ImageUsage struct {
	// Quality 请求的质量 low / medium / high；空 = 调用方不传（按 default_quality）
	Quality string
	// RefImages 每张出图送网关的参考图张数（上界即可：多算只会多冻、成功后按实际退回）
	RefImages int
}

// FreezeCap 解析后的 freeze_cap_json。
type FreezeCap struct {
	Quality        map[string]*big.Rat
	DefaultQuality string
	EditQuality    string
	BasePixels     int64
	TierMaxPixels  map[string]int64
	PerRef         *big.Rat
	AuxPerImage    *big.Rat
}

// ParseFreezeCap 解析 freeze_cap_json；空 / 非法（缺字段、非 USD、负数）返回 nil, false（按价目表冻结）。
// 金额可写成字符串或数字，一律按十进制精确读。
func ParseFreezeCap(s string) (*FreezeCap, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	var raw struct {
		Currency       string                     `json:"currency"`
		Quality        map[string]json.RawMessage `json:"quality"`
		DefaultQuality string                     `json:"default_quality"`
		EditQuality    string                     `json:"edit_quality"`
		BasePixels     int64                      `json:"base_pixels"`
		TierMaxPixels  map[string]int64           `json:"tier_max_pixels"`
		PerRef         json.RawMessage            `json:"per_ref"`
		AuxPerImage    json.RawMessage            `json:"aux_per_image"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(raw.Currency), "USD") || len(raw.Quality) == 0 || raw.BasePixels <= 0 || len(raw.TierMaxPixels) == 0 {
		return nil, false
	}
	fc := &FreezeCap{Quality: map[string]*big.Rat{}, TierMaxPixels: map[string]int64{}, BasePixels: raw.BasePixels,
		DefaultQuality: lowerTrim(raw.DefaultQuality), EditQuality: lowerTrim(raw.EditQuality)}
	for k, v := range raw.Quality {
		r, ok := decimalOf(v)
		if !ok || r.Sign() <= 0 {
			return nil, false
		}
		fc.Quality[lowerTrim(k)] = r
	}
	for k, v := range raw.TierMaxPixels {
		if v <= 0 {
			return nil, false
		}
		fc.TierMaxPixels[lowerTrim(k)] = v
	}
	var ok bool
	if fc.PerRef, ok = decimalOrZero(raw.PerRef); !ok || fc.PerRef.Sign() < 0 {
		return nil, false
	}
	if fc.AuxPerImage, ok = decimalOrZero(raw.AuxPerImage); !ok || fc.AuxPerImage.Sign() < 0 {
		return nil, false
	}
	return fc, true
}

// PerImageUSD 每张上限美元；档位或质量取不到返回 nil, false（不封顶）。
func (fc *FreezeCap) PerImageUSD(resolution string, u ImageUsage) (*big.Rat, bool) {
	if fc == nil {
		return nil, false
	}
	px, ok := fc.TierMaxPixels[lowerTrim(resolution)]
	if !ok {
		return nil, false
	}
	refs := u.RefImages
	if refs < 0 {
		refs = 0
	}
	q := ""
	if refs > 0 && fc.EditQuality != "" {
		q = fc.EditQuality
	} else if req := lowerTrim(u.Quality); req != "" && fc.Quality[req] != nil {
		q = req
	} else {
		q = fc.DefaultQuality
	}
	out := fc.Quality[q]
	if out == nil {
		return nil, false
	}
	out = new(big.Rat).Set(out)
	if px > fc.BasePixels {
		out.Mul(out, big.NewRat(px, fc.BasePixels))
		out = ceilScale(out, 8)
	}
	out.Add(out, new(big.Rat).Mul(fc.PerRef, big.NewRat(int64(refs), 1)))
	out.Add(out, fc.AuxPerImage)
	return out, true
}

// UnitCapPoints 每张上限点数 max(1, ceil(美元 × 汇率 × 加成 × 100))；不封顶返回 0, false。
func (fc *FreezeCap) UnitCapPoints(resolution string, u ImageUsage, rate, markup *big.Rat) (int64, bool) {
	usd, ok := fc.PerImageUSD(resolution, u)
	if !ok || rate == nil || markup == nil {
		return 0, false
	}
	return FairPoints(usd, rate, markup), true
}

// ceilScale 向上取到 scale 位小数。
func ceilScale(x *big.Rat, scale int) *big.Rat {
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(x, new(big.Rat).SetInt(pow))
	return new(big.Rat).SetFrac(big.NewInt(ceilRat(scaled)), pow)
}

func decimalOf(raw json.RawMessage) (*big.Rat, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, false
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, false
		}
		s = strings.TrimSpace(str)
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func decimalOrZero(raw json.RawMessage) (*big.Rat, bool) {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return new(big.Rat), true
	}
	return decimalOf(raw)
}

func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ---- 载体参数（freeze_cap_json 列可能还不存在：单独查、单独降级，不影响 settle_mode 的读取）----

// freezeCapOf 载体的 freeze_cap_json（缓存 60 秒；列不存在时 10 分钟后再探）。查不到 / 非法 / 出错 → nil。
func (s *Service) freezeCapOf(ctx context.Context, modelID int64) *FreezeCap {
	if s == nil || s.conn == nil || modelID <= 0 || s.caps == nil {
		return nil
	}
	cc := s.caps
	cc.mu.Lock()
	if time.Now().Before(cc.noColumnTil) {
		cc.mu.Unlock()
		return nil
	}
	if c, ok := cc.m[modelID]; ok && time.Since(c.at) < carrierTTL {
		cc.mu.Unlock()
		return c.fc
	}
	cc.mu.Unlock()

	var raw string
	err := s.conn.QueryRowCtx(ctx, &raw,
		"SELECT COALESCE(CAST(`freeze_cap_json` AS CHAR), '') FROM `afv_ai_model` WHERE `id` = ? AND `deleted` = 0 LIMIT 1", modelID)
	var fc *FreezeCap
	switch {
	case err == nil:
		if strings.TrimSpace(raw) != "" {
			var ok bool
			if fc, ok = ParseFreezeCap(raw); !ok {
				logx.WithContext(ctx).Errorf("billing: 载体 %d 的 freeze_cap_json 非法，按价目表冻结 判据=FREEZE_CAP_JSON_VALID", modelID)
				fc = nil
			}
		}
	case errors.Is(err, sqlx.ErrNotFound):
	case isUnknownColumn(err):
		cc.mu.Lock()
		cc.noColumnTil = time.Now().Add(noColumnRecheck)
		cc.mu.Unlock()
		logx.WithContext(ctx).Infof("billing: afv_ai_model 还没有 freeze_cap_json 列（Java 迁移 V1.0.223 未上），按价目表冻结: %v", err)
		return nil
	default:
		logx.WithContext(ctx).Errorf("billing: 查载体 %d 冻结上限参数失败，本次按价目表冻结 判据=FREEZE_CAP_LOOKUP_FAILED: %v", modelID, err)
		return nil
	}
	cc.mu.Lock()
	cc.m[modelID] = cachedCap{fc: fc, at: time.Now()}
	cc.mu.Unlock()
	return fc
}

type capCache struct {
	mu          sync.Mutex
	m           map[int64]cachedCap
	noColumnTil time.Time
}

type cachedCap struct {
	fc *FreezeCap
	at time.Time
}

// applyFreezeCap 按请求参数压低冻结：载体 ACTUAL_COST + 配了参数 + Charge.Usage 非空时，载体上按张计价的步骤单价 =
// min(价目表单价, 上限)，重算小计与总价。报价（Quote / Enough / Estimate）与冻结（Freeze）都经这里，同一份 Charge 同一个数。
func (s *Service) applyFreezeCap(ctx context.Context, q *Quote, c Charge) {
	if s == nil || q == nil || c.Usage == nil || q.CarrierModelID <= 0 {
		return
	}
	ci := s.carrier(ctx, q.CarrierModelID)
	if ci.Mode != SettleActualCost {
		return
	}
	fc := s.freezeCapOf(ctx, q.CarrierModelID)
	if fc == nil {
		return
	}
	rate, markup := s.rates(ctx, ci)
	capPts, ok := fc.UnitCapPoints(q.Resolution, *c.Usage, rate, markup)
	if !ok {
		return
	}
	capQuote(q, capPts)
}

// capQuote 把载体上按张计价的收费步骤单价压到 capPts（只降不升），重算总价。
func capQuote(q *Quote, capPts int64) {
	if q == nil || capPts < 1 {
		return
	}
	var total int64
	for i := range q.Lines {
		l := &q.Lines[i]
		if l.CostOnly {
			continue
		}
		if l.ModelID == q.CarrierModelID && l.Unit != unitPerSecond && l.UnitPoints > capPts {
			if q.TierUnitPoints == 0 {
				q.TierUnitPoints = l.UnitPoints
			}
			l.UnitPoints = capPts
			units := l.Quantity
			if units <= 0 {
				units = 1
			}
			l.Points = roundPoints(float64(capPts) * float64(units) * l.Multiplier)
			q.UnitPointsCap = capPts
		}
		total += l.Points
	}
	if q.UnitPointsCap > 0 {
		q.PricePoints = total
	}
}

// roundPoints 照 priceLine：四舍五入（Java applyMultiplier HALF_UP）。
func roundPoints(x float64) int64 { return int64(math.Round(x)) }
