package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DeliverBlockedReason 预留已失效、无法确认扣费时的用户文案：不交付成品，请用户重新提交。
const DeliverBlockedReason = "点数已失效，请重新提交"

// LeaseDuration 本地生活预留的租约时长。任务在途（排队或执行中）时，每次轮询执行面都会把过期时间顺延到「现在 + 租约」；
// 任务结束后不再续期，预留到期即释放。这样排队很久的任务不会因为固定时长而过期，而服务挂掉时租约自然过期，不会白交付。
const LeaseDuration = 15 * time.Minute

// Renew 续期一笔在途的本地生活预留。只对 HELD 生效；已结算、已释放的单子不动。
// 调用方：每次轮询到「仍在途」的任务时调用一次。
func (s *Service) Renew(ctx context.Context, userID int64, requestID string) error {
	if s == nil || s.conf.Mode != ModeOn || requestID == "" {
		return nil
	}
	bizID := Charge{UserID: userID, RequestID: requestID}.bizID()
	_, err := s.conn.ExecCtx(ctx,
		"UPDATE `afv_billing_reservation` SET `expire_time` = ? "+
			"WHERE `biz_type` = ? AND `biz_id` = ? AND `status` = ? AND `deleted` = 0",
		time.Now().Add(LeaseDuration), bizTypeLocalLife, bizID, statusHeld)
	return err
}

// Settle 本地生活交付前的确认扣费。返回 nil 才允许交付成品。
//   - 计费关闭（off）或 shadow：不扣费，返回 nil；
//   - HELD：确认扣费（实际产出量 actual，多图部分成功按成功张数）；
//   - 已确认 / 部分结算：幂等，返回 nil；
//   - 已过期释放、已退款等：返回 ErrGone，调用方不得交付，应标记失败。
func (s *Service) Settle(ctx context.Context, userID int64, requestID string, actual int) error {
	if s == nil || s.conf.Mode != ModeOn {
		return nil
	}
	if requestID == "" {
		return fmt.Errorf("%w: 缺少请求号", ErrGone)
	}
	bizID := Charge{UserID: userID, RequestID: requestID}.bizID()
	var status string
	err := s.conn.QueryRowCtx(ctx, &status,
		"SELECT `status` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ? AND `deleted` = 0 LIMIT 1",
		bizTypeLocalLife, bizID)
	if errors.Is(err, sqlx.ErrNotFound) {
		return fmt.Errorf("%w: 找不到预留单 %s", ErrGone, bizID)
	}
	if err != nil {
		return err
	}
	switch status {
	case statusHeld:
		r, err := loadHeld(ctx, s.conn, bizID)
		if err != nil {
			return err
		}
		if r == nil {
			// 查询与加锁之间被别人改了状态：按已变化处理，不交付
			return fmt.Errorf("%w: %s 状态已变化", ErrGone, bizID)
		}
		return s.Confirm(ctx, r, actual)
	case statusConfirmed, statusPartiallyRefunded:
		return nil
	default:
		return fmt.Errorf("%w: %s 当前是 %s", ErrGone, bizID, status)
	}
}
