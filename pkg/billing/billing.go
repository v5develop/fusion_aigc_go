// Package billing 点数计费：冻结 → 确认 / 退回。
//
// 写的是现有 Java 服务那批表（afv_points_account / _batch / _log / _log_line、
// afv_billing_reservation / _line），**语义、编号、幂等键格式全部照它的来**——
// 两边同时在跑，格式一分叉就对不上账。哪些是照抄的、哪些是我们改进的，
// 每处都在注释里写了出处。
//
// 口径出处：库里 1219 条预留单、2555 条流水（2026-09-27 核）。
//
//	FREEZE   余额 → 冻结     balance -= P, frozen += P, amount = -P
//	CONSUME  钱真正离开       frozen -= P，**balance 不动**，amount = -实际
//	REFUND   原路退回         frozen -= P, balance += P, amount = +P
//
// 全库配平：FREEZE 8988 = CONSUME 7894 + REFUND 1094。
package billing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Mode 上线档位。计费动的是钱，不敢一步到位，所以留三档。
const (
	ModeOff    = "off"    // 完全不计费（默认）：Freeze 直接返回 nil，调用方照常往下走
	ModeShadow = "shadow" // 影子：照常算价、打日志，但**不动账**。用来和 Java 的报价对数
	ModeOn     = "on"     // 真扣
)

// Conf 计费配置。
type Conf struct {
	// Mode off / shadow / on，默认 off
	Mode string `json:",default=off,options=off|shadow|on"`
	// Hold 预留（冻结）多久没确认就可以回收。
	//
	// ⚠️ 这是这套东西最容易出事的地方：Java 那边现有的单子是 30 分钟
	// （实测 create_time 14:24:25 → expire_time 14:54:25），而我们的视频任务
	// 超时是 40 分钟（口播实测本地 GPU 出 15 秒视频要 14 分钟）。
	// 照 30 分钟写，任务还在跑钱就被回收了，等成功去确认单子已经不是 HELD。
	// 所以按任务类型分开配，视频给足。
	Hold struct {
		DefaultMinutes int `json:",default=15"` // 图片、文案这类快的
		VideoMinutes   int `json:",default=90"` // 视频：要大于我们自己的任务超时
	}
	// CheckEntitlement 用不用创作权益做准入（afv_app_user_feature）。
	//
	// 产品 2026-09-27 定了开。前提是入口码挂的能力码要真有人被发放过——
	// 图片视频都共享电商的码（ecom_image_gen / ecom_video_gen / video_to_video，各 18 人），
	// AI文案不计费、不做准入（产品 2026-10-11 定）。**千万别挂 locallife_image_gen**（只有 2 人，还是手工发的），
	// 也别新建能力码——新码零发放，开了就是把那个功能对所有人关掉。
	// 上线前用 deploy/sql/check-billing-entitlement.sql 核一遍。
	//
	// ⚠️ 这是**准入闸，不是钱**，所以 Mode=off 也生效（见 CheckAccess）。
	CheckEntitlement bool `json:",default=true"`
}

// 档次。视频模块都是这两档，档位表里对应 <offer_code>_cloud / <offer_code>_lite。
const (
	TierCloud = "cloud"
	TierLite  = "lite"
)

// Kind 计费对象的大类，只用来决定预留多久。
type Kind int

const (
	KindQuick Kind = iota // 图片 / 文案
	KindVideo             // 视频
)

var (
	// ErrNotEnough 余额不够。调用方应转成 1008 并带上还差多少。
	ErrNotEnough = errors.New("billing: 点数余额不足")
	// ErrNoOffer 这个入口码还没在 afv_billing_offer 里配。
	ErrNoOffer = errors.New("billing: 入口码没有配置计费方案")
	// ErrGone 预留单不在了（被回收或已结算），确认时遇到要当异常报警。
	ErrGone = errors.New("billing: 预留单已不是 HELD")
	// ErrPricingRule 有已发布的计价规则会作用到我们身上，而我们没实现规则引擎。见 rules.go。
	ErrPricingRule = errors.New("billing: 命中未实现的计价规则")
	// ErrNoFeature 用户没有这个创作能力（仅在 CheckEntitlement 打开时出现）。
	ErrNoFeature = errors.New("billing: 用户未开通该创作能力")
)

