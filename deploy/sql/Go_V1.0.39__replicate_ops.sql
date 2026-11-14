-- 视频做同款上线前补齐（next-phase P0-3/4/6；P0-5 站内通知写 Java 的 afv_notification，不在本文件建表）
--
-- 设计：fusion-aigc/.scratch/next-phase/PLAN.md P0-3~P0-6；video-replicate-agent/DESIGN.md §5.1a、§5.16、§10；EXPERIENCE.md §3.7.2、§3.8.3、§8.5。
-- 编号：Go_V1.0.39（37 视频做同款第一切片、38 画布智能体调用；40 留给画布实时同步）。上线前在各目标库 SELECT 已执行版本核对。
--
-- 内容：
--   1. （无）站内通知改写 Java 通知中心表 afv_notification（V1.0.99），其 link / biz_type / biz_id / dedupe_key 补列与
--      唯一键 uk_dedupe_key 由 Java 迁移负责；Go 只经 pkg/notify 写。
--   2. 视频做同款自有表：afv_replicate_moderation（方案审核结论）、afv_replicate_delivery（成片 / 单段入素材库发件箱）、
--      afv_replicate_admin_action（后台人工退点 / 强制收尾审计）、afv_replicate_admin_refund_line（人工退点逐单明细）
--   3. afv_replicate_job 补列：deleted（任务中心同步需要源表有 deleted）、tc_updated_at（任务中心同步水位：没有 ON UPDATE，
--      只在业务状态写路径显式 SET NOW(3)——状态 / 段数 / 成片 / 预留镜像 / 入库；租约与 next_run_at 不碰它，免得同步抖动）
--   4. 素材库分类 video_replicate（一级「视频」下，afv_asset_category + afv_asset_category_entry）
--   5. 后台权限码 replicate:view / replicate:refund / replicate:force（写法同 Go_V1.0.36：NOT EXISTS 守卫，重复执行无副作用）
--
-- 只新增 Go 自有表 / 列（afv_replicate_*）与后台权限数据，不改任何 Java 表结构；可重复执行（IF NOT EXISTS；补列 / 索引用存储过程查 information_schema）。
-- 排序规则统一 utf8mb4_unicode_ci（任务中心同步联表避免 1267）。
SET NAMES utf8mb4;

-- ---------------------------------------------------------------- 2. 视频做同款自有表

