package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 算价链路（照 Java 的配置模型走，我们只读不写）：
//
//	afv_billing_offer            entry_code(驼峰) → offer_code(下划线)
//	  └ afv_billing_offer_variant   offer_code → variant_code，一个档位一条
//	      └ afv_billing_charge_component  一个档位若干计费步骤
//	          └ afv_model_pricing         (model_id, action_type, resolution) → 单价
//
// ⚠️ 坑：offer 上有两个码。`entry_code` 是驼峰（hitVideo），`variant.offer_code`
// 是下划线（hit_video），两者**不能直接 join**——我全表比对过，32 个 offer 的
// entry_code 没有一个能匹配上 variant.offer_code。必须经 offer.offer_code 中转。
//
// 单条步骤的价钱：
//
//	unit  = model_pricing.price_points；查不到（或步骤没有 action_type）才退回 base_points
//	units = 张数/份数（Java 的 quantity / billedUnits），默认 1
//	小计  = round(unit * units * (PER_SECOND ? 秒数 : 1) * price_multiplier)
//
// ⚠️ 三处细节照 Java 抄的（BillingPricingServiceImpl）：
//   - **按秒也要乘份数**：它是 `单价 × quantity`，再 `× 秒数`，不是只乘秒数；
//   - 倍率用**四舍五入**（applyMultiplier → round(HALF_UP)），不是向上取整；
//   - base_points 是**兜底**不是优先：只有 action_type 为空、
//     或按 (model_id, action_type) 查不到单价行时才用它，两者都没有就直接报错。
//
// 只算 cost_type='BILLED' 的；COST_ONLY 是只记成本不收用户钱的步骤（如复刻的视频解析）。
//
// ⚠️ **计量看 `price_unit`，不看 `component_type`**。
// 现有配置里两者会打架：action_transfer 的步骤是 `OUTPUT`（按产出数量），
// 单价却是 `PER_SECOND`（按秒）。按 component_type 算会把 15 秒的视频算成 1 次，
// 15 倍的差价。单价单位才是权威的——"8 积分/秒"这句话里的"秒"就在 price_unit 上。
// component_type 只在没有单价行（用 base_points 固定点数）时才拿来判断。

// Quote 一次报价的结果。
type Quote struct {
	EntryCode   string
	OfferCode   string
	FeatureCode string
	VariantCode string
	// ModelGroup 按模型组选中档位时的组码（Charge.ModelGroup 命中了分模型档位）；退回默认档位时为空
	ModelGroup  string
	ModelID     int64
	PricingID   int64 // 主计费步骤用的单价行，写进预留单
	PricePoints int64
	PriceUnit   string // PER_IMAGE / PER_SECOND
	Quantity    int    // 计量：张数或秒数
	// Settlement ALL_OR_NOTHING / PARTIAL。PARTIAL 才允许部分成功按实际结算。
	Settlement string
	Lines      []QuoteLine
}

// QuoteLine 一个计费步骤的明细，后面一比一写进 afv_billing_reservation_line。
type QuoteLine struct {
	ComponentCode string
	StepCode      string
	Mode          sql.NullInt64 // AIGC 执行面的 task_type
	ModelID       int64
	PricingID     int64
	UnitPoints    int64
	Unit          string // PER_SECOND / PER_IMAGE / PER_CALL
	Quantity      int
	Multiplier    float64
	Points        int64
	CostOnly      bool
}

