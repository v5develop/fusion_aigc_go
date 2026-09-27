package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

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
//	unit = base_points > 0 ? base_points : model_pricing.price_points
//	qty  = OUTPUT → 张数；DURATION → 秒数；BASE → 1
//	小计 = ceil(unit * qty * price_multiplier)
//
// 只算 cost_type='BILLED' 的；COST_ONLY 是只记成本不收用户钱的步骤（如复刻的视频解析）。

// Quote 一次报价的结果。
type Quote struct {
	EntryCode   string
	OfferCode   string
	FeatureCode string
	VariantCode string
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
		"modelId": q.ModelID, "priceUnit": q.PriceUnit, "quantity": q.Quantity,
		"pricePoints": q.PricePoints, "settlement": q.Settlement, "lines": q.Lines,
		"source": "aigc-life", // 一眼看出这单是 Go 写的
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

	v, err := pickVariant(ctx, conn, o.OfferCode, c.ModelCode)
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
		// actionTransfer 就是这样：offer 有、variant 和 component 全空。
		// 与其算出 0 点让用户白嫖，不如明确报错，逼配置补上。
		return nil, fmt.Errorf("%w: %s 的档位 %s 没有计费步骤", ErrNoOffer, c.EntryCode, v.VariantCode)
	}

	res := c.Resolution
	if res == "" {
		res = v.DefaultRes.String
	}
	q := &Quote{
		EntryCode: o.EntryCode, OfferCode: o.OfferCode, FeatureCode: o.FeatureCode,
		VariantCode: v.VariantCode, ModelID: v.DefaultModelID.Int64,
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
			q.PricingID, q.PriceUnit, q.Quantity = line.PricingID, unitOf(cp.ComponentType.String), line.Quantity
			if line.ModelID != 0 {
				q.ModelID = line.ModelID
			}
		}
	}
	return q, nil
}

// pickVariant 选档位。modelCode 指定了就按模型选，否则取第一个启用的。
//
// 为什么按模型选：一个 offer 下的多个 variant 差别就是模型
// （hit_video 有 standard / seedance_2.0 / seedance_2.0_fast 三档）。
func pickVariant(ctx context.Context, conn sqlx.SqlConn, offerCode, modelCode string) (*variantRow, error) {
	const cols = "`variant_code`,`quality_tier`,`default_model_id`,`default_resolution`," +
		"`default_duration_seconds`,`settlement_type`"
	if strings.TrimSpace(modelCode) != "" {
		var v variantRow
		const q = "SELECT " + cols + " FROM `afv_billing_offer_variant` v " +
			"WHERE v.`offer_code` = ? AND v.`status` = 'ENABLED' AND v.`deleted` = 0 " +
			"AND v.`default_model_id` = (SELECT m.`id` FROM `afv_ai_model` m WHERE m.`code` = ? AND m.`deleted` = 0 LIMIT 1) " +
			"ORDER BY v.`id` LIMIT 1"
		switch err := conn.QueryRowCtx(ctx, &v, q, offerCode, modelCode); {
		case err == nil:
			return &v, nil
		case !errors.Is(err, sqlx.ErrNotFound):
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s 下没有模型 %s 的档位", ErrNoOffer, offerCode, modelCode)
	}
	var v variantRow
	const q = "SELECT " + cols + " FROM `afv_billing_offer_variant` " +
		"WHERE `offer_code` = ? AND `status` = 'ENABLED' AND `deleted` = 0 ORDER BY `id` LIMIT 1"
	switch err := conn.QueryRowCtx(ctx, &v, q, offerCode); {
	case errors.Is(err, sqlx.ErrNotFound):
		return nil, fmt.Errorf("%w: %s 没有启用的档位", ErrNoOffer, offerCode)
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// priceLine 算一个计费步骤。
func priceLine(ctx context.Context, conn sqlx.SqlConn, cp componentRow, c Charge, v *variantRow, res string) (*QuoteLine, error) {
	line := &QuoteLine{
		ComponentCode: cp.ComponentCode, StepCode: cp.StepCode.String, Mode: cp.Mode,
		ModelID:    pick(cp.ModelID.Int64, v.DefaultModelID.Int64),
		Multiplier: or1(cp.Multiplier.Float64),
		Quantity:   quantityOf(cp.ComponentType.String, c, v),
		CostOnly:   cp.CostType.String == "COST_ONLY",
	}
	if line.CostOnly {
		return line, nil
	}
	if cp.BasePoints.Int64 > 0 {
		// 固定点数的步骤，不查单价表
		line.UnitPoints = cp.BasePoints.Int64
	} else {
		p, err := findPricing(ctx, conn, line.ModelID, cp.ActionType.String, res)
		if err != nil {
			return nil, err
		}
		line.UnitPoints, line.PricingID = p.PricePoints, p.Id
	}
	line.Points = int64(math.Ceil(float64(line.UnitPoints) * float64(line.Quantity) * line.Multiplier))
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

// quantityOf 这个步骤按什么计量。
func quantityOf(componentType string, c Charge, v *variantRow) int {
	switch componentType {
	case "DURATION":
		if c.Seconds > 0 {
			return c.Seconds
		}
		if v.DefaultSeconds.Valid && v.DefaultSeconds.Int64 > 0 {
			return int(v.DefaultSeconds.Int64)
		}
		return 1
	case "OUTPUT":
		if c.Count > 0 {
			return c.Count
		}
		return 1
	default: // BASE：固定一次
		return 1
	}
}

func unitOf(componentType string) string {
	if componentType == "DURATION" {
		return "PER_SECOND"
	}
	return "PER_IMAGE"
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
