-- ============================================================
-- 视频做同款：成片分享进示例库（CONTRACT §T）
--   afv_replicate_share  用户把自己的完整成片投稿成公开示例：待审核 → 通过 / 驳回 / 撤回；
--                        通过后由运营在管理端手动「发放奖励」（积分只在 Java，这里只记发放状态与时间，用于幂等与「每人每月最多 3 次」）。
--   * (job_id) 唯一：同一成片只能投一次（撤回后可重新投，同一行改回待审核；被驳回的不能再投）。
--   * portrait_consent：方案替换了人物时用户是否已勾选肖像授权。Java 在对话里收集授权，Go 看不到，
--     所以由智能体调工具时带 portrait_consent=true，这里只留一份记录；替换人物的方案没带就不建投稿。
--   * industry：投稿时用户在广场设置的主行业 code（可空），通过后作为新示例的行业。
--   * sample_id：通过后生成的示例 afv_replicate_sample.id；撤回时据此立即下架（逻辑删除 + 停用）。
--   * reward_status none / granted；reward_at 用来数「同一用户本月已发放几次」。
--
-- 编号：Go_V1.0.58（所有分支 deploy/sql 已提交最高 Go_V1.0.56，本工作区未提交的 Go_V1.0.57 换声音已占一号）。上线前在各目标库核对已执行版本。
-- 只新增 Go 自有表；可重复执行（IF NOT EXISTS）。排序规则统一 utf8mb4_unicode_ci。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_share` (
  `id`               bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID（对外 submissionId）',
  `user_id`          bigint       NOT NULL COMMENT '投稿用户ID',
  `job_id`           bigint       NOT NULL COMMENT '任务 afv_replicate_job.id（完整成片）',
  `plan_id`          bigint       NOT NULL COMMENT '方案 afv_replicate_plan.id',
  `status`           varchar(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending / approved / rejected / withdrawn',
  `reject_reason`    varchar(255) NOT NULL DEFAULT '' COMMENT '驳回原因（运营填写，展示给用户）',
  `sample_id`        bigint       DEFAULT NULL COMMENT '通过后生成的示例 afv_replicate_sample.id',
  `portrait_consent` tinyint      NOT NULL DEFAULT '0' COMMENT '1=替换了人物且用户已勾选肖像授权',
  `industry`         varchar(64)  NOT NULL DEFAULT '' COMMENT '投稿时用户的主行业 code（可空）',
  `reward_status`    varchar(16)  NOT NULL DEFAULT 'none' COMMENT 'none / granted（积分在 Java，管理端发放成功后回写）',
  `reward_at`        datetime(3)  DEFAULT NULL COMMENT '奖励发放时间（数每人每月次数）',
  `reviewed_by`      bigint       NOT NULL DEFAULT '0' COMMENT '审核人 admin id',
  `reviewed_at`      datetime(3)  DEFAULT NULL COMMENT '审核时间',
  `created_at`       datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '投稿时间',
  `updated_at`       datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_job` (`job_id`),
  KEY `idx_status_id` (`status`, `id`),
  KEY `idx_user_reward` (`user_id`, `reward_status`, `reward_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：成片分享进示例库的用户投稿';