// Snapshot 价格快照，落进 afv_billing_reservation.price_snapshot。
// 单价以后会改，账要能还原成当时的样子。
func (q *Quote) Snapshot() string {
	b, err := json.Marshal(map[string]any{
		"entryCode": q.EntryCode, "offerCode": q.OfferCode, "variantCode": q.VariantCode,
		"modelGroup": q.ModelGroup, "modelId": q.ModelID, "priceUnit": q.PriceUnit, "quantity": q.Quantity,
		"pricePoints": q.PricePoints, "settlement": q.Settlement, "lines": q.Lines,
		"source": "aigc-local-life", // 一眼看出这单是 Go 写的
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

type offerRow struct {
	OfferCode   string `db:"offer_code"`
	EntryCode   string `db:"entry_code"`
	FeatureCode string `db:"feature_code"`
}

type variantRow struct {
	VariantCode    string         `db:"variant_code"`
	QualityTier    sql.NullString `db:"quality_tier"`
	DefaultModelID sql.NullInt64  `db:"default_model_id"`
	DefaultRes     sql.NullString `db:"default_resolution"`
	DefaultSeconds sql.NullInt64  `db:"default_duration_seconds"`
	Settlement     sql.NullString `db:"settlement_type"`
}

type componentRow struct {
	ComponentCode string          `db:"component_code"`
	StepCode      sql.NullString  `db:"step_code"`
	ComponentType sql.NullString  `db:"component_type"`
	Mode          sql.NullInt64   `db:"mode"`
	CostType      sql.NullString  `db:"cost_type"`
	ActionType    sql.NullString  `db:"action_type"`
	ModelID       sql.NullInt64   `db:"model_id"`
	BasePoints    sql.NullInt64   `db:"base_points"`
	Multiplier    sql.NullFloat64 `db:"price_multiplier"`
}

type pricingRow struct {
	Id          int64          `db:"id"`
	PricePoints int64          `db:"price_points"`
	PriceUnit   string         `db:"price_unit"`
	Resolution  sql.NullString `db:"resolution"`
}

// price 把一次 Charge 算成点数。
func price(ctx context.Context, conn sqlx.SqlConn, c Charge) (*Quote, error) {
	if strings.TrimSpace(c.EntryCode) == "" {
		return nil, errors.New("billing: 缺少 EntryCode")
	}
	var o offerRow
	const oq = "SELECT `offer_code`,`entry_code`,`feature_code` FROM `afv_billing_offer` " +
		"WHERE `entry_code` = ? AND `status` = 'ENABLED' AND `deleted` = 0 LIMIT 1"
	switch err := conn.QueryRowCtx(ctx, &o, oq, c.EntryCode); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, fmt.Errorf("%w: %s", ErrNoOffer, c.EntryCode)
	case err != nil:
		return nil, err
	}

	v, group, err := pickVariant(ctx, conn, o.OfferCode, c.ModelCode, c.ModelGroup, c.Tier)
	if err != nil {
		return nil, err
	}

	var comps []componentRow
	const cq = "SELECT `component_code`,`step_code`,`component_type`,`mode`,`cost_type`,`action_type`," +
		"`model_id`,`base_points`,`price_multiplier` FROM `afv_billing_charge_component` " +
		"WHERE `variant_code` = ? AND `deleted` = 0 ORDER BY `sort`, `id`"
	if err := conn.QueryRowsCtx(ctx, &comps, cq, v.VariantCode); err != nil {
		return nil, err
	}
	if len(comps) == 0 {
		// 档位建了但没配计费步骤。与其算出 0 点让用户白嫖，不如明确报错，逼配置补上。
		return nil, fmt.Errorf("%w: %s 的档位 %s 没有计费步骤", ErrNoOffer, c.EntryCode, v.VariantCode)
	}

	// Java 算完基础价还会过一层规则引擎，我们没实现——有规则命中就拒绝算，别算出个不一样的价
	if err := checkNoPricingRules(ctx, conn, o.OfferCode, v.VariantCode); err != nil {
		return nil, err
	}

	res := c.Resolution
	if res == "" {
		res = v.DefaultRes.String
	}
	q := &Quote{
		EntryCode: o.EntryCode, OfferCode: o.OfferCode, FeatureCode: o.FeatureCode,
		VariantCode: v.VariantCode, ModelGroup: group, ModelID: v.DefaultModelID.Int64,
		Settlement: or(v.Settlement.String, "ALL_OR_NOTHING"),
	}
	for _, cp := range comps {
		line, err := priceLine(ctx, conn, cp, c, v, res)
		if err != nil {
			return nil, err
		}
		q.Lines = append(q.Lines, *line)
		if line.CostOnly {
			continue
		}
		q.PricePoints += line.Points
		// 主步骤（第一条计费的）决定对外的计量单位与单价行
		if q.PricingID == 0 {
			q.PricingID, q.PriceUnit, q.Quantity = line.PricingID, line.Unit, line.Quantity
			if line.ModelID != 0 {
				q.ModelID = line.ModelID
			}
		}
	}
	// 算出 0 点 = 配置有洞（单价写成 0、步骤全是只记成本、没有收费步骤），不能当"免费"放过去。
	if q.PricePoints <= 0 {
		return nil, fmt.Errorf("%w: %s 的档位 %s 算出 %d 点，没有有效的收费步骤或单价为 0",
			ErrNoOffer, c.EntryCode, v.VariantCode, q.PricePoints)
	}
	return q, nil
}

// pickVariant 选档位，按这个顺序：
//
//  1. 给了 modelCode  → 按档位的默认模型匹配
//  2. 给了 modelGroup → 按档位默认模型的 gateway_model_group 匹配；没有这个组的档位**不报错**，往下走 3 / 4
//     （该入口还没配分模型价、或任务是换代前的旧组，按原来的档位扣，与没传组时完全一样）
//  3. 给了 tier       → 先找 <offer_code>_<tier> 这个档位码，再找 quality_tier
//  4. 都没给          → 第一个启用的
//
// 常用的是 2：一个 offer 下的多档差别就是模型，而两边的模型码对不上
// （见 Charge.Tier 的注释），按档次选才稳。
//
// 选不到就报错，**不退回"第一个档位"**——那会把用户选的 lite 按云端价扣。
func pickVariant(ctx context.Context, conn sqlx.SqlConn, offerCode, modelCode, modelGroup, tier string) (_ *variantRow, matchedGroup string, _ error) {
	const cols = "`variant_code`,`quality_tier`,`default_model_id`,`default_resolution`," +
		"`default_duration_seconds`,`settlement_type`"
	const vcols = "v.`variant_code`,v.`quality_tier`,v.`default_model_id`,v.`default_resolution`," +
		"v.`default_duration_seconds`,v.`settlement_type`"
	if strings.TrimSpace(modelCode) != "" {
		var v variantRow
		const q = "SELECT " + cols + " FROM `afv_billing_offer_variant` v " +
			"WHERE v.`offer_code` = ? AND v.`status` = 'ENABLED' AND v.`deleted` = 0 " +
			"AND v.`default_model_id` = (SELECT m.`id` FROM `afv_ai_model` m WHERE m.`code` = ? AND m.`deleted` = 0 LIMIT 1) " +
			"ORDER BY v.`id` LIMIT 1"
		switch err := conn.QueryRowCtx(ctx, &v, q, offerCode, modelCode); {
		case err == nil:
			return &v, "", nil
		case !errors.Is(err, sqlx.ErrNotFound):
			return nil, "", err
		}
		return nil, "", fmt.Errorf("%w: %s 下没有模型 %s 的档位", ErrNoOffer, offerCode, modelCode)
	}
	if g := strings.TrimSpace(modelGroup); g != "" {
		var v variantRow
		const q = "SELECT " + vcols + " FROM `afv_billing_offer_variant` v " +
			"JOIN `afv_ai_model` m ON m.`id` = v.`default_model_id` AND m.`deleted` = 0 " +
			"WHERE v.`offer_code` = ? AND v.`status` = 'ENABLED' AND v.`deleted` = 0 AND m.`gateway_model_group` = ? " +
			"ORDER BY v.`id` LIMIT 1"
		switch err := conn.QueryRowCtx(ctx, &v, q, offerCode, g); {
		case err == nil:
			return &v, g, nil
		case !errors.Is(err, sqlx.ErrNotFound):
			return nil, "", err
		}
		// 没有这个组的档位：按 tier / 第一个档位选（见函数注释第 2 条）
	}
	if tier = strings.TrimSpace(tier); tier != "" {
		var v variantRow
		// 档位码优先（digital_human_cloud / digital_human_lite），退回 quality_tier
		const q = "SELECT " + cols + " FROM `afv_billing_offer_variant` " +
			"WHERE `offer_code` = ? AND `status` = 'ENABLED' AND `deleted` = 0 " +
			"AND (`variant_code` = ? OR `quality_tier` = ?) " +
			"ORDER BY (`variant_code` = ?) DESC, `id` LIMIT 1"
		full := offerCode + "_" + tier
		switch err := conn.QueryRowCtx(ctx, &v, q, offerCode, full, tier, full); {
		case err == nil:
			return &v, "", nil
		case !errors.Is(err, sqlx.ErrNotFound):
			return nil, "", err
		}
		return nil, "", fmt.Errorf("%w: %s 下没有 %s 档（找过档位码 %s 和 quality_tier）",
			ErrNoOffer, offerCode, tier, full)
	}
	var v variantRow
	const q = "SELECT " + cols + " FROM `afv_billing_offer_variant` " +
		"WHERE `offer_code` = ? AND `status` = 'ENABLED' AND `deleted` = 0 ORDER BY `id` LIMIT 1"
	switch err := conn.QueryRowCtx(ctx, &v, q, offerCode); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, "", fmt.Errorf("%w: %s 没有启用的档位", ErrNoOffer, offerCode)
	case err != nil:
		return nil, "", err
	}
	return &v, "", nil
}

// priceLine 算一个计费步骤。
func priceLine(ctx context.Context, conn sqlx.SqlConn, cp componentRow, c Charge, v *variantRow, res string) (*QuoteLine, error) {
	line := &QuoteLine{
		ComponentCode: cp.ComponentCode, StepCode: cp.StepCode.String, Mode: cp.Mode,
		ModelID:    pick(cp.ModelID.Int64, v.DefaultModelID.Int64),
		Multiplier: or1(cp.Multiplier.Float64),
		CostOnly:   cp.CostType.String == "COST_ONLY",
	}
	if line.CostOnly {
		// 只记成本不收用户钱（比如复刻里的视频解析），不用算量
		line.Unit = unitOf(cp.ComponentType.String)
		return line, nil
	}
	// 先查单价；查不到或步骤没有 action_type，才退回 base_points
	var p *pricingRow
	if strings.TrimSpace(cp.ActionType.String) != "" && line.ModelID != 0 {
		var err error
		if p, err = findPricing(ctx, conn, line.ModelID, cp.ActionType.String, res); err != nil && !errors.Is(err, ErrNoOffer) {
			return nil, err
		}
	}
	switch {
	case p != nil:
		line.UnitPoints, line.PricingID, line.Unit = p.PricePoints, p.Id, p.PriceUnit
	case cp.BasePoints.Int64 > 0:
		// 固定点数的步骤没有单价行，计量单位只能退回去看 component_type
		line.UnitPoints, line.Unit = cp.BasePoints.Int64, unitOf(cp.ComponentType.String)
	default:
		return nil, fmt.Errorf("%w: 步骤 %s 既没有单价行也没有 base_points", ErrNoOffer, cp.ComponentCode)
	}

	units := c.Count // Java 的 quantity / billedUnits
	if units <= 0 {
		units = 1
	}
	raw := float64(line.UnitPoints) * float64(units)
	line.Quantity = units
	if line.Unit == unitPerSecond {
		secs := billableSeconds(c, v)
		raw *= float64(secs)
		line.Quantity = secs
	}
	line.Points = int64(math.Round(raw * line.Multiplier)) // 照 Java：四舍五入，不是向上取整
	return line, nil
}

// findPricing 取单价。先按分辨率精确匹配，没有再取不限分辨率的那条。
//
// 为什么要兜底：afv_model_pricing 里有的模型分 480p/720p/1080p 三行（模型 4、5），
// 有的只有一行且 resolution 为 NULL（模型 13）。只按分辨率查会漏。
func findPricing(ctx context.Context, conn sqlx.SqlConn, modelID int64, action, res string) (*pricingRow, error) {
	if modelID == 0 {
		return nil, errors.New("billing: 计费步骤没有模型，算不出单价")
	}
	var p pricingRow
	const q = "SELECT `id`,`price_points`,`price_unit`,`resolution` FROM `afv_model_pricing` " +
		"WHERE `model_id` = ? AND `action_type` = ? AND `status` = 1 AND `deleted` = 0 " +
		"AND (`effective_time` IS NULL OR `effective_time` <= NOW()) " +
		"AND (`expire_time` IS NULL OR `expire_time` > NOW()) " +
		// 分辨率对得上的排前面，其次是不限分辨率的
		"ORDER BY (`resolution` = ?) DESC, (`resolution` IS NULL) DESC, `id` LIMIT 1"
	switch err := conn.QueryRowCtx(ctx, &p, q, modelID, action, res); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, fmt.Errorf("%w: 模型 %d 的 %s 没有单价", ErrNoOffer, modelID, action)
	case err != nil:
		return nil, err
	}
	return &p, nil
}

