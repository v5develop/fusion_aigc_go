-- 视频做同款：示例视频（共享只读分析 + 预计算示例成片）、我的方案、改标注（方案取代关系）
--
-- 设计：fusion-aigc/.scratch/video-replicate-agent/DESIGN.md §5.18、§8.2；EXPERIENCE.md §2.2、§3.4.1、§3.9.2。契约：docs/api/api-replicate.md §10–§11。
-- 编号：Go_V1.0.41（37 第一切片、39 上线补齐、38 / 40 画布）。上线前在各目标库 SELECT 已执行版本核对。
--
-- 内容：
--   1. afv_replicate_sample：运营登记的示例视频；分析以 user_id=0 跑在 afv_replicate_analysis（analysis_id 已发布 / pending_analysis_id 进行中）
--   2. afv_replicate_saved_plan：用户「我的方案」（每人 ≤50，应用层按用户串行；同一方案只存一份 uk_user_plan）
--   3. afv_replicate_plan 补列：sample_id（用示例出的方案）、parent_plan_id（由哪张方案改标注而来）、superseded_by（被哪张新方案取代，status=STALE）
--   4. 后台权限码 replicate:sample（super_admin、operator）
--   5. afv_replicate_charge 补列 held_at（预留续期起算点）
--
-- 只新增 Go 自有表 / 列（afv_replicate_*）与后台权限数据，不改任何 Java 表结构；可重复执行（IF NOT EXISTS；补列 / 索引用存储过程查
-- information_schema，表不存在跳过；权限数据 NOT EXISTS 守卫）。排序规则统一 utf8mb4_unicode_ci。
SET NAMES utf8mb4;

-- ---------------------------------------------------------------- 1. 示例视频

