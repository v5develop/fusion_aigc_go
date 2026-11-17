-- ============================================================
-- 视频做同款：批量做多条（CONTRACT §M）
--
-- 「这个方案满意了，换 N 个商品 / 模特各做一条」：一次确认 = 一批（N ≤ 10 条任务），整批原子预留点数。
--   afv_replicate_batch      批次头：一次批量确认一行。(user_id, request_id) 唯一 = 幂等（同 call_id 重放同一批）。
--   afv_replicate_batch_job  批次 → 任务的关联：seq 是变体序号（从 0 起）；job_id 唯一（一条任务至多属于一批）。
-- 不改任何已有表：任务属于哪一批只经本关联表查（JobSummary.batchId）。
--
-- 批次头 status：0 提交中（逐条预留进行中）/ 1 已提交（全部预留成功、任务已排队）/ 2 已失败（预留失败，已整批回滚退点）。
-- 展示用的整体状态（running / succeeded / partial / failed）不落库，由各任务状态现算。
--
-- 编号：Go_V1.0.54（所有分支 deploy/sql 最高 Go_V1.0.52，本地未提交的漏斗表占 Go_V1.0.53）。上线前在各目标库核对已执行版本。
-- 只新增 Go 自有表；可重复执行（IF NOT EXISTS）。排序规则统一 utf8mb4_unicode_ci。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_batch` (
  `id`            bigint       NOT NULL AUTO_INCREMENT COMMENT '批次ID（对外 batchId）',
  `user_id`       bigint       NOT NULL COMMENT '用户ID',
  `plan_id`       bigint       NOT NULL COMMENT '源方案 afv_replicate_plan.id（已有成功成片的那张）',
  `plan_no`       varchar(32)  NOT NULL COMMENT '源方案号',
  `request_id`    varchar(64)  NOT NULL COMMENT '幂等键（工具 call_id；同用户唯一）',
  `variant_count` int          NOT NULL COMMENT '变体条数 N（1~10）',
  `total_points`  bigint       NOT NULL DEFAULT '0' COMMENT '整批合计点数（确认时的重算值，= 同意卡金额）',
  `status`        tinyint      NOT NULL DEFAULT '0' COMMENT '0 提交中 / 1 已提交 / 2 已失败（已回滚）',
  `fail_reason`   varchar(255) DEFAULT NULL COMMENT '失败原因（用户口径）',
  `created_at`    datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`    datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_request` (`user_id`, `request_id`),
  KEY `idx_plan` (`plan_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：批量批次头';

CREATE TABLE IF NOT EXISTS `afv_replicate_batch_job` (
  `id`            bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `batch_id`      bigint       NOT NULL COMMENT '批次 afv_replicate_batch.id',
  `seq`           int          NOT NULL COMMENT '变体序号（从 0 起）',
  `child_plan_id` bigint       NOT NULL DEFAULT '0' COMMENT '该变体派生的子方案 afv_replicate_plan.id',
  `job_id`        bigint       NOT NULL COMMENT '任务 afv_replicate_job.id',
  `created_at`    datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_batch_seq` (`batch_id`, `seq`),
  UNIQUE KEY `uk_job` (`job_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：批次与任务的关联';