// Charge 一次计费请求。
type Charge struct {
	UserID int64
	// RequestID 客户端幂等键（X-Request-Id）。和 UserID 一起构成 biz_id。
	RequestID string
	// EntryCode 入口码，对应 afv_billing_offer.entry_code（驼峰，如 digitalHuman）
	EntryCode string
	// Tier 档次，按 `<offer_code>_<tier>` 档位码或 quality_tier 找档位。
	//
	// 2026-10-10 在测试库核实：六个视频入口都有 `<offer>_cloud` / `<offer>_lite` 档位，模型码带 -b6/-b8/-b12。
	// Java 对照仓库说查不到，是因为这些是后台手工配的、仓库迁移里没有。
	// 例外：actionTransfer 的 lite 档叫 `action_transfer_standard`，要改名成 `_lite` 才找得到。
	// 图文笔记 / 海报 / 长图 在测试库里还没有 offer（产品 2026-10-11 定模型 GPT Image 2 = creaft-image-2，等后台配）；AI文案不计费。
	Tier string
	// ModelCode 模型码，对应 afv_billing_offer_variant 默认模型的 afv_ai_model.code。
	// 一般不用填，填了优先于 Tier。
	ModelCode string
	Kind      Kind
	// Count 产出数量（按张计的用），默认 1
	Count int
	// Seconds 时长（按秒计的用）
	Seconds int
	// Resolution 分辨率，影响单价（afv_model_pricing.resolution）。空则用 variant 默认。
	Resolution string
}

// bizID 和 Java 一致：{userId}_{requestId}。
// afv_billing_reservation 上有 UNIQUE uk_biz(biz_type, biz_id)，重复提交靠它挡。
// bizID afv_billing_reservation.biz_id 是 varchar(64)（Java 回复 2.5），请求号超长就折叠成哈希，
// 不然插入时 1406。折叠要稳定（同一个请求号永远同一个值），幂等才成立。
func (c Charge) bizID() string {
	id := fmt.Sprintf("%d_%s", c.UserID, c.RequestID)
	if len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%d_%x", c.UserID, sum[:])[:64]
}

// Reservation 冻结成功后的预留单。Mode=off 时 Freeze 返回 nil，后续 Confirm/Refund 收到 nil 直接跳过。
type Reservation struct {
	No          string // BR + 32 hex
	PricePoints int64
	Quote       *Quote
	shadow      bool // shadow 模式产生的，没真写库
}

// Service 计费服务。
type Service struct {
	conf Conf
	conn sqlx.SqlConn
}

func New(c Conf, conn sqlx.SqlConn) *Service {
	if c.Mode == "" {
		c.Mode = ModeOff
	}
	if c.Hold.DefaultMinutes <= 0 {
		c.Hold.DefaultMinutes = 15
	}
	if c.Hold.VideoMinutes <= 0 {
		c.Hold.VideoMinutes = 90
	}
	return &Service{conf: c, conn: conn}
}

func (s *Service) Mode() string { return s.conf.Mode }
func (s *Service) On() bool     { return s.conf.Mode == ModeOn }

// holdFor 这类任务的预留时长。
func (s *Service) holdFor(k Kind) time.Duration {
	if k == KindVideo {
		return time.Duration(s.conf.Hold.VideoMinutes) * time.Minute
	}
	return time.Duration(s.conf.Hold.DefaultMinutes) * time.Minute
}

// Quote 一次报价。Price 只读配置表，不动账，可以单独拿来给前端显示。
func (s *Service) Quote(ctx context.Context, c Charge) (*Quote, error) {
	return price(ctx, s.conn, c)
}

// Balance 余额。用户没有账户时返回零值而不是报错——
// 新用户开箱就是没账户的（24 个人里有 1 个没有），这不是异常。
func (s *Service) Balance(ctx context.Context, userID int64) (*Account, error) {
	return loadAccount(ctx, s.conn, userID)
}

