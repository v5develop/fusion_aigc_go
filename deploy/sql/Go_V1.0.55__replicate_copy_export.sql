-- ============================================================
-- 视频做同款：发布文案 + 多比例导出（CONTRACT §N，都免费、无同意卡）
--   1. afv_replicate_copy        一条成片在一个平台的发布文案；(job_id, platform) 唯一：重复调用回放，不占额度
--   2. afv_replicate_copy_quota  按用户、按自然日（Asia/Shanghai，day 由应用层算）的已用次数（每人每天 N 次，默认 20）；
--                                与定妆图额度同一套「有余量才 +1」的原子更新，文案生成失败再退回
--   3. afv_replicate_export      一条成片的一种比例导出；(job_id, ratio, mode, src_rid) 唯一 = 每成片每比例每模式只做一次
--                                （src_rid 是被导出的成片 rid：部分成片重做成功换成完整版后，新成片可另做一份）
--
-- 入素材库不另建表：走现有发件箱 afv_replicate_delivery（kind='ratio'，version 编码见 core/export_ratio.go：
--   version = 100 * 代数 + 10 * 比例序号(9:16=1,1:1=2,16:9=3,3:4=4,4:3=5) + 模式(pad=0,crop=1)）。
--
-- 编号：Go_V1.0.55（所有分支 deploy/sql 最高 Go_V1.0.52；本工作区未提交的 Go_V1.0.53 漏斗 / Go_V1.0.54 批量已占前两号）。上线前在各目标库核对已执行版本。
-- 只新增 Go 自有表；可重复执行（IF NOT EXISTS）。排序规则统一 utf8mb4_unicode_ci。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_copy` (
  `id`         bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`    bigint       NOT NULL COMMENT '所属用户ID',
  `job_id`     bigint       NOT NULL COMMENT '任务 afv_replicate_job.id',
  `platform`   varchar(16)  NOT NULL COMMENT 'douyin / kuaishou / xiaohongshu / shipinhao',
  `status`     tinyint      NOT NULL DEFAULT '0' COMMENT '0=生成中（占坑）1=已生成',
  `content`    json         DEFAULT NULL COMMENT '清洗后的文案 {titles,hashtags,description,coverAtSec}',
  `call_id`    varchar(64)  NOT NULL DEFAULT '' COMMENT '网关调用号（排查用）',
  `created_at` datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at` datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间（生成中超过回收时限的占坑可被接手）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_job_platform` (`job_id`, `platform`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：发布文案（一成片一平台一份）';

CREATE TABLE IF NOT EXISTS `afv_replicate_copy_quota` (
  `user_id`    bigint      NOT NULL COMMENT '用户ID',
  `day`        char(10)    NOT NULL COMMENT '自然日 yyyy-MM-dd（Asia/Shanghai）',
  `used`       int         NOT NULL DEFAULT '0' COMMENT '当日已用次数（成功生成才占，失败退回）',
  `updated_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`user_id`, `day`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：发布文案每日额度';

CREATE TABLE IF NOT EXISTS `afv_replicate_export` (
  `id`         bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`    bigint       NOT NULL COMMENT '所属用户ID',
  `job_id`     bigint       NOT NULL COMMENT '任务 afv_replicate_job.id',
  `ratio`      varchar(8)   NOT NULL COMMENT '9:16 / 1:1 / 16:9 / 3:4 / 4:3',
  `mode`       varchar(8)   NOT NULL COMMENT 'pad / crop',
  `src_rid`    varchar(128) NOT NULL COMMENT '被导出的成片 rid',
  `status`     tinyint      NOT NULL DEFAULT '0' COMMENT '0=导出中（占坑）1=已导出',
  `video_rid`  varchar(128) NOT NULL DEFAULT '' COMMENT '导出视频 rid（媒体服务，中间产物私有桶；视图里现签短时地址）',
  `version`    int          NOT NULL DEFAULT '0' COMMENT '对应 afv_replicate_delivery.version（入库发件箱）',
  `created_at` datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at` datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间（导出中超过回收时限的占坑可被接手）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_job_ratio_mode_src` (`job_id`, `ratio`, `mode`, `src_rid`),
  KEY `idx_job` (`job_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：多比例导出';
