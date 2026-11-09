// Package usertier 用户分层（免费 / 付费）判定：与 Java 主项目 UserTierService + WatermarkPolicy 同一判据，直接读共享库。
//
// # 判据（与 Java 逐条一致，改这里必须同步改 Java，反之亦然）
//
//   - 付过钱（HasEverPaid）：afv_points_batch 里存在该用户的一行，source ∈ PaidSources（RECHARGE / UNION_RECHARGE，
//     即 Java PointsBatchSourceEnum.paidSources()），且 deleted = 0。只看「有没有过」，不看余额、过期、金额 ——
//     付费是既成事实，点数用完也不掉回免费。赠送（REGISTER_GIFT / CAMPAIGN_GIFT / ADMIN_GIFT）、ADMIN_ADJUST、REFUND
//     都不算付费。不看分层开关（Java hasEverPaid 同样不看）。
//   - 免费档（IsFree）：分层开关开（afv_system_config.user_tier.enabled，DB 有非空值以 DB 为准，否则用配置默认值，
//     对应 Java app.user-tier.enabled，默认 true），且没付过钱。开关关掉时所有人都是付费档（宽松方向）。
//   - 免费图片要带水印（WatermarkedImage）：图片展示水印总开关开（afv_system_config.watermark.display_enabled，
//     同上 DB > 默认值，对应 Java app.watermark.enabled，默认 true），且 IsFree —— 即 Java
//     WatermarkPolicy.watermarked(userId, ASSET_LIBRARY) = DeliverableStoragePolicy.freeImage(userId)。
//   - 开关值按 Java Boolean.parseBoolean 解析：忽略大小写等于 "true" 才是开，其它非空值都是关。
//
// # 失败方向
//
// 查询失败一律按「付过钱」处理且不缓存（与 Java resolvePaid 返回 null 同向）：数据库抖一下不该给全站产物盖水印。
// 读开关失败沿用上次的值（首次失败用默认值）。
//
// # 缓存
//
// 只缓存「付过钱」（单调，一旦成立不会变回去），默认 60 秒；「没付过」不缓存，用户刚充值就立刻按付费对待。
// 开关值缓存 60 秒（运营在后台改了最多这么久生效）。
package usertier

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// PaidSources afv_points_batch.source 中算「付费」的取值（Java PointsBatchSourceEnum 里 paid=true 的两个）。
var PaidSources = []string{"RECHARGE", "UNION_RECHARGE"}

// afv_system_config 里的两个开关（Java SystemConfigService.KEY_USER_TIER_ENABLED / KEY_DISPLAY_WATERMARK_ENABLED）。
const (
	KeyUserTierEnabled         = "user_tier.enabled"
	KeyDisplayWatermarkEnabled = "watermark.display_enabled"
)

const (
	// DefaultPaidTTL 「付过钱」的进程内缓存时长。
	DefaultPaidTTL = 60 * time.Second
	// knobTTL 开关值的缓存时长。
	knobTTL = 60 * time.Second
	// maxPaidEntries 付费缓存上限，超过时整表清空（简单可靠，最多多查一轮库）。
	maxPaidEntries = 50000
)

// PaidExistsSQL 付费判定语句（单参数 user_id）。走 afv_points_batch 的 idx_user_source(user_id, source)。
const PaidExistsSQL = "SELECT COUNT(1) FROM (SELECT 1 FROM afv_points_batch WHERE user_id = ? AND source IN ('RECHARGE', 'UNION_RECHARGE') AND deleted = 0 LIMIT 1) t"

// PaidExistsCond 供批量 SQL 拼接的同一判据（EXISTS 子查询，外层用户列名由调用方给出）。
func PaidExistsCond(userCol string) string {
	return "EXISTS (SELECT 1 FROM afv_points_batch p WHERE p.user_id = " + userCol +
		" AND p.source IN ('RECHARGE', 'UNION_RECHARGE') AND p.deleted = 0)"
}

// Defaults 开关在 DB 没配置时的默认值（对应 Java yaml 的 USER_TIER_ENABLED / WATERMARK_ENABLED，两者默认都是 true）。
type Defaults struct {
	UserTierEnabled         bool
	DisplayWatermarkEnabled bool
}

// Resolver 分层判定。零值不可用，用 New。
type Resolver struct {
	paidQuery func(ctx context.Context, userID int64) (bool, error)
	knobQuery func(ctx context.Context, key string) (string, bool, error)
	defaults  Defaults
	paidTTL   time.Duration
	now       func() time.Time

	mu    sync.Mutex
	paid  map[int64]time.Time // 付过钱的用户 → 缓存写入时间
	knobs map[string]knob
}

type knob struct {
	val bool
	at  time.Time
}

// New 用共享库连接构造。
func New(conn sqlx.SqlConn, d Defaults) *Resolver {
	return NewWith(paidQueryOf(conn), knobQueryOf(conn), d)
}

