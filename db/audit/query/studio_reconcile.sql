-- studio 对账（只读；读者：对账人员 / 值班运营）。
-- 预留单关联规则与 pkg/billing 的 bizID 一致：biz_type='LOCALLIFE'，
-- biz_id = {user_id}_{billing_request_id}，超过 64 字节折叠成 {user_id}_{SHA-256 十六进制} 截 64 位。
-- 参与核对按 billing_request_id 非空（提交时回填，冻结前就有），不再按 billing_status 镜像筛：
-- 镜像写失败（停在 0-未计费）而预留单真实存在的任务也要核；镜像为 0 且确实没有预留单的（Billing.Mode=off）不算不一致，
-- 4（影子模式不动账）没有真预留单，排除。任务状态 5-交付中 是在途态，不参与。
-- 预留单状态 CONFIRMED / PARTIALLY_REFUNDED 都算已结算；EXPIRED / CANCELLED / REFUNDED 都算已退回。

-- name: StudioTasksBillingMismatch :many
-- 业务含义：任务终态与预留单状态不一致。三种情况：
--   succeeded_not_confirmed  任务成功，但预留单没结算（仍 HELD，或已退回，或根本没有预留单）
--   failed_but_held          任务失败 / 取消，预留单还冻着，点数没退
--   failed_but_confirmed     任务失败 / 取消，预留单却已结算，多扣了
-- 读者：对账人员每天跑；有结果就要人工核。
SELECT
  t.id,
  t.task_id,
  t.user_id,
  t.kind,
  t.status          AS task_status,
  t.billing_status,
  t.billing_request_id,
  t.finished_at,
  r.reservation_no,
  r.status          AS reservation_status,
  r.price_points    AS reservation_points,
  CASE
    WHEN t.status = 2 THEN 'succeeded_not_confirmed'
    WHEN r.status = 'HELD' THEN 'failed_but_held'
    ELSE 'failed_but_confirmed'
  END AS mismatch_type
FROM afv_studio_task t
LEFT JOIN afv_billing_reservation r
  ON r.biz_type = 'LOCALLIFE' AND r.deleted = 0
 AND r.biz_id = (CASE WHEN CHAR_LENGTH(CONCAT(t.user_id, '_', t.billing_request_id)) <= 64
                   THEN CONCAT(t.user_id, '_', t.billing_request_id)
                   ELSE LEFT(CONCAT(t.user_id, '_', SHA2(CONCAT(t.user_id, '_', t.billing_request_id), 256)), 64) END)
WHERE t.billing_request_id IS NOT NULL AND t.billing_request_id <> ''
  AND t.billing_status <> 4
  AND (t.billing_status <> 0 OR r.id IS NOT NULL)
  AND (
        (t.status = 2 AND (r.id IS NULL OR r.status NOT IN ('CONFIRMED', 'PARTIALLY_REFUNDED')))
     OR (t.status IN (3, 4) AND r.status IN ('HELD', 'CONFIRMED'))
  )
ORDER BY t.id
LIMIT ?;

-- name: StudioStaleGenerating :many
-- 业务含义：任务停在「生成中」超过 older_than 仍没到终态（回调丢了 / 进程挂了 / gateway 卡住）。
-- 计时起点取 started_at，没有就取 updated_at。
-- 读者：值班运营，决定重试或强制失败。
SELECT
  t.id,
  t.task_id,
  t.user_id,
  t.kind,
  t.model_group,
  t.gateway_call_id,
  t.gateway_status,
  t.billing_status,
  t.billing_request_id,
  t.started_at,
  t.updated_at
FROM afv_studio_task t
WHERE t.status = 1
  AND COALESCE(t.started_at, t.updated_at) < sqlc.arg(older_than)
ORDER BY COALESCE(t.started_at, t.updated_at)
LIMIT ?;

-- name: StudioTaskCenterMissing :many
-- 业务含义：studio 任务在任务中心（afv_creation_task）里没有投影行。
-- 投影行的 task_id = taskcenter.Ref(前缀, 任务主键) = 'AiCreation_LF_' + MD5('<前缀>:<主键>')，前缀按 kind：
-- tryon=tyo、pose=pse、imagegen=simg（studio-rpc 配置 TaskIDPrefix 的默认值；改配置要同步改这里）。
-- 只看已回填 task_id 的任务，且创建时间早于 created_before（给周期同步留缓冲，通常传 now-10min）。
-- 读者：对账人员 / 排查「用户在任务中心看不到任务」。
SELECT
  t.id,
  t.task_id,
  t.user_id,
  t.kind,
  t.status,
  t.created_at
FROM afv_studio_task t
LEFT JOIN afv_creation_task c
  ON c.task_id = CONCAT('AiCreation_LF_', MD5(CONCAT(
       CASE t.kind WHEN 'tryon' THEN 'tyo' WHEN 'pose' THEN 'pse' ELSE 'simg' END, ':', t.id)))
WHERE t.task_id IS NOT NULL
  AND t.created_at < sqlc.arg(created_before)
  AND c.id IS NULL
ORDER BY t.id
LIMIT ?;
