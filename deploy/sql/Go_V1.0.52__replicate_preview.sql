-- ============================================================
-- 视频做同款：定妆图（免费，每人每天 N 次）
--
-- 方案确认前，用原片第 1 段的代表帧 + 方案参考图 + 改法，经网关 image.edit 出一张「替换后的第 1 帧」，让用户先看效果再付费。
--   1. afv_replicate_preview        一张方案的定妆图；(user_id, plan_hash) 唯一：同一方案只生成一次，重复调用回放（不占额度）
--   2. afv_replicate_preview_quota  按用户、按自然日（Asia/Shanghai，day 由应用层按该时区算出）的已用次数；
--                                   用「有余量才 +1」的原子更新占额度，出图失败再退回
--
-- 编号：Go_V1.0.52（所有分支 deploy/sql 最高 Go_V1.0.50；Go_V1.0.51 为本工作区未提交的 delivery_version）。上线前在各目标库核对已执行版本。
-- 只新增 Go 自有表，不改任何 Java 表；可重复执行（IF NOT EXISTS）。排序规则统一 utf8mb4_unicode_ci。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_preview` (
  `id`         bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`    bigint        NOT NULL COMMENT '所属用户ID',
  `plan_id`    bigint        NOT NULL COMMENT '方案 afv_replicate_plan.id',
  `plan_no`    varchar(32)   NOT NULL COMMENT '方案号（冗余）',
  `plan_hash`  varchar(64)   NOT NULL COMMENT '方案指纹（同一指纹只出一张）',
  `status`     tinyint       NOT NULL DEFAULT '0' COMMENT '0=出图中（占坑）1=已出图',
  `image_rid`  varchar(128)  NOT NULL DEFAULT '' COMMENT '定妆图 rid（媒体服务，中间产物私有桶；视图里现签短时地址）',
  `frame_rid`  varchar(128)  NOT NULL DEFAULT '' COMMENT '输入的原片帧 rid',
  `frame_at_s` double        NOT NULL DEFAULT '0' COMMENT '该帧在原片里的秒数',
  `call_id`    varchar(64)   NOT NULL DEFAULT '' COMMENT '网关调用号（排查用）',
  `created_at` datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at` datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间（出图中超过回收时限的占坑可被接手）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_hash` (`user_id`, `plan_hash`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：定妆图（一方案一张）';

CREATE TABLE IF NOT EXISTS `afv_replicate_preview_quota` (
  `user_id`    bigint      NOT NULL COMMENT '用户ID',
  `day`        char(10)    NOT NULL COMMENT '自然日 yyyy-MM-dd（Asia/Shanghai）',
  `used`       int         NOT NULL DEFAULT '0' COMMENT '当日已用次数（成功出图才占，失败退回）',
  `updated_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`user_id`, `day`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：定妆图每日额度';
