-- 视频做同款分段预留对账（只读；读者：对账人员 / 值班运营）。
-- 每段一张预留单，镜像在 afv_replicate_charge；真值在 afv_billing_reservation。
-- 关联规则与 pkg/billing 的 bizID 一致：biz_type='LOCALLIFE'，biz_id = {user_id}_{request_id}，超过 64 字节折叠成
-- {user_id}_{SHA-256 十六进制} 截 64 位（与 studio_reconcile.sql 同一写法）。
-- 镜像状态：0-待预留 1-已预留 2-已扣除 3-已退回 4-不计费 5-预留失败 6-结算待重试。
-- 预留单状态 CONFIRMED / PARTIALLY_REFUNDED 算已扣除；REFUNDED / EXPIRED / CANCELLED 算已退回。

-- name: ReplicateChargeBillingMismatch :many
-- 业务含义：镜像与预留单真实状态不一致，或该结算的没结算：
--   confirmed_but_not_charged   镜像已扣除，预留单不是已扣除（或没有）
--   refunded_but_charged        镜像已退回，预留单却已扣除（多扣）
--   refunded_but_held           镜像已退回，预留单仍冻结（漏退）
--   settle_failed               镜像结算待重试（扣除 / 退回调用失败，引擎对账会重做；长时间不消失要人工核）
--   held_on_final_job           任务已终态（成功 / 失败 / 取消 / 部分成功）而预留单仍冻结，超过 older_than（漏结算）
-- 读者：对账人员每天跑；有结果就要人工核。
SELECT
  c.id,
  c.job_id,
  c.user_id,
  c.seg_index,
  c.stage,
  c.request_id,
  c.points,
  c.status          AS charge_status,
  j.status          AS job_status,
  r.reservation_no,
  r.status          AS reservation_status,
  r.price_points    AS reservation_points,
  CASE
    WHEN c.status = 2 THEN 'confirmed_but_not_charged'
    WHEN c.status = 3 AND r.status IN ('CONFIRMED', 'PARTIALLY_REFUNDED') THEN 'refunded_but_charged'
    WHEN c.status = 3 THEN 'refunded_but_held'
    WHEN c.status = 6 THEN 'settle_failed'
    ELSE 'held_on_final_job'
  END AS mismatch_type
FROM afv_replicate_charge c
JOIN afv_replicate_job j ON j.id = c.job_id
LEFT JOIN afv_billing_reservation r
  ON r.biz_type = 'LOCALLIFE' AND r.deleted = 0
 AND r.biz_id = (CASE WHEN CHAR_LENGTH(CONCAT(c.user_id, '_', c.request_id)) <= 64
                   THEN CONCAT(c.user_id, '_', c.request_id)
                   ELSE LEFT(CONCAT(c.user_id, '_', SHA2(CONCAT(c.user_id, '_', c.request_id), 256)), 64) END)
WHERE (c.status = 2 AND (r.id IS NULL OR r.status NOT IN ('CONFIRMED', 'PARTIALLY_REFUNDED')))
   OR (c.status = 3 AND r.status IN ('CONFIRMED', 'PARTIALLY_REFUNDED', 'HELD'))
   OR c.status = 6
   OR (j.status IN (2, 3, 4, 5) AND r.status = 'HELD' AND c.updated_at < sqlc.arg(older_than))
ORDER BY c.id
LIMIT ?;

-- name: ReplicateSegmentRetryAudit :many
-- 业务含义：每个 job 每段的出片尝试：自动重试（#r1）次数、用户重做（#u<n>）次数、最终状态；
-- 用于审计自动重试成本（平台承担）与重做是否被滥用。只看 created_after 之后建的 job。
-- 读者：运营 / 财务。
SELECT
  s.job_id,
  s.seg_index,
  CAST(SUM(CASE WHEN s.step_key REGEXP '#r[0-9]+$' THEN 1 ELSE 0 END) AS SIGNED)                           AS auto_retries,
  CAST(SUM(CASE WHEN s.step_key LIKE '%#u%' AND s.step_key NOT REGEXP '#r[0-9]+$' THEN 1 ELSE 0 END) AS SIGNED) AS user_redos,
  CAST(SUM(CASE WHEN s.status = 'SUCCEEDED' THEN 1 ELSE 0 END) AS SIGNED)                            AS succeeded_steps,
  CAST(COUNT(*) AS SIGNED)                                                                           AS attempts
FROM afv_replicate_step s
JOIN afv_replicate_job j ON j.id = s.job_id
WHERE s.kind = 'gateway' AND j.created_at >= sqlc.arg(created_after)
GROUP BY s.job_id, s.seg_index
HAVING attempts > 1
ORDER BY s.job_id, s.seg_index
LIMIT ?;
