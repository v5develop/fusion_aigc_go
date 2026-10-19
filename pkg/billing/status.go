package billing

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Status 按用户和请求号取预留单的当前状态（HELD / CONFIRMED / REFUNDED …）。
//
// 没有预留单（含 billing 未开启）返回 ""，调用方据此知道这次生成没有涉及点数。
// 对账以这张库表为准：它是计费的唯一记录，不依赖任何由业务表派生出来的副本。
func (s *Service) Status(ctx context.Context, userID int64, requestID string) (string, error) {
	if s.conf.Mode != ModeOn || requestID == "" {
		return "", nil
	}
	var status string
	err := s.conn.QueryRowCtx(ctx, &status,
		"SELECT `status` FROM `afv_billing_reservation` WHERE `biz_type` = ? AND `biz_id` = ? AND `deleted` = 0 LIMIT 1",
		bizTypeLocalLife, Charge{UserID: userID, RequestID: requestID}.bizID())
	if errors.Is(err, sqlx.ErrNotFound) {
		return "", nil
	}
	return status, err
}
