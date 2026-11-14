package billing

// 流水（afv_points_log.idempotency_key）的键前缀，导出给只读对账方（如视频做同款后台判断一笔退回是不是本次操作写的）。
// 只是把现有字面量公开，行为不变：ledger.go 的 refund / 结算差额退回写 RefundKeyPrefix + 单号，
// refund_confirmed.go 的人工退回写 PostRefundKeyPrefix + 单号（契约测试 ledger_keys_test.go 锁死两处一致）。
const (
	// RefundKeyPrefix 冻结单退回（billing.Refund）与按实际交付量结算的差额退回。
	RefundKeyPrefix = "refunded:"
	// PostRefundKeyPrefix 已确认单的人工退回（billing.RefundConfirmed）。
	PostRefundKeyPrefix = "post-refund:"
)
