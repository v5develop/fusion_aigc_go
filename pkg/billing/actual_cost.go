package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 按实际用量结算（ACTUAL_COST）。
//
// 背景：GPT Image 2.6 这类按 token 计费的模型，冻结价只能按「最坏情况上限」配（大图 × 高质量 × 多参考图），
// 一张 1K medium 文生图实际 $0.0145 却按上限扣了 836 点。所以这类载体改成：
//
//	冻结   照旧按固定价（= 本次最多扣多少，上限）
//	成功   载体 settle_mode=ACTUAL_COST 且拿到 gateway 实报成本（source=reported && final）→
//	       确认 min(冻结, max(1, ceil(成本USD × 汇率 × 加价倍率 × 100)))，差额同一事务退回
//	       （PARTIAL 档位部分交付时「冻结」换成按交付量本该确认的点数，与 Java chargedAfterUndelivered 同口径）
//	       拿不到实报成本 → 按冻结全额确认，ERROR 日志 判据=ACTUAL_COST_UNAVAILABLE（之后由补退工具按实报成本补退）
//
// 共享契约（Java 迁移落库，Go 只读）：
//   - afv_ai_model.settle_mode  'FIXED' | 'ACTUAL_COST'，默认 FIXED；列不存在（Java 迁移前的库）按 FIXED
//   - afv_ai_model.cost_markup  DECIMAL，NULL = 用运行时默认倍率
//   - afv_system_config  billing_actual_cost_usd_cny_rate（默认 7.2）、billing_actual_cost_default_markup（默认 1.3）
//
// 「载体」= 选中档位的默认模型（afv_billing_offer_variant.default_model_id），与 Java 按档位选模型同口径。
const (
	SettleFixed      = "FIXED"
	SettleActualCost = "ACTUAL_COST"

	KeyUSDCNYRate    = "billing_actual_cost_usd_cny_rate"
	KeyDefaultMarkup = "billing_actual_cost_default_markup"

	DefaultUSDCNYRate = "7.2"
	DefaultMarkup     = "1.3"

	// CheckActualCostUnavailable 载体按实际用量结算、却拿不到实报成本 → 按冻结全额确认。
	CheckActualCostUnavailable = "ACTUAL_COST_UNAVAILABLE"
)

// CostFunc 取本次预留下全部 gateway 调用的供应商实报成本合计（美元）。ok=false 表示拿不到实报终值。
// 只在载体是 ACTUAL_COST 时才会被调用（FIXED 载体不多打一次 gateway）。
type CostFunc func(ctx context.Context) (usd *big.Rat, ok bool)

// Settlement 一次结算的结果。
type Settlement struct {
	// Mode 载体的结算方式 FIXED / ACTUAL_COST
	Mode string
	// Reserved 冻结点数
	Reserved int64
	// Points 本次确认扣除的点数（幂等命中已结算的单子时为 0、Already=true）
	Points int64
	// ActualCost 是否按实报成本结算了（false = 按冻结 / 实际产出量结算）
	ActualCost bool
	// Already 单子此前已结算（幂等）
	Already bool
}

// FairPoints 按实报成本算的点数：max(1, ceil(usd × rate × markup × 100))。全程有理数运算，不吃浮点误差。
func FairPoints(usd, rate, markup *big.Rat) int64 {
	if usd == nil || rate == nil || markup == nil {
		return 1
	}
	x := new(big.Rat).Mul(usd, rate)
	x.Mul(x, markup)
	x.Mul(x, big.NewRat(100, 1))
	n := ceilRat(x)
	if n < 1 {
		return 1
	}
	return n
}

// ActualPoints 按实际用量应确认的点数：min(冻结, FairPoints)。
func ActualPoints(reserved int64, usd, rate, markup *big.Rat) int64 {
	return min64(reserved, FairPoints(usd, rate, markup))
}

func ceilRat(x *big.Rat) int64 {
	q, m := new(big.Int).QuoRem(x.Num(), x.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 1<<62 - 1
	}
	return q.Int64()
}

// ParseDecimal 十进制字符串 → 有理数；空串 / 非法 / 非正数返回 false。
func ParseDecimal(s string) (*big.Rat, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return nil, false
	}
	return r, true
}

