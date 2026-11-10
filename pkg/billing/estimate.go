package billing

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"
)

// Estimate 提交前报价（只读）：本次提交会冻结多少点、余额够不够。
//
// 和 Freeze 走同一个 price()：业务服务用**提交时构造的同一个 Charge**来问，
// 报出来的就是冻结的数——不另写一套算价。不冻结、不写库。
//
// 报价 = 提交的实际行为（Hold）：
//   - on：未配价格 / 算价失败 / 没开通权益（CheckEntitlement 打开时只读查 afv_app_user_feature）都会拦提交 →
//     Available=false + Reason（「该功能暂未配置价格，请联系客服」「未开通该功能」…）；
//   - shadow：Hold 对任何计费错误只记日志、不拦、冻结 0 点 → 不查权益，算价失败也 Available=true（Points 未知记 0，Charged=false）；
//   - off：不算价，Available=true、0 点。
//
// 免费额度：Go 这边冻结不扣任何免费额度（Freeze → price → freeze 全链路没有），
// 所以报价也不减；哪天冻结支持了免费额度，要在这里同步减掉。
type Estimate struct {
	// Available 报不报得出价。false 时 Points 为 0，原因见 Reason（给用户看的话）
	Available bool
	Reason    string
	// Mode 计费档位 off / shadow / on
	Mode string
	// Charged 提交是否真的冻结点数（仅 on）。off / shadow 提交冻结 0 点
	Charged bool
	// Points 按这组参数算出的点数（与冻结的 PricePoints 同一个数）。Charged=false 时提交实际冻结 0
	Points int64
	// UnitPoints 单价：按张的每张点数、按秒的每秒点数（主计费步骤）
	UnitPoints int64
	PriceUnit  string // PER_IMAGE / PER_SECOND / PER_REQUEST / PER_VIDEO
	Quantity   int    // 计量：张数或秒数
	// Balance 当前可用余额；查不到时为 0 且 BalanceKnown=false
	Balance      int64
	BalanceKnown bool
	// Sufficient 余额够不够这次冻结（Charged=false 时恒为 true）
	Sufficient bool
}

// estimator 报价要用的三样（*Service 满足），抽出来便于单测。
type estimator interface {
	Mode() string
	Quote(ctx context.Context, c Charge) (*Quote, error)
	Balance(ctx context.Context, userID int64) (*Account, error)
	// Entitled 只读权益检查（与 Freeze 同口径；CheckEntitlement 关闭时恒 true）
	Entitled(ctx context.Context, userID int64, featureCode string) (bool, error)
}

// Entitled 用户有没有该能力码的创作权益（只读，无副作用）。CheckEntitlement 关闭时恒为 true，与 Freeze 同口径。
func (s *Service) Entitled(ctx context.Context, userID int64, featureCode string) (bool, error) {
	if !s.conf.CheckEntitlement {
		return true, nil
	}
	return hasEntitlement(ctx, s.conn, userID, featureCode)
}

// Estimate 见类型注释。s 为 nil（服务没配计费）按 off 处理。
func (s *Service) Estimate(ctx context.Context, c Charge) Estimate {
	if s == nil {
		return Estimate{Available: true, Mode: ModeOff, Sufficient: true, BalanceKnown: false}
	}
	return estimate(ctx, s, c)
}

func estimate(ctx context.Context, e estimator, c Charge) Estimate {
	out := Estimate{Mode: e.Mode(), Sufficient: true}
	out.Charged = out.Mode == ModeOn
	if out.Mode == ModeOff {
		// 不计费：提交 Hold 直接返回 nil，冻结 0 点
		out.Available = true
		return out
	}
	shadow := out.Mode == ModeShadow
	q, err := e.Quote(ctx, c)
	if err != nil {
		check := "BILLING_UNAVAILABLE"
		if errors.Is(err, ErrNoOffer) {
			check = "BILLING_NO_OFFER"
		}
		logx.WithContext(ctx).Errorf("billing: 报价失败 判据=%s entry=%s 用户 %d 档次 %s 数量 %d 秒数 %d: %v",
			check, c.EntryCode, c.UserID, c.Tier, c.Count, c.Seconds, err)
		if shadow {
			// 影子期 Hold 对任何计费错误（含未配价格）只记日志、不拦提交，冻结 0 点：报价同样不当成拦截原因
			out.Available = true
			return out
		}
		if errors.Is(err, ErrNoOffer) {
			out.Reason = MsgNoOffer
		} else {
			out.Reason = "暂时无法报价，请稍后重试"
		}
		return out
	}
	// 权益准入：只在真扣（on）时查——on 下 Freeze 没开通会被 Hold 拦成「暂未开通该功能」；
	// shadow 下 Hold 吞掉这个错误照常提交，所以报价也不能说没开通。只读，无副作用
	if !shadow {
		if ok, err := e.Entitled(ctx, c.UserID, q.FeatureCode); err != nil {
			logx.WithContext(ctx).Errorf("billing: 报价查用户 %d 权益 %s 失败: %v", c.UserID, q.FeatureCode, err)
			out.Reason = "暂时无法报价，请稍后重试"
			return out
		} else if !ok {
			out.Reason = "未开通该功能"
			return out
		}
	}
	out.Available = true
	out.Points, out.PriceUnit, out.Quantity = q.PricePoints, q.PriceUnit, q.Quantity
	for _, l := range q.Lines {
		if !l.CostOnly {
			out.UnitPoints = l.UnitPoints // 主计费步骤（第一条收费的）
			break
		}
	}
	if c.UserID > 0 {
		if a, err := e.Balance(ctx, c.UserID); err != nil {
			logx.WithContext(ctx).Errorf("billing: 报价查用户 %d 余额失败: %v", c.UserID, err)
		} else {
			out.Balance, out.BalanceKnown = a.Balance, true
		}
	}
	if out.Charged && out.BalanceKnown {
		// 和 Enough / freeze 同口径：可用余额（balance，已冻结的不算）≥ 本次点数
		out.Sufficient = out.Balance >= out.Points
	}
	return out
}