CREATE TABLE IF NOT EXISTS `afv_replicate_moderation` (
  `id`          bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`     bigint        NOT NULL COMMENT '所属用户ID',
  `plan_id`     bigint        NOT NULL COMMENT 'afv_replicate_plan.id',
  `plan_no`     varchar(32)   NOT NULL COMMENT '方案号（审核日志 afv_moderation_log.request_id = replicate:<plan_no>）',
  `plan_hash`   char(64)      NOT NULL COMMENT '审的是哪一版方案（指纹变了要重审）',
  `policy_sig`  varchar(64)   NOT NULL DEFAULT '' COMMENT '审核策略签名（fail_open / review_as_block / 模型组）：策略变了要重审',
  `attempt`     int           NOT NULL DEFAULT '0' COMMENT '重审轮次（每次作废重审 +1，参与网关幂等键，重审一定是新调用）',
  `status`      varchar(16)   NOT NULL COMMENT 'PASSED 通过 / REJECTED 未通过（方案不可确认）/ ERROR_OPEN 审核服务故障按 fail-open 放行',
  `reason`      varchar(255)  NOT NULL DEFAULT '' COMMENT '未通过的用户口径原因（不含命中词）',
  `detail`      json          DEFAULT NULL COMMENT '内部明细：各类结论、命中标签、网关调用号（后台可见，不下发用户）',
  `created_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_plan_hash` (`plan_id`, `plan_hash`),
  KEY `idx_plan_no` (`plan_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：方案确认前的内容审核结论';

CREATE TABLE IF NOT EXISTS `afv_replicate_delivery` (
  `id`            bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `job_id`        bigint        NOT NULL COMMENT 'afv_replicate_job.id',
  `user_id`       bigint        NOT NULL COMMENT '所属用户ID',
  `kind`          varchar(16)   NOT NULL COMMENT 'final 整条成片 / segment 用户选择保存的单段',
  `seg_index`     int           NOT NULL DEFAULT '0' COMMENT '段号（final 为 0）',
  `src_rid`       varchar(128)  NOT NULL COMMENT '入队时冻结的源视频 rid（私有桶；重试用同一个）',
  `cover_rid`     varchar(128)  DEFAULT NULL COMMENT '封面 rid（首帧；没有为空）',
  `status`        tinyint       NOT NULL DEFAULT '0' COMMENT '0 待入库 / 1 已入库 / 2 放弃（永久失败或超过重试次数）',
  `attempts`      int           NOT NULL DEFAULT '0' COMMENT '已尝试次数',
  `lease_token`   varchar(64)   DEFAULT NULL COMMENT '认领令牌（CAS 认领 / 完成都带它，被接手的旧执行者写入落空）',
  `lease_until`   datetime(3)   DEFAULT NULL COMMENT '认领到期（库时钟）',
  `next_run_at`   datetime(3)   DEFAULT NULL COMMENT '下次尝试时间（NULL=立即；失败指数退避）',
  `video_url`     varchar(1024) DEFAULT NULL COMMENT '素材库公有桶地址（入库成功后回填；任务中心 result_url 读它）',
  `cover_url`     varchar(1024) DEFAULT NULL COMMENT '封面公有地址（任务中心 cover_url 读它）',
  `last_error`    varchar(255)  DEFAULT NULL COMMENT '最近一次失败原因（内部）',
  `created_at`    datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`    datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  `finished_at`   datetime(3)   DEFAULT NULL COMMENT '入库 / 放弃时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_job_kind_seg` (`job_id`, `kind`, `seg_index`),
  KEY `idx_status_next` (`status`, `next_run_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：成片 / 单段入素材库发件箱（同 job 同对象只入库一次）';

CREATE TABLE IF NOT EXISTS `afv_replicate_admin_action` (
  `id`           bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `job_id`       bigint        NOT NULL COMMENT 'afv_replicate_job.id',
  `admin_id`     bigint        NOT NULL COMMENT '操作的管理员ID',
  `action`       varchar(16)   NOT NULL COMMENT 'REFUND 人工退点 / FORCE_FINISH 强制收尾',
  `seg_index`    int           NOT NULL DEFAULT '0' COMMENT '段号（强制收尾为 0）',
  `request_key`  varchar(64)   NOT NULL COMMENT '幂等键（请求头 Idempotency-Key；没带时按 动作+段+原因 派生）',
  `reason`       varchar(255)  NOT NULL COMMENT '原因（必填）',
  `points`       bigint        NOT NULL DEFAULT '0' COMMENT '本次实际退回的点数',
  `status`       varchar(16)   NOT NULL DEFAULT 'RUNNING' COMMENT 'RUNNING 处理中 / DONE 完成 / FAILED 失败',
  `lease_until`  datetime(3)   DEFAULT NULL COMMENT '处理中的认领到期（库时钟）：过期的 RUNNING 可被同幂等键接管（逐单明细见 afv_replicate_admin_refund_line）',
  `lease_token`  varchar(64)   DEFAULT NULL COMMENT '认领令牌：占用 / 接管时换新，结果写回带令牌条件（被接管的旧执行者写入落空）',
  `result`       json          DEFAULT NULL COMMENT '逐单结果（请求号、处理前真实状态、退回点数、错误）',
  `created_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_job_action_key` (`job_id`, `action`, `request_key`),
  KEY `idx_job` (`job_id`, `id`),
  KEY `idx_admin_created` (`admin_id`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：后台人工动钱 / 收尾审计';

CREATE TABLE IF NOT EXISTS `afv_replicate_admin_refund_line` (
  `id`              bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `action_id`       bigint        NOT NULL COMMENT 'afv_replicate_admin_action.id',
  `job_id`          bigint        NOT NULL COMMENT 'afv_replicate_job.id',
  `seg_index`       int           NOT NULL COMMENT '段号',
  `request_id`      varchar(64)   NOT NULL COMMENT '计费请求号（afv_replicate_charge.request_id）',
  `reservation_no`  varchar(64)   NOT NULL DEFAULT '' COMMENT '预留单号',
  `before_status`   varchar(32)   NOT NULL DEFAULT '' COMMENT '退之前的真实状态（HELD / CONFIRMED / PARTIALLY_REFUNDED）',
  `expected_points` bigint        NOT NULL DEFAULT '0' COMMENT '退之前算出的可退点数',
  `points`          bigint        NOT NULL DEFAULT '0' COMMENT '本动作实际退回的点数（以计费流水为准：流水描述带本动作标记才算）',
  `status`          varchar(16)   NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING 动钱前先落 / DONE 已确认结果 / FAILED 调用失败（可同键重试）',
  `error`           varchar(255)  DEFAULT NULL COMMENT '失败原因（内部）',
  `created_at`      datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`      datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_action_request` (`action_id`, `request_id`),
  KEY `idx_job` (`job_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：后台人工退点逐单明细（动钱前先落 PENDING，崩溃后已动的钱可见）';

-- ---------------------------------------------------------------- 3. afv_replicate_job 补列（任务中心同步）

DROP PROCEDURE IF EXISTS `go_v1039_add_column`;
DROP PROCEDURE IF EXISTS `go_v1039_add_index`;
DELIMITER $$
CREATE PROCEDURE `go_v1039_add_column`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_def TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD COLUMN `', p_col, '` ', p_def);
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
CREATE PROCEDURE `go_v1039_add_index`(IN p_table VARCHAR(64), IN p_index VARCHAR(64), IN p_cols TEXT)
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

CALL `go_v1039_add_column`('afv_replicate_job', 'deleted',
  'tinyint NOT NULL DEFAULT ''0'' COMMENT ''逻辑删除（任务中心同步只会把 1 带过去；本功能暂无用户删除入口）'' AFTER `finished_at`');
CALL `go_v1039_add_column`('afv_replicate_job', 'tc_updated_at',
  'datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT ''任务中心同步水位：业务状态写路径显式 SET NOW(3)（无 ON UPDATE，租约 / next_run_at 不触发）'' AFTER `deleted`');
CALL `go_v1039_add_column`('afv_replicate_moderation', 'policy_sig',
  'varchar(64) NOT NULL DEFAULT '''' COMMENT ''审核策略签名（fail_open / review_as_block / 模型组）：策略变了要重审'' AFTER `plan_hash`');
CALL `go_v1039_add_column`('afv_replicate_moderation', 'attempt',
  'int NOT NULL DEFAULT ''0'' COMMENT ''重审轮次（每次作废重审 +1，参与网关幂等键）'' AFTER `policy_sig`');
CALL `go_v1039_add_column`('afv_replicate_admin_action', 'lease_token',
  'varchar(64) DEFAULT NULL COMMENT ''认领令牌：占用 / 接管时换新，结果写回带令牌条件'' AFTER `status`');
CALL `go_v1039_add_column`('afv_replicate_admin_action', 'lease_until',
  'datetime(3) DEFAULT NULL COMMENT ''处理中的认领到期（库时钟）：过期的 RUNNING 可被同幂等键接管'' AFTER `status`');
CALL `go_v1039_add_index`('afv_replicate_job', 'idx_tc_updated', '`tc_updated_at`, `id`');

DROP PROCEDURE IF EXISTS `go_v1039_add_column`;
DROP PROCEDURE IF EXISTS `go_v1039_add_index`;

-- ---------------------------------------------------------------- 3b. 素材库分类（写法同 Go_V1.0.25 画布）
-- 成片 / 单段入素材库的 entry_code = video_replicate（同计费 Offer），挂在一级「视频」（category_code=video，与 canvas_video 同级）下。
-- media_type 按 Java V1.0.60 口径只在一级有意义，二级留空（视频形态由父级「视频」决定，与 canvas_video 一致）。

INSERT INTO `afv_asset_category` (`category_code`,`category_name`,`parent_id`,`level`,`media_type`,`sort_order`,`status`)
SELECT * FROM (
  SELECT 'video_replicate' AS category_code, '视频做同款' AS category_name,
         (SELECT c.id FROM afv_asset_category c WHERE c.category_code='video' AND c.level=1 AND c.deleted=0 LIMIT 1) AS parent_id,
         2 AS level, '' AS media_type, 120 AS sort_order, 1 AS status
) t
WHERE t.parent_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category` c WHERE c.`category_code` = t.category_code AND c.`deleted` = 0);

INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, c.`category_code`, 10 FROM `afv_asset_category` c
WHERE c.`category_code` = 'video_replicate' AND c.`deleted` = 0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id` = c.`id` AND e.`entry_code` = c.`category_code`);

-- ---------------------------------------------------------------- 4. 后台权限码
--   replicate:view    检索 / 详情（super_admin、operator）
--   replicate:refund  人工退点（super_admin）
--   replicate:force   强制收尾（super_admin）

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'replicate:view' AS permission_code, '{"zh-CN": "查看视频做同款任务", "en-US": "View Video Replicate Jobs", "ja-JP": "動画リプリケート閲覧", "ko-KR": "영상 따라 만들기 조회"}' AS permission_name, 'replicate' AS resource, 'view' AS action, '视频做同款任务检索与详情（段、预留单、网关调用、失败原因、审核记录）' AS description
  UNION ALL SELECT 'replicate:refund', '{"zh-CN": "视频做同款人工退点", "en-US": "Refund Video Replicate Segment", "ja-JP": "動画リプリケート返金", "ko-KR": "영상 따라 만들기 환불"}', 'replicate', 'refund', '按段人工退回点数（走 pkg/billing 幂等退回，原因必填，留审计）'
  UNION ALL SELECT 'replicate:force', '{"zh-CN": "视频做同款强制收尾", "en-US": "Force Finish Video Replicate Job", "ja-JP": "動画リプリケート強制終了", "ko-KR": "영상 따라 만들기 강제 종료"}', 'replicate', 'force', '非在途任务强制收尾（先结清未结预留镜像，原因必填，留审计）'
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_resource` (`resource_code`, `resource_name`, `description`, `sort_order`, `status`)
SELECT 'replicate', '{"zh-CN": "视频做同款", "en-US": "Video Replicate", "ja-JP": "動画リプリケート", "ko-KR": "영상 따라 만들기"}',
       '视频做同款（Go replicate 域）的后台权限', 0, 1
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_resource` r WHERE r.`resource_code` = 'replicate');

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code IN ('replicate:view', 'replicate:refund', 'replicate:force')
WHERE r.role_code = 'super_admin'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'replicate:view'
WHERE r.role_code = 'operator'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