func mustRat(s string) *big.Rat {
	r, _ := ParseDecimal(s)
	return r
}

// ---- 运行时参数（afv_system_config，与 Java SettingKey 同表同键）----

const knobTTL = 60 * time.Second

// ratKnob afv_system_config 里的正十进制参数：没有该行 / 值非法用默认值；查库失败沿用上次的值。缓存 60 秒。
type ratKnob struct {
	key string
	def *big.Rat
	mu  sync.Mutex
	val *big.Rat
	at  time.Time
}

func (k *ratKnob) get(ctx context.Context, conn sqlx.SqlConn) *big.Rat {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.val != nil && time.Since(k.at) < knobTTL {
		return k.val
	}
	k.at = time.Now()
	if k.val == nil {
		k.val = k.def
	}
	if conn == nil {
		return k.val
	}
	var v string
	err := conn.QueryRowCtx(ctx, &v, "SELECT COALESCE(config_value, '') FROM afv_system_config WHERE config_key = ? AND deleted = 0 LIMIT 1", k.key)
	switch {
	case errors.Is(err, sqlx.ErrNotFound) || errors.Is(err, sql.ErrNoRows):
		k.val = k.def
	case err != nil:
		logx.WithContext(ctx).Errorf("billing: 读运行时参数 %s 失败，沿用 %s: %v", k.key, k.val.FloatString(4), err)
	default:
		if r, ok := ParseDecimal(v); ok {
			k.val = r
		} else {
			logx.WithContext(ctx).Errorf("billing: 运行时参数 %s=%q 不是正数，用默认 %s", k.key, v, k.def.FloatString(4))
			k.val = k.def
		}
	}
	return k.val
}

// ---- 载体结算方式 ----

// carrierInfo 载体模型的结算方式与加价倍率（nil = 用默认倍率）。
type carrierInfo struct {
	ModelID int64
	Mode    string
	Markup  *big.Rat
}

// carrierCache 载体结算方式的进程内缓存（报价接口频繁，60 秒）；settle_mode 列不存在时整体按 FIXED，10 分钟后再探一次。
type carrierCache struct {
	mu          sync.Mutex
	m           map[int64]cachedCarrier
	noColumnTil time.Time
}

type cachedCarrier struct {
	info carrierInfo
	at   time.Time
}

const (
	carrierTTL       = 60 * time.Second
	noColumnRecheck  = 10 * time.Minute
	mysqlUnknownCol  = 1054
	mysqlNoSuchTable = 1146
)

func isUnknownColumn(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == mysqlUnknownCol || me.Number == mysqlNoSuchTable)
}

// carrier 按模型 id 取结算方式。查不到 / 列不存在 / 查库出错 → FIXED（出错只记日志：宁可按冻结价扣，事后补退，
// 也不在结算路径上报错卡住交付）。
func (s *Service) carrier(ctx context.Context, modelID int64) carrierInfo {
	fixed := carrierInfo{ModelID: modelID, Mode: SettleFixed}
	if s == nil || s.conn == nil || modelID <= 0 {
		return fixed
	}
	cc := s.carriers
	cc.mu.Lock()
	if time.Now().Before(cc.noColumnTil) {
		cc.mu.Unlock()
		return fixed
	}
	if c, ok := cc.m[modelID]; ok && time.Since(c.at) < carrierTTL {
		cc.mu.Unlock()
		return c.info
	}
	cc.mu.Unlock()

	var row struct {
		Mode   sql.NullString `db:"settle_mode"`
		Markup sql.NullString `db:"cost_markup"`
	}
	err := s.conn.QueryRowCtx(ctx, &row,
		"SELECT `settle_mode`, `cost_markup` FROM `afv_ai_model` WHERE `id` = ? AND `deleted` = 0 LIMIT 1", modelID)
	info := fixed
	switch {
	case err == nil:
		if strings.EqualFold(strings.TrimSpace(row.Mode.String), SettleActualCost) {
			info.Mode = SettleActualCost
		}
		if row.Markup.Valid {
			if m, ok := ParseDecimal(row.Markup.String); ok {
				info.Markup = m
			}
		}
	case errors.Is(err, sqlx.ErrNotFound):
	case isUnknownColumn(err):
		cc.mu.Lock()
		cc.noColumnTil = time.Now().Add(noColumnRecheck)
		cc.mu.Unlock()
		logx.WithContext(ctx).Infof("billing: afv_ai_model 还没有 settle_mode / cost_markup 列（Java 迁移未上），全部按 FIXED 结算: %v", err)
		return fixed
	default:
		logx.WithContext(ctx).Errorf("billing: 查载体 %d 结算方式失败，本次按 FIXED 判据=ACTUAL_COST_LOOKUP_FAILED: %v", modelID, err)
		return fixed // 不缓存，下次再查
	}
	cc.mu.Lock()
	cc.m[modelID] = cachedCarrier{info: info, at: time.Now()}
	cc.mu.Unlock()
	return info
}