CREATE TABLE IF NOT EXISTS `afv_replicate_sample` (
  `id`                  bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `title`               varchar(64)   NOT NULL COMMENT '标题（用户可见）',
  `industry`            varchar(64)   NOT NULL DEFAULT '' COMMENT '行业 code（afv_template_industry.code）；空=通用，所有行业都展示',
  `source_rid`          varchar(128)  NOT NULL COMMENT '示例原片 rid（媒体服务；共享只读，分析与出片切段只读取）',
  `cover_rid`           varchar(128)  DEFAULT NULL COMMENT '封面 rid',
  `result_rid`          varchar(128)  DEFAULT NULL COMMENT '预计算示例成片 rid（运营预先生成上传；用户试看不触发任何生成）',
  `annotations`         json          DEFAULT NULL COMMENT '示例的改法（与方案 annotations 同结构），「用这个示例的方案」时带出',
  `sort_order`          int           NOT NULL DEFAULT '0' COMMENT '排序（小在前）',
  `enabled`             tinyint       NOT NULL DEFAULT '0' COMMENT '1=启用（还须有已发布的分析，用户端才看得到）',
  `analysis_id`         bigint        DEFAULT NULL COMMENT '已发布的分析 afv_replicate_analysis.id（user_id=0）',
  `pending_analysis_id` bigint        DEFAULT NULL COMMENT '正在跑的预分析 afv_replicate_analysis.id；成功后（读示例时惰性）替换 analysis_id',
  `analysis_round`      int           NOT NULL DEFAULT '0' COMMENT '第几轮预分析（参与 source_key：sha256(sample:<id>:<轮次>)，每轮是新的一行）',
  `analysis_error`      varchar(255)  DEFAULT NULL COMMENT '最近一轮预分析失败原因（不影响已发布的分析）',
  `analyzed_at`         datetime(3)   DEFAULT NULL COMMENT '当前已发布分析的发布时间',
  `created_by`          bigint        NOT NULL DEFAULT '0' COMMENT '创建人（管理员ID）',
  `updated_by`          bigint        NOT NULL DEFAULT '0' COMMENT '最后修改人（管理员ID）',
  `deleted`             tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除（已建方案引用的分析行不受影响）',
  `created_at`          datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`          datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  KEY `idx_visible` (`deleted`, `enabled`, `industry`, `sort_order`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：示例视频（共享只读分析 + 预计算示例成片）';

-- ---------------------------------------------------------------- 2. 我的方案

CREATE TABLE IF NOT EXISTS `afv_replicate_saved_plan` (
  `id`               bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`          bigint        NOT NULL COMMENT '所属用户ID',
  `name`             varchar(64)   NOT NULL COMMENT '方案名（≤40 字）',
  `plan_id`          bigint        NOT NULL COMMENT '保存自哪张方案 afv_replicate_plan.id',
  `plan_no`          varchar(32)   NOT NULL COMMENT '方案号（冗余）',
  `analysis_id`      bigint        NOT NULL COMMENT '原片分析 afv_replicate_analysis.id（「用它做」不传新视频时沿用）',
  `sample_id`        bigint        DEFAULT NULL COMMENT '用示例出的方案：示例 id',
  `annotations`      json          NOT NULL COMMENT '改法（规范化后的标注）',
  `refs`             json          DEFAULT NULL COMMENT '参考图 [{index,rid}]',
  `model_group`      varchar(64)   NOT NULL COMMENT '出片模型组',
  `resolution`       varchar(16)   NOT NULL DEFAULT '720p' COMMENT '分辨率',
  `aspect_ratio`     varchar(16)   NOT NULL DEFAULT '' COMMENT '画幅，空=跟随原片',
  `title`            varchar(128)  NOT NULL DEFAULT '' COMMENT '原片一句话概述（保存时冻结）',
  `source_thumb_rid` varchar(128)  DEFAULT NULL COMMENT '原片首帧 rid（保存时冻结）',
  `use_count`        int           NOT NULL DEFAULT '0' COMMENT '「用它做」次数',
  `last_used_at`     datetime(3)   DEFAULT NULL COMMENT '上次「用它做」时间',
  `created_at`       datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`       datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_plan` (`user_id`, `plan_id`),
  KEY `idx_user` (`user_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：我的方案（每人 ≤50）';

-- ---------------------------------------------------------------- 3. afv_replicate_plan 补列（示例来源 / 改标注取代关系）

DROP PROCEDURE IF EXISTS `go_v1041_add_column`;
DROP PROCEDURE IF EXISTS `go_v1041_add_index`;
DELIMITER $$
CREATE PROCEDURE `go_v1041_add_column`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_def TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD COLUMN `', p_col, '` ', p_def);
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
CREATE PROCEDURE `go_v1041_add_index`(IN p_table VARCHAR(64), IN p_index VARCHAR(64), IN p_cols TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND INDEX_NAME = p_index) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD KEY `', p_index, '` (', p_cols, ')');
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
DELIMITER ;

CALL `go_v1041_add_column`('afv_replicate_plan', 'sample_id',
  'bigint DEFAULT NULL COMMENT ''用示例出的方案：afv_replicate_sample.id（analysis_id 为当时已发布的系统分析行）'' AFTER `analysis_id`');
CALL `go_v1041_add_column`('afv_replicate_plan', 'parent_plan_id',
  'bigint DEFAULT NULL COMMENT ''改标注：由哪张方案改来（plan_hash 带它，改回旧标注也是新方案）'' AFTER `sample_id`');
CALL `go_v1041_add_column`('afv_replicate_plan', 'superseded_by',
  'bigint DEFAULT NULL COMMENT ''改标注：被哪张新方案取代（此时 status=STALE，不可确认）'' AFTER `job_id`');
CALL `go_v1041_add_index`('afv_replicate_plan', 'idx_sample', '`sample_id`');

-- afv_replicate_charge.held_at：冻结成功写镜像的时刻（之后不再改）。预留续期从它起算——镜像 updated_at 会被扣费待重试等写路径改写，
-- 推不出冻结时刻；有了它，扣费待重试（6 + confirm:）的单也能安全续期（续期不缩短到期时间，见 api-replicate.md §6）。
CALL `go_v1041_add_column`('afv_replicate_charge', 'held_at',
  'datetime(3) DEFAULT NULL COMMENT ''冻结成功写镜像为已预留的时刻（应用时钟，写一次不再改；续期起算点）。NULL=旧行/没冻结成功'' AFTER `reason`');

DROP PROCEDURE IF EXISTS `go_v1041_add_column`;
DROP PROCEDURE IF EXISTS `go_v1041_add_index`;

-- ---------------------------------------------------------------- 4. 后台权限码
--   replicate:sample  示例视频的新建 / 修改 / 删除 / 触发预分析（super_admin、operator；查看沿用 replicate:view）

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'replicate:sample' AS permission_code, '{"zh-CN": "管理视频做同款示例", "en-US": "Manage Video Replicate Samples", "ja-JP": "動画リプリケート見本管理", "ko-KR": "영상 따라 만들기 예시 관리"}' AS permission_name, 'replicate' AS resource, 'sample' AS action, '示例视频的新建 / 修改 / 删除 / 触发预分析（示例分析共享、不计费）' AS description
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_resource` (`resource_code`, `resource_name`, `description`, `sort_order`, `status`)
SELECT 'replicate', '{"zh-CN": "视频做同款", "en-US": "Video Replicate", "ja-JP": "動画リプリケート", "ko-KR": "영상 따라 만들기"}',
       '视频做同款（Go replicate 域）的后台权限', 0, 1
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_resource` r WHERE r.`resource_code` = 'replicate');

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'replicate:sample'
WHERE r.role_code IN ('super_admin', 'operator')
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
