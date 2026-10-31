-- studio 定点审计（只读；读者：运营 / 客服 / 对账）。

-- name: StudioBatchStepLog :one
-- 业务含义：取一个批次的步骤日志（切图 / 反推 / 生图每步耗时与成本 [{step,ms,images,crops,degraded,cost_cny,at}]）及批次汇总字段。
-- 读者：运营排查某批次慢在哪一步、成本花在哪一步。
SELECT
  b.id,
  b.user_id,
  b.kind,
  b.status,
  b.estimated_points,
  b.task_total,
  b.task_done,
  b.task_failed,
  b.step_log,
  b.error_reason,
  b.created_at,
  b.updated_at
FROM afv_studio_batch b
WHERE b.id = sqlc.arg(batch_id);

-- name: StudioUserSpend :many
-- 业务含义：某用户在 [date_from, date_to) 内按入口类型的 studio 消耗：任务数、成功数、上游成本（人民币）、
-- 已结算点数（预留单 CONFIRMED / PARTIALLY_REFUNDED 的 price_points 合计，按 pkg/billing 的 biz_id 规则关联）。
-- 读者：客服处理用户扣费争议、运营核查大额用户。
SELECT
  t.kind,
  COUNT(*)                                                           AS task_count,
  CAST(COALESCE(SUM(t.status = 2), 0) AS SIGNED)                     AS succeeded_count,
  CAST(COALESCE(SUM(t.cost_cny), 0) AS DECIMAL(18, 6))               AS cost_cny_total,
  CAST(COALESCE(SUM(CASE WHEN r.status IN ('CONFIRMED', 'PARTIALLY_REFUNDED') THEN r.price_points ELSE 0 END), 0) AS SIGNED) AS settled_points
FROM afv_studio_task t
LEFT JOIN afv_billing_reservation r
  ON r.biz_type = 'LOCALLIFE' AND r.deleted = 0
 AND r.biz_id = (CASE WHEN CHAR_LENGTH(CONCAT(t.user_id, '_', t.billing_request_id)) <= 64
                   THEN CONCAT(t.user_id, '_', t.billing_request_id)
                   ELSE LEFT(CONCAT(t.user_id, '_', SHA2(CONCAT(t.user_id, '_', t.billing_request_id), 256)), 64) END)
WHERE t.user_id = sqlc.arg(user_id)
  AND t.created_at >= sqlc.arg(date_from) AND t.created_at < sqlc.arg(date_to)
GROUP BY t.kind
ORDER BY t.kind;