// carrierOfReservation 预留单的载体：快照里的 carrierModelId（新单）；旧单没有就按快照档位码查档位默认模型。
func (s *Service) carrierOfReservation(ctx context.Context, r *Reservation) carrierInfo {
	id := r.carrierModelID
	if id <= 0 && r.variantCode != "" && s.conn != nil {
		var mid int64
		err := s.conn.QueryRowCtx(ctx, &mid,
			"SELECT COALESCE(`default_model_id`, 0) FROM `afv_billing_offer_variant` WHERE `variant_code` = ? AND `deleted` = 0 LIMIT 1", r.variantCode)
		if err != nil && !errors.Is(err, sqlx.ErrNotFound) {
			logx.WithContext(ctx).Errorf("billing: 查档位 %s 的载体失败，按 FIXED: %v", r.variantCode, err)
		}
		id = mid
	}
	return s.carrier(ctx, id)
}

// rates 当前汇率与该载体的加价倍率。
func (s *Service) rates(ctx context.Context, ci carrierInfo) (rate, markup *big.Rat) {
	rate = s.rateKnob.get(ctx, s.conn)
	markup = ci.Markup
	if markup == nil {
		markup = s.markupKnob.get(ctx, s.conn)
	}
	return rate, markup
}

// settleModeOf 报价用：这次计费的载体按什么方式结算。拿不到按 FIXED。
func (s *Service) settleModeOf(ctx context.Context, q *Quote) string {
	if q == nil {
		return SettleFixed
	}
	return s.carrier(ctx, q.CarrierModelID).Mode
}

// SettleModeOf 预留单载体的结算方式（提交接口回给前端用）。nil / shadow 单返回空串。
func (s *Service) SettleModeOf(ctx context.Context, r *Reservation) string {
	if s == nil || r == nil || r.shadow {
		return ""
	}
	return s.carrierOfReservation(ctx, r).Mode
}

// ---- 结算 ----