// NewWith 注入查询函数（测试用）。paidQuery 返回是否付过；knobQuery 返回 (值, 是否有该行, 错误)。
func NewWith(paidQuery func(ctx context.Context, userID int64) (bool, error),
	knobQuery func(ctx context.Context, key string) (string, bool, error), d Defaults) *Resolver {
	return &Resolver{
		paidQuery: paidQuery, knobQuery: knobQuery, defaults: d, paidTTL: DefaultPaidTTL, now: time.Now,
		paid: map[int64]time.Time{}, knobs: map[string]knob{},
	}
}

// SetClock 测试用。
func (r *Resolver) SetClock(now func() time.Time) { r.now = now }

// HasEverPaid 是否曾经收到过付费来源的点数批次。不看分层开关。查询失败按付过处理（不缓存）。
func (r *Resolver) HasEverPaid(ctx context.Context, userID int64) bool {
	if r == nil || userID <= 0 {
		return false
	}
	now := r.now()
	r.mu.Lock()
	if at, ok := r.paid[userID]; ok && now.Sub(at) < r.paidTTL {
		r.mu.Unlock()
		return true
	}
	r.mu.Unlock()
	paid, err := r.paidQuery(ctx, userID)
	if err != nil {
		logx.WithContext(ctx).Errorw("usertier: tier lookup failed, treating as paid (check=user_tier.lookup)",
			logx.Field("user_id", userID), logx.Field("err", err.Error()))
		return true
	}
	if paid {
		r.mu.Lock()
		if len(r.paid) >= maxPaidEntries {
			r.paid = map[int64]time.Time{}
		}
		r.paid[userID] = now
		r.mu.Unlock()
	}
	return paid
}

// TierEnabled 分层开关（DB > 默认值）。
func (r *Resolver) TierEnabled(ctx context.Context) bool {
	return r.knob(ctx, KeyUserTierEnabled, r.defaults.UserTierEnabled)
}

// DisplayWatermarkEnabled 图片展示水印总开关（DB > 默认值）。
func (r *Resolver) DisplayWatermarkEnabled(ctx context.Context) bool {
	return r.knob(ctx, KeyDisplayWatermarkEnabled, r.defaults.DisplayWatermarkEnabled)
}

// IsFree 免费档：分层开启且没付过钱（Java UserTierService.isFree）。nil / 非法用户按付费。
func (r *Resolver) IsFree(ctx context.Context, userID int64) bool {
	if r == nil || userID <= 0 || !r.TierEnabled(ctx) {
		return false
	}
	return !r.HasEverPaid(ctx, userID)
}

// WatermarkedImage 该用户的图片产物是否要按免费档带水印交付（Java DeliverableStoragePolicy.freeImage）。
func (r *Resolver) WatermarkedImage(ctx context.Context, userID int64) bool {
	if r == nil || !r.DisplayWatermarkEnabled(ctx) {
		return false
	}
	return r.IsFree(ctx, userID)
}

// Evict 让某用户的付费缓存立刻失效（测试 / 运维用）。
func (r *Resolver) Evict(userID int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.paid, userID)
	r.mu.Unlock()
}

func (r *Resolver) knob(ctx context.Context, key string, def bool) bool {
	now := r.now()
	r.mu.Lock()
	k, ok := r.knobs[key]
	r.mu.Unlock()
	if ok && now.Sub(k.at) < knobTTL {
		return k.val
	}
	val := def
	if ok {
		val = k.val // 查询失败沿用上次的值
	}
	if r.knobQuery != nil {
		raw, found, err := r.knobQuery(ctx, key)
		switch {
		case err != nil:
			logx.WithContext(ctx).Errorw("usertier: read system config failed, keep last value (check=user_tier.knob)",
				logx.Field("key", key), logx.Field("value", val), logx.Field("err", err.Error()))
		case !found || strings.TrimSpace(raw) == "":
			val = def
		default:
			val = ParseBool(raw)
		}
	}
	r.mu.Lock()
	r.knobs[key] = knob{val: val, at: now}
	r.mu.Unlock()
	return val
}

// ParseBool Java Boolean.parseBoolean：忽略大小写等于 "true" 才为 true（Java 不 trim，这里去掉首尾空白，DB 值不会带空白）。
func ParseBool(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "true") }

func paidQueryOf(conn sqlx.SqlConn) func(ctx context.Context, userID int64) (bool, error) {
	return func(ctx context.Context, userID int64) (bool, error) {
		if conn == nil {
			return false, errors.New("usertier: 没有数据库连接")
		}
		var n int64
		if err := conn.QueryRowCtx(ctx, &n, PaidExistsSQL, userID); err != nil {
			return false, err
		}
		return n > 0, nil
	}
}

func knobQueryOf(conn sqlx.SqlConn) func(ctx context.Context, key string) (string, bool, error) {
	return func(ctx context.Context, key string) (string, bool, error) {
		if conn == nil {
			return "", false, nil
		}
		var v string
		err := conn.QueryRowCtx(ctx, &v, "SELECT COALESCE(config_value, '') FROM afv_system_config WHERE config_key = ? AND deleted = 0 LIMIT 1", key)
		if errors.Is(err, sqlx.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return v, true, nil
	}
}
