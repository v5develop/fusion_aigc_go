-- ============================================================
-- 视频做同款：换声音 / 改台词（CONTRACT §P）
--   1. afv_replicate_voice_quota  按用户、按自然日（Asia/Shanghai，day 由应用层算）的已用次数（每人每天 N 条，默认 10）
--   2. afv_replicate_voice_use    一个任务占用的那一次额度（job_id 唯一）：确认时占用，任务没成功 / 合成失败时退回，
--                                 status 1=占用中 2=已退回；退回用「1→2」的条件更新，重复触发只退一次
--
-- 编号：Go_V1.0.57（所有分支 deploy/sql 最高 Go_V1.0.56，已提交）。上线前在各目标库核对已执行版本。
-- 只新增 Go 自有表；可重复执行（IF NOT EXISTS）。排序规则统一 utf8mb4_unicode_ci。
-- 音色目录复用 afv_digital_human_voice（只读，不新建表）。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_voice_quota` (
  `user_id`    bigint      NOT NULL COMMENT '用户ID',
  `day`        char(10)    NOT NULL COMMENT '自然日 yyyy-MM-dd（Asia/Shanghai）',
  `used`       int         NOT NULL DEFAULT '0' COMMENT '当日已占用条数（确认时占，失败退回）',
  `updated_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`user_id`, `day`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：换声音每日额度';

CREATE TABLE IF NOT EXISTS `afv_replicate_voice_use` (
  `job_id`     bigint      NOT NULL COMMENT '任务 afv_replicate_job.id',
  `user_id`    bigint      NOT NULL COMMENT '用户ID',
  `day`        char(10)    NOT NULL COMMENT '占用额度的自然日（退回时扣回同一天）',
  `status`     tinyint     NOT NULL DEFAULT '1' COMMENT '1=占用中 2=已退回',
  `created_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`job_id`),
  KEY `idx_user_day` (`user_id`, `day`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：换声音额度占用流水（一任务一条）';