// SettleCost 与 Settle 相同（交付前确认扣费，返回 nil 才允许交付），另外按载体结算方式决定确认多少：
//   - FIXED：按实际产出量 actual 确认（与 Settle 完全一致）；
//   - ACTUAL_COST：cost 拿得到实报成本 → 确认 min(按 actual 应确认的点数, 公平点数)、差额退回；拿不到（含 cost=nil）→ 按 actual 确认并
//     ERROR 日志 判据=ACTUAL_COST_UNAVAILABLE。
func (s *Service) SettleCost(ctx context.Context, userID int64, requestID string, actual int, cost CostFunc) (*Settlement, error) {
	if s == nil || s.conf.Mode != ModeOn {
		return &Settlement{Mode: SettleFixed}, nil
	}
	if requestID == "" {
		return nil, fmt.Errorf("%w: 缺少请求号", ErrGone)
	}
	bizID := Charge{UserID: userID, RequestID: requestID}.bizID()
	var status string
	err := s.conn.QueryRowCtx(ctx, &status,
		"SELECT `status` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ? AND `deleted` = 0 LIMIT 1",
		bizTypeLocalLife, bizID)
	if errors.Is(err, sqlx.ErrNotFound) {
		return nil, fmt.Errorf("%w: 找不到预留单 %s", ErrGone, bizID)
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case statusHeld:
	case statusConfirmed, statusPartiallyRefunded:
		return &Settlement{Mode: SettleFixed, Already: true}, nil
	default:
		return nil, fmt.Errorf("%w: %s 当前是 %s", ErrGone, bizID, status)
	}
	r, err := loadHeld(ctx, s.conn, bizID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		// 查状态与捞单之间被别人改了状态：重读一次。并发的另一次确认已扣除 → 幂等成功（Already），
		// 不能误报 ErrGone（调用方会据此拒绝交付、甚至退款）；其余（被回收 / 已退回）才是 ErrGone，不交付。
		var now string
		rerr := s.conn.QueryRowCtx(ctx, &now,
			"SELECT `status` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ? AND `deleted` = 0 LIMIT 1",
			bizTypeLocalLife, bizID)
		switch {
		case rerr == nil && (now == statusConfirmed || now == statusPartiallyRefunded):
			return &Settlement{Mode: SettleFixed, Already: true}, nil
		case rerr != nil && !errors.Is(rerr, sqlx.ErrNotFound):
			// 重读失败：保持原口径按 ErrGone（不交付，调用方下轮再核），不把查库错误当成已扣除
			return nil, fmt.Errorf("%w: %s 状态已变化（重读失败: %v）", ErrGone, bizID, rerr)
		}
		return nil, fmt.Errorf("%w: %s 状态已变化（现在是 %q）", ErrGone, bizID, now)
	}
	if r.shadow {
		return &Settlement{Mode: SettleFixed}, nil
	}
	out := &Settlement{Mode: SettleFixed, Reserved: r.PricePoints}
	ci := s.carrierOfReservation(ctx, r)
	out.Mode = ci.Mode
	if ci.Mode == SettleActualCost {
		var (
			usd *big.Rat
			ok  bool
		)
		if cost != nil {
			usd, ok = cost(ctx)
		}
		if ok {
			rate, markup := s.rates(ctx, ci)
			// 上限 = 按实际交付量本该确认的点数（PARTIAL 档位部分成功时已扣掉未交付的份数），与 Java confirmByActualCost 的
			// chargedAfterUndelivered 同口径：按实际用量结算只会比固定价更便宜，不能因为失败但被供应商计费的尝试多扣
			capPts, _ := splitActual(r, r.PricePoints, actual)
			points := ActualPoints(capPts, usd, rate, markup)
			done, err := settlePoints(ctx, s.conn, r, points, "按实际用量确认消费", "按实际用量结算，退回差额")
			if err != nil {
				return nil, err
			}
			out.Points, out.ActualCost, out.Already = points, true, !done
			logx.WithContext(ctx).Infow("billing actual-cost settle",
				logx.Field("reservation_no", r.No), logx.Field("user_id", userID), logx.Field("carrier_model_id", ci.ModelID),
				logx.Field("cost_usd", usd.FloatString(6)), logx.Field("usd_cny_rate", rate.FloatString(4)),
				logx.Field("markup", markup.FloatString(4)), logx.Field("reserved", r.PricePoints), logx.Field("cap", capPts), logx.Field("confirmed", points),
				logx.Field("refunded", r.PricePoints-points))
			return out, nil
		}
		logx.WithContext(ctx).Errorw(fmt.Sprintf("billing: 按实际用量结算拿不到实报成本，按冻结价确认 判据=%s", CheckActualCostUnavailable),
			logx.Field("check", CheckActualCostUnavailable), logx.Field("reservation_no", r.No), logx.Field("user_id", userID),
			logx.Field("carrier_model_id", ci.ModelID), logx.Field("reserved", r.PricePoints), logx.Field("has_cost_source", cost != nil))
	}
	consume, _ := splitActual(r, r.PricePoints, actual)
	done, err := settlePoints(ctx, s.conn, r, consume, "按实际交付量确认消费", "未交付部分退回")
	if err != nil {
		return nil, err
	}
	out.Points, out.Already = consume, !done
	return out, nil
}

// ConfirmPoints 按点数确认（部分确认）：确认 points（夹到 [0, 冻结]），差额同一事务退回。幂等：已结算的单子直接返回 nil。
func (s *Service) ConfirmPoints(ctx context.Context, r *Reservation, points int64) error {
	if r == nil || r.shadow {
		return nil
	}
	_, err := settlePoints(ctx, s.conn, r, points, "按实际用量确认消费", "按实际用量结算，退回差额")
	return err
}