// billableSeconds 按秒计费时用几秒：请求里的 → 档位默认 → 兜底 15。
// 兜底值照 Java 的 DEFAULT_VIDEO_DURATION_SECONDS（它这时也会打 WARN）。
func billableSeconds(c Charge, v *variantRow) int {
	if c.Seconds > 0 {
		return c.Seconds
	}
	if v.DefaultSeconds.Valid && v.DefaultSeconds.Int64 > 0 {
		return int(v.DefaultSeconds.Int64)
	}
	logx.Errorf("billing: %s 既没传时长、档位也没有默认时长，按 %d 秒算", v.VariantCode, defaultVideoSeconds)
	return defaultVideoSeconds
}

// 计价单位，取值和 Java 的 BillingPricingServiceImpl 一致。
// PER_IMAGE / PER_REQUEST / PER_VIDEO 都是「单价 × 份数」，只有 PER_SECOND 额外乘时长。
const (
	unitPerSecond  = "PER_SECOND"
	unitPerImage   = "PER_IMAGE"
	unitPerRequest = "PER_REQUEST" // 按次：AI文案用这个
	unitPerVideo   = "PER_VIDEO"
	unitPerCall    = "PER_CALL" // 只在没有单价行、拿 component_type 猜的时候用

	defaultVideoSeconds = 15
)

// unitOf 没有单价行时的兜底：拿 component_type 猜计量单位。
func unitOf(componentType string) string {
	switch componentType {
	case "DURATION":
		return unitPerSecond
	case "OUTPUT":
		return unitPerImage
	default: // BASE：固定一次
		return unitPerCall
	}
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func or1(f float64) float64 {
	if f <= 0 {
		return 1
	}
	return f
}

func pick(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
