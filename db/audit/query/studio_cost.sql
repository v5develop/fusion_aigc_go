-- studio 成本汇总（只读；读者：运营 / 财务对账）。
-- cost_cny 是任务落库的上游成本（人民币），用来对 gateway /v1/ledger/costs。
-- 不过滤 deleted：用户删除只是隐藏，钱已经花了。时间口径统一左闭右开 [date_from, date_to)，按任务 created_at。

-- name: StudioCostSummaryByKind :many
-- 业务含义：按入口类型（tryon / pose / imagegen）汇总任务数、成功数、失败数（含取消）、上游成本合计。
-- 读者：运营看板、月度成本核对。
SELECT
  t.kind,
  COUNT(*)                                                  AS task_count,
  CAST(COALESCE(SUM(t.status = 2), 0) AS SIGNED)            AS succeeded_count,
  CAST(COALESCE(SUM(t.status IN (3, 4)), 0) AS SIGNED)      AS failed_count,
  CAST(COALESCE(SUM(t.cost_cny), 0) AS DECIMAL(18, 6))      AS cost_cny_total
FROM afv_studio_task t
WHERE t.created_at >= sqlc.arg(date_from) AND t.created_at < sqlc.arg(date_to)
GROUP BY t.kind
ORDER BY t.kind;

-- name: StudioCostSummaryByModelGroup :many
-- 业务含义：按 ai-gateway model group（如 img.gpt-image-2）汇总任务数、成功数、成本，看哪个模型花钱多、失败多。
-- 读者：运营选型、成本对账（对 gateway 按模型的账单）。
SELECT
  t.model_group,
  COUNT(*)                                                  AS task_count,
  CAST(COALESCE(SUM(t.status = 2), 0) AS SIGNED)            AS succeeded_count,
  CAST(COALESCE(SUM(t.status IN (3, 4)), 0) AS SIGNED)      AS failed_count,
  CAST(COALESCE(SUM(t.cost_cny), 0) AS DECIMAL(18, 6))      AS cost_cny_total
FROM afv_studio_task t
WHERE t.created_at >= sqlc.arg(date_from) AND t.created_at < sqlc.arg(date_to)
GROUP BY t.model_group
ORDER BY cost_cny_total DESC, t.model_group;

-- name: StudioBatchCost :many
-- 业务含义：一个批次内每个任务的成本、状态和对应预留单（状态 / 点数）；预留单按 pkg/billing 的 biz_id 规则关联，没有预留单时为空。
-- 读者：客服 / 运营查单批次为何成本异常、对账定位到具体任务。
SELECT
  t.id,
  t.task_id,
  t.kind,
  t.model_index,
  t.shot_index,
  t.pose_index,
  t.attempt,
  t.model_group,
  t.status,
  t.billing_status,
  t.cost_cny,
  t.billing_request_id,
  r.reservation_no,
  r.status       AS reservation_status,
  r.price_points AS reservation_points,
  t.started_at,
  t.finished_at
FROM afv_studio_task t
LEFT JOIN afv_billing_reservation r
  ON r.biz_type = 'LOCALLIFE' AND r.deleted = 0
 AND r.biz_id = (CASE WHEN CHAR_LENGTH(CONCAT(t.user_id, '_', t.billing_request_id)) <= 64
                   THEN CONCAT(t.user_id, '_', t.billing_request_id)
                   ELSE LEFT(CONCAT(t.user_id, '_', SHA2(CONCAT(t.user_id, '_', t.billing_request_id), 256)), 64) END)
WHERE t.batch_id = sqlc.arg(batch_id)
ORDER BY t.model_index, t.shot_index, t.pose_index, t.attempt, t.id;

-- name: StudioDailyCost :many
-- 业务含义：按自然日 + 入口类型汇总任务数与成本，画成本趋势。
-- 读者：运营日报、成本异常告警的对照数据。
SELECT
  DATE(t.created_at)                                        AS day,
  t.kind,
  COUNT(*)                                                  AS task_count,
  CAST(COALESCE(SUM(t.status = 2), 0) AS SIGNED)            AS succeeded_count,
  CAST(COALESCE(SUM(t.cost_cny), 0) AS DECIMAL(18, 6))      AS cost_cny_total
FROM afv_studio_task t
WHERE t.created_at >= sqlc.arg(date_from) AND t.created_at < sqlc.arg(date_to)
GROUP BY DATE(t.created_at), t.kind
ORDER BY day, t.kind;