// CheckAccess 准入校验：这个用户有没有这个功能的创作权益。
//
// **和计费开关无关**——Mode=off（不扣点）时它照样生效。权益是"能不能用"，
// 计费是"用了扣多少"，两件事，不该捆在一起：不扣点的阶段也不该让没买的人白用。
//
// Java 把这道闸放在报价里（BillingPricingServiceImpl.validateFeatureAvailable），
// 我们直接写计费表、不走它的报价接口，所以得自己来。
//
// 没开校验、或这个入口码还没配计费方案时一律放行——
// 权益校验不该因为计费没配好就把功能拦死。
func (s *Service) CheckAccess(ctx context.Context, userID int64, entryCode string) error {
	if !s.conf.CheckEntitlement {
		return nil
	}
	code, err := featureCodeOf(ctx, s.conn, entryCode)
	if err != nil {
		if errors.Is(err, ErrNoOffer) {
			logx.WithContext(ctx).Errorf("billing: 入口码 %s 没配计费方案，权益校验跳过", entryCode)
			return nil
		}
		return err
	}
	ok, err := hasEntitlement(ctx, s.conn, userID, code)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoFeature, code)
	}
	return nil
}

// Freeze 提交生成时冻结点数。
//
// 返回 nil 表示这次不计费（Mode=off），调用方不用判空之外的事，
// 后面把它原样交给 Confirm / Refund 即可。
//
// 余额不足返回 ErrNotEnough，错误里带了还差多少（见 NotEnough）。
func (s *Service) Freeze(ctx context.Context, c Charge) (*Reservation, error) {
	if s.conf.Mode == ModeOff {
		return nil, nil
	}
	if c.RequestID == "" {
		return nil, errors.New("billing: 缺少 RequestID，无法保证幂等")
	}
	q, err := price(ctx, s.conn, c)
	if err != nil {
		return nil, err
	}
	if s.conf.CheckEntitlement {
		ok, err := hasEntitlement(ctx, s.conn, c.UserID, q.FeatureCode)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNoFeature, q.FeatureCode)
		}
	}
	if s.conf.Mode == ModeShadow {
		// 影子模式：只记一笔，不动账。用来和 Java 的 /billing/quote/preview 对数。
		logx.WithContext(ctx).Infow("billing shadow freeze",
			logx.Field("user_id", c.UserID), logx.Field("entry_code", c.EntryCode),
			logx.Field("model", c.ModelCode), logx.Field("points", q.PricePoints),
			logx.Field("biz_id", c.bizID()))
		return &Reservation{PricePoints: q.PricePoints, Quote: q, shadow: true}, nil
	}
	return freeze(ctx, s.conn, c, q, s.holdFor(c.Kind))
}

// Confirm 生成成功后确认扣除。
//
// actual 是**实际产出量**（张数或秒数）：多图部分成功时传成功的张数，
// 差额会在同一个事务里退回，不留尾巴。传 0 表示按原量全额确认。
func (s *Service) Confirm(ctx context.Context, r *Reservation, actual int) error {
	if r == nil || r.shadow {
		return nil
	}
	return settle(ctx, s.conn, r, actual)
}

// Refund 生成失败 / 超时，把冻结的原路退回。
func (s *Service) Refund(ctx context.Context, r *Reservation, reason string) error {
	if r == nil || r.shadow {
		return nil
	}
	return refund(ctx, s.conn, r.No, reason)
}

// Load 按 biz_id 找回预留单。
//
// 为什么需要它：冻结发生在提交接口里，确认/退回发生在轮询器里（另一个进程、
// 甚至重启之后），内存里的 *Reservation 传不过去。所以状态只认库——
// 轮询器拿 userID + requestID 把单子捞回来再结算。
func (s *Service) Load(ctx context.Context, userID int64, requestID string) (*Reservation, error) {
	if s.conf.Mode != ModeOn || requestID == "" {
		return nil, nil
	}
	return loadHeld(ctx, s.conn, Charge{UserID: userID, RequestID: requestID}.bizID())
}

// NotEnough 余额不足的细节，给接口层拼提示用。
type NotEnough struct {
	Need      int64 // 本次需要
	Available int64 // 当前可用
}

func (e *NotEnough) Error() string {
	return fmt.Sprintf("billing: 点数余额不足，需要 %d，可用 %d", e.Need, e.Available)
}

func (e *NotEnough) Is(target error) bool { return target == ErrNotEnough }

// Missing 还差多少。
func (e *NotEnough) Missing() int64 {
	if e.Need > e.Available {
		return e.Need - e.Available
	}
	return 0
}
