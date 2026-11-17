-- ============================================================
-- 视频做同款：成片反馈 + 画面连贯度（CONTRACT §Y / §Z）
--   1. afv_replicate_feedback  用户对成片 / 某一段的满意 / 不满意 + 原因 + 备注；
--      同一用户同一任务同一段只留一行（uk_user_job_seg，覆盖更新）。
--      seg_index：0 = 整条成片，≥1 = 某一段（不用 NULL，唯一键里 NULL 不去重）。
--      continuity：该任务确认时的画面连贯度（fast / smooth），冗余存一份，便于按它对比满意度。
--   2. afv_replicate_funnel.continuity  确认类事件记录连贯度（其余事件默认 fast）；
--      同时放宽 stage 注释，新增阶段 feedback_up / feedback_down。
--
-- 编号：Go_V1.0.60（所有分支 deploy/sql 最高 Go_V1.0.59）。上线前在各目标库核对已执行版本。
-- 只新增 / 改 Go 自有表，不改任何 Java 表；可重复执行。排序规则统一 utf8mb4_unicode_ci。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_feedback` (
  `id`         bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`    bigint       NOT NULL COMMENT '用户ID',
  `job_id`     bigint       NOT NULL COMMENT '出片任务 afv_replicate_job.id',
  `seg_index`  int          NOT NULL DEFAULT '0' COMMENT '段号：0 = 整条成片，>=1 = 某一段',
  `rating`     varchar(8)   NOT NULL COMMENT '满意度：up / down',
  `reasons`    json         DEFAULT NULL COMMENT '不满意原因数组：face_mismatch/product_deformed/motion_weird/seam_jump/text_garbled/other（仅 down）',
  `note`       varchar(600) NOT NULL DEFAULT '' COMMENT '备注（<=200 字，已过文本审核）',
  `continuity` varchar(8)   NOT NULL DEFAULT 'fast' COMMENT '任务的画面连贯度：fast / smooth',
  `created_at` datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '首次反馈时间',
  `updated_at` datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '最近一次更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_job_seg` (`user_id`, `job_id`, `seg_index`),
  KEY `idx_job` (`job_id`),
  KEY `idx_time` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：成片反馈';

DROP PROCEDURE IF EXISTS `go_v1060_add_column`;
DELIMITER $$
CREATE PROCEDURE `go_v1060_add_column`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_def TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD COLUMN `', p_col, '` ', p_def);
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
DELIMITER ;

CALL `go_v1060_add_column`('afv_replicate_funnel', 'continuity',
  'varchar(8) NOT NULL DEFAULT ''fast'' COMMENT ''画面连贯度 fast / smooth（确认类事件有值，其余默认 fast）''');

DROP PROCEDURE IF EXISTS `go_v1060_add_column`;
