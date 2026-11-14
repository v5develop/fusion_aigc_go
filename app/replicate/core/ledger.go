package core

import (
	"context"
	"errors"
	"fmt"

	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/logx"
)

// billingAPI *billing.Service 中本功能用到的方法（便于测试替换）。
type billingAPI interface {
	Quote(ctx context.Context, c billing.Charge) (*billing.Quote, error)
	Hold(ctx context.Context, c billing.Charge) (*billing.Reservation, error)
	Status(ctx context.Context, userID int64, requestID string) (string, error)
	Load(ctx context.Context, userID int64, requestID string) (*billing.Reservation, error)
	Refund(ctx context.Context, r *billing.Reservation, reason string) error
	Settle(ctx context.Context, userID int64, requestID string, actual int) error
	Renew(ctx context.Context, userID int64, requestID string) error
}

// 预留单状态（afv_billing_reservation.status，与 Java 同口径）。
const (
	resHeld              = "HELD"
	resConfirmed         = "CONFIRMED"
	resPartiallyRefunded = "PARTIALLY_REFUNDED"
	resRefunded          = "REFUNDED"
	resExpired           = "EXPIRED"
	resCancelled         = "CANCELLED"
)

// BillingLedger 经 pkg/billing 动账（唯一写 afv_billing_* 的路径）。
//
// 与 billing.Done / billing.Fail 的区别：那两个吞错误只打日志；这里把错误交回引擎，
// 退回失败时镜像记「结算待重试」并由对账重做，用户侧只在退回真正成功后才显示「已退回」（EXPERIENCE §4.8 X-4）。
type BillingLedger struct{ b billingAPI }

// NewBillingLedger 包一层 *billing.Service。
func NewBillingLedger(s *billing.Service) *BillingLedger { return &BillingLedger{b: s} }

func (l *BillingLedger) Quote(ctx context.Context, c billing.Charge) (int64, error) {
	q, err := l.b.Quote(ctx, c)
	if err != nil {
		return 0, billing.UserError(ctx, c.UserID, c.EntryCode, err)
	}
	return q.PricePoints, nil
}

func (l *BillingLedger) Hold(ctx context.Context, c billing.Charge) (bool, error) {
	r, err := l.b.Hold(ctx, c)
	if err != nil || r == nil {
		return false, err
	}
	// 冻结按幂等键命中旧单时原样返回（不论状态、不论当初按什么价 / 入口冻结）。请求号带 job 内部盐（见 reqFirst），
	// 外部幂等键碰不上；仍逐项核对入口 / 模型组 / 点数，防止把别处先占的单当成本段的预留（verifyReplay）。
	if err := l.verifyReplay(ctx, c, r); err != nil {
		return false, err
	}
	// 再核一次状态，防止把已退回 / 已回收的旧单当成刚冻结成功。
	st, err := l.b.Status(ctx, c.UserID, c.RequestID)
	if err != nil {
		// 冻结已成功返回，只是读不出状态：钱可能已冻结，不能当成「预留失败、没扣点数」（调用方按请求号幂等退回）
		return false, fmt.Errorf("%w: 请求号 %s 冻结后读状态失败: %v", ErrHoldUnknown, c.RequestID, err)
	}
	switch st {
	case resHeld:
		return true, nil
	case "":
		return false, nil // shadow：没有真预留单
	}
	return false, fmt.Errorf("%w: 请求号 %s 的预留单是 %s", ErrChargeGone, c.RequestID, st)
}

// verifyReplay 冻结命中已有单时，核对它与本次冻结的入口、模型组、点数一致；不一致返回 ErrHoldMismatch（不得当成本段已预留）。
// 本次新冻结的单就是按这次的 Charge 算价写入的，无需核对。
func (l *BillingLedger) verifyReplay(ctx context.Context, c billing.Charge, r *billing.Reservation) error {
	if !r.Replayed() {
		return nil
	}
	q, err := l.b.Quote(ctx, c)
	if err != nil {
		return fmt.Errorf("%w: 请求号 %s 命中已有预留单，重算报价失败: %v", ErrHoldMismatch, c.RequestID, err)
	}
	var why string
	switch {
	case r.Quote == nil:
		why = "已有单缺价格快照"
	case r.Quote.EntryCode != c.EntryCode:
		why = fmt.Sprintf("入口 %q ≠ %q", r.Quote.EntryCode, c.EntryCode)
	case r.Quote.ModelGroup != q.ModelGroup:
		why = fmt.Sprintf("模型组 %q ≠ %q", r.Quote.ModelGroup, q.ModelGroup)
	case r.PricePoints != q.PricePoints:
		why = fmt.Sprintf("点数 %d ≠ %d", r.PricePoints, q.PricePoints)
	default:
		return nil
	}
	logx.WithContext(ctx).Errorf("replicate: 冻结命中的已有预留单与本次不一致，拒绝复用 判据=HOLD_REPLAY_MISMATCH user=%d 请求号=%s 预留单=%s 原因=%s",
		c.UserID, c.RequestID, r.No, why)
	return fmt.Errorf("%w: 请求号 %s 的已有预留单 %s：%s", ErrHoldMismatch, c.RequestID, r.No, why)
}

func (l *BillingLedger) Confirm(ctx context.Context, userID int64, requestID string) error {
	err := l.b.Settle(ctx, userID, requestID, 0)
	if errors.Is(err, billing.ErrGone) {
		return fmt.Errorf("%w: %v", ErrChargeGone, err)
	}
	return err
}

func (l *BillingLedger) Refund(ctx context.Context, userID int64, requestID, reason string) (bool, error) {
	st, err := l.b.Status(ctx, userID, requestID)
	if err != nil {
		return false, err
	}
	switch st {
	case "":
		return false, nil // 没有预留单（off / shadow / 0 点）
	case resRefunded, resExpired, resCancelled:
		return true, nil // 已退回 / 已到期释放：钱已回到可用余额（幂等）
	case resConfirmed, resPartiallyRefunded:
		return false, fmt.Errorf("%w: 请求号 %s 当前是 %s", ErrChargeConfirmed, requestID, st)
	}
	r, err := l.b.Load(ctx, userID, requestID)
	if err != nil {
		return false, err
	}
	if r == nil { // 查状态与加载之间被改了：下一轮重新判断
		return false, fmt.Errorf("replicate: 请求号 %s 的预留单状态刚变化，稍后重试", requestID)
	}
	if err := l.b.Refund(ctx, r, reason); err != nil {
		return false, err
	}
	return true, nil
}

func (l *BillingLedger) Renew(ctx context.Context, userID int64, requestID string) error {
	return l.b.Renew(ctx, userID, requestID)
}

func (l *BillingLedger) Status(ctx context.Context, userID int64, requestID string) (string, error) {
	return l.b.Status(ctx, userID, requestID)
}

// settledStatus 预留单已扣除（含部分结算）。
func settledStatus(st string) bool { return st == resConfirmed || st == resPartiallyRefunded }
