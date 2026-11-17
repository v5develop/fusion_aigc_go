-- ============================================================
-- 视频做同款：业务漏斗事件（CONTRACT §L1）
--
-- 每个漏斗阶段落一行（只增不改），供 Grafana 按天出漏斗、转化率与两个决策指标：
--   · 多人原片占比      = analysis_succeed 里 multi_person=1 的比例（也可按 analysis_id 切分后续转化）
--   · 定妆图 → 确认转化 = 同一个 plan_id 先有 preview 事件、后有 confirm_first / confirm_all 事件的比例
-- Prometheus 侧同名计数 replicate_funnel_total{stage,channel} 看实时；此表看按天的历史与转化关系。
--
-- channel：agent（智能体内部入口）/ web（REST）/ system（轮询器等系统推进；取同一 job / 分析上最早用户事件的渠道，查不到才是 system）。
-- 归属：analysis_id / plan_id / job_id 有就填，没有为 0；job 阶段的事件通过 plan_id 可关联到方案与分析。
-- dedupe_key 唯一：重放 / 多实例并发推进只落一行（INSERT IGNORE）。写入尽力而为，失败只告警（判据=funnel_event_write_failed）。
--
-- 编号：Go_V1.0.53（所有分支 deploy/sql 最高 Go_V1.0.52）。上线前在各目标库核对已执行版本。
-- 只新增 Go 自有表，不改任何 Java 表；可重复执行（IF NOT EXISTS）。排序规则统一 utf8mb4_unicode_ci。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_funnel` (
  `id`           bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `stage`        varchar(32)  NOT NULL COMMENT '漏斗阶段：analysis_create/analysis_succeed/plan_create/plan_edit/preview/confirm_first/confirm_all/batch_confirm/first_done/continue/decision_timeout/delivered/segment_save/redo/refine/bad_output/stop/cancel_rest/publish_copy/export_ratio',
  `channel`      varchar(16)  NOT NULL DEFAULT 'system' COMMENT '渠道：agent / web / system',
  `user_id`      bigint       NOT NULL DEFAULT '0' COMMENT '用户ID',
  `analysis_id`  bigint       NOT NULL DEFAULT '0' COMMENT '分析 afv_replicate_analysis.id（未知为 0）',
  `plan_id`      bigint       NOT NULL DEFAULT '0' COMMENT '方案 afv_replicate_plan.id（未知为 0）',
  `job_id`       bigint       NOT NULL DEFAULT '0' COMMENT '出片任务 afv_replicate_job.id（未知为 0）',
  `seg_index`    int          NOT NULL DEFAULT '0' COMMENT '段号（段级事件；其余为 0）',
  `multi_person` tinyint      NOT NULL DEFAULT '0' COMMENT '原片拆解主体数 >= 2（analysis_succeed / plan_create / plan_edit 上有值）',
  `dedupe_key`   varchar(96)  NOT NULL COMMENT '去重键（同一事件只落一行）',
  `created_at`   datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '发生时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_dedupe` (`dedupe_key`),
  KEY `idx_stage_time` (`stage`, `created_at`),
  KEY `idx_plan` (`plan_id`),
  KEY `idx_job` (`job_id`),
  KEY `idx_analysis` (`analysis_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：业务漏斗事件';
