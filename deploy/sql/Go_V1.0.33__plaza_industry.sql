-- ============================================================
-- 智能体模板页：行业目录、模板归类、用户偏好、模板生命周期、审计，以及行业建议 / 搜索缺口 / 示例素材 / 渠道字典。
-- 设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 第 4、16、17 节。
--
-- 表归属（一个表一个写者）：
--   Go 独写：afv_template_industry、afv_plaza_template_industry、afv_user_industry_pref、afv_plaza_audit_log、
--            afv_plaza_channel、afv_plaza_industry_suggestion(+_status)、afv_plaza_search_miss_daily / _term、afv_plaza_sample_asset
--   接管表（Java 创建）：afv_compat_plaza_template —— 只 ADD COLUMN / ADD KEY，不改既有列的含义与类型（Java 的 Agent 运行时仍读它）。
-- 种子与回填在 Go_V1.0.34（DDL 与数据分文件，DDL 失败重放不重复灌种子）。
-- 可重复执行：建表均 IF NOT EXISTS；接管表加列（MySQL 8.0 无 ADD COLUMN IF NOT EXISTS）包在存储过程里，
-- 以 `state` 列是否存在为准整条 ALTER（单语句，要么全加要么全不加）+ 一次性回填只做一次。不需要 migrate.sh 的 guard_done。
-- ============================================================

CREATE TABLE IF NOT EXISTS `afv_template_industry` (
  `id`                BIGINT       NOT NULL AUTO_INCREMENT,
  `code`              VARCHAR(64)  NOT NULL COMMENT '稳定标识（小写字母数字下划线），接口与埋点用它，创建后不可改',
  `name`              VARCHAR(32)  NOT NULL COMMENT '展示名',
  `parent_id`         BIGINT       NULL     COMMENT '一级为 NULL；只允许两级',
  `icon_url`          VARCHAR(512) NULL,
  `cover_url`         VARCHAR(512) NULL,
  `hint`              VARCHAR(120) NULL     COMMENT '行业门按钮下的副标题；后台新建/启用时必填',
  `audience_name`     VARCHAR(32)  NULL     COMMENT '人群称呼，如「足浴人」',
  `examples_json`     JSON         NULL     COMMENT '输入框示例文案数组（≤5，每条 ≤40 字）',
  `next_actions_json` JSON         NULL     COMMENT '成品后的下一步建议数组（≤4，每条 ≤20 字）',
  `sort_order`        INT          NOT NULL DEFAULT 0,
  `enabled`           TINYINT      NOT NULL DEFAULT 1,
  `show_when_empty`   TINYINT      NOT NULL DEFAULT 0 COMMENT '1=没有可见模板也在行业门里展示',
  `created_by`        BIGINT       NULL,
  `updated_by`        BIGINT       NULL,
  `create_time`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted`           TINYINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_template_industry_code` (`code`),
  KEY `idx_template_industry_tree` (`deleted`, `enabled`, `parent_id`, `sort_order`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模板行业（运营维护，两级）';

CREATE TABLE IF NOT EXISTS `afv_plaza_template_industry` (
  `id`          BIGINT   NOT NULL AUTO_INCREMENT,
  `template_id` BIGINT   NOT NULL COMMENT 'afv_compat_plaza_template.id',
  `industry_id` BIGINT   NOT NULL COMMENT 'afv_template_industry.id（可为一级或二级）',
  `sort_order`  INT      NOT NULL DEFAULT 0 COMMENT '该模板在该行业内的顺序（升序，步长 10）',
  `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_plaza_tpl_industry` (`template_id`, `industry_id`),
  KEY `idx_plaza_tpl_industry_ind` (`industry_id`, `template_id`),
  KEY `idx_plaza_tpl_industry_order` (`industry_id`, `sort_order`, `template_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模板与行业多对多（关系行物理删除，不做逻辑删除）';

CREATE TABLE IF NOT EXISTS `afv_user_industry_pref` (
  `id`          BIGINT   NOT NULL AUTO_INCREMENT,
  `user_id`     BIGINT   NOT NULL,
  `industry_id` BIGINT   NOT NULL,
  `is_primary`  TINYINT  NOT NULL DEFAULT 0 COMMENT '同一用户最多一行为 1；v1 UI 只写一行且为 primary',
  `sort_order`  INT      NOT NULL DEFAULT 0,
  `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_industry` (`user_id`, `industry_id`),
  KEY `idx_user_industry_ind` (`industry_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='用户行业偏好（预留多选）';

-- 接管表加列：媒体 + 生命周期 + 渠道/可替换位 + 创建人 + 回收站。Java 的 MyBatis-Plus 实体只映射自己声明的列，不受影响。
DROP PROCEDURE IF EXISTS `go_v1033_plaza_template_columns`;
DELIMITER $$
CREATE PROCEDURE `go_v1033_plaza_template_columns`()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_compat_plaza_template' AND COLUMN_NAME = 'state') THEN
        ALTER TABLE `afv_compat_plaza_template`
          ADD COLUMN `media_type`          VARCHAR(8)    NOT NULL DEFAULT 'image' COMMENT 'image|video',
          ADD COLUMN `video_url`           VARCHAR(1024) NULL COMMENT '视频模板预览视频（公有桶）',
          ADD COLUMN `poster_url`          VARCHAR(1024) NULL COMMENT '视频封面；空则取 data_json.result[0]',
          ADD COLUMN `state`               VARCHAR(10)   NOT NULL DEFAULT 'PUBLISHED' COMMENT 'DRAFT | PUBLISHED | OFFLINE（与回收站 deleted 正交）',
          ADD COLUMN `online_at`           DATETIME      NULL COMMENT '定时上线；NULL=立即',
          ADD COLUMN `offline_at`          DATETIME      NULL COMMENT '定时下线；NULL=不限',
          ADD COLUMN `channels_json`       JSON          NULL COMMENT '目标渠道（code 或带宽高覆盖的对象）数组，主渠道在前；原生接口使用，不进 v1 payload',
          ADD COLUMN `slots_json`          JSON          NULL COMMENT '可替换位定义数组；原生接口使用，不进 v1 payload',
          ADD COLUMN `needs_product_image` TINYINT       NOT NULL DEFAULT 1 COMMENT '做同款是否需要用户上传产品图',
          ADD COLUMN `text_overlay`        TINYINT       NOT NULL DEFAULT 0 COMMENT '1=文字走后期叠加（P1）',
          ADD COLUMN `media_status`        VARCHAR(8)    NOT NULL DEFAULT 'READY' COMMENT '视频转码/抽帧状态 PENDING|READY|FAILED；视频模板只有 READY 才可发布',
          ADD COLUMN `created_by`          BIGINT        NULL,
          ADD COLUMN `updated_by`          BIGINT        NULL,
          ADD COLUMN `deleted_at`          DATETIME      NULL COMMENT '进回收站时间；30 天后由清理任务物理删除',
          ADD COLUMN `deleted_by`          BIGINT        NULL,
          ADD KEY `idx_compat_plaza_tpl_media` (`enabled`, `deleted`, `media_type`, `sort_order`, `id`),
          ADD KEY `idx_compat_plaza_tpl_state` (`deleted`, `state`, `online_at`, `offline_at`),
          ADD KEY `idx_compat_plaza_tpl_creator` (`created_by`, `deleted`);
        -- 存量行：Java 的 enabled=1 即已发布，enabled=0 即已下线（一次性，只在刚加列时做）
        UPDATE `afv_compat_plaza_template` SET `state` = IF(`enabled` = 1, 'PUBLISHED', 'OFFLINE');
    END IF;
END$$
DELIMITER ;
CALL `go_v1033_plaza_template_columns`();
DROP PROCEDURE IF EXISTS `go_v1033_plaza_template_columns`;

-- 乐观并发版本号：Go 每次写模板 +1，编辑接口以 `WHERE id = ? AND revision = ?` 的影响行数判冲突
-- （update_time 只精确到秒，同一秒内的两次保存比不出来）。单独判存在，已建过 state 等列的库重放也能补上。
DROP PROCEDURE IF EXISTS `go_v1033_plaza_template_revision`;
DELIMITER $$
CREATE PROCEDURE `go_v1033_plaza_template_revision`()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_compat_plaza_template' AND COLUMN_NAME = 'revision') THEN
        ALTER TABLE `afv_compat_plaza_template`
          ADD COLUMN `revision` INT NOT NULL DEFAULT 0 COMMENT '乐观并发版本号（Go 每次写 +1；Java 不写）';
    END IF;
END$$
DELIMITER ;
CALL `go_v1033_plaza_template_revision`();
DROP PROCEDURE IF EXISTS `go_v1033_plaza_template_revision`;

CREATE TABLE IF NOT EXISTS `afv_plaza_audit_log` (
  `id`            BIGINT       NOT NULL AUTO_INCREMENT,
  `operator_id`   BIGINT       NULL,
  `operator_name` VARCHAR(64)  NULL,
  `action`        VARCHAR(48)  NOT NULL COMMENT 'TEMPLATE_CREATE/UPDATE/DUPLICATE/PUBLISH/OFFLINE/SCHEDULE/DELETE/RESTORE/PURGE/BATCH_*、INDUSTRY_*、CHANNEL_*、SAMPLE_*、SUGGESTION_*、SEARCH_MISS_*、HEADLINE_*',
  `resource_type` VARCHAR(24)  NOT NULL COMMENT 'TEMPLATE | INDUSTRY | CHANNEL | SAMPLE | SUGGESTION | SEARCH_MISS | HEADLINE_RULE | HEADLINE_BLOCK | HEADLINE_PARAMS',
  `resource_id`   VARCHAR(64)  NULL,
  `batch_id`      CHAR(36)     NULL COMMENT '批量操作各行共用',
  `before_json`   JSON         NULL COMMENT '改前快照（创建为空）',
  `after_json`    JSON         NULL COMMENT '改后快照（删除可空）',
  `diff_json`     JSON         NULL COMMENT '字段级变化 {field:{from,to}}，大字段仅记长度与哈希',
  `status`        VARCHAR(12)  NOT NULL DEFAULT 'SUCCESS',
  `error_message` VARCHAR(512) NULL,
  `request_id`    VARCHAR(64)  NULL,
  `client_ip`     VARCHAR(64)  NULL,
  `create_time`   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_plaza_audit_resource` (`resource_type`, `resource_id`, `id`),
  KEY `idx_plaza_audit_operator` (`operator_id`, `create_time`),
  KEY `idx_plaza_audit_action` (`action`, `create_time`),
  KEY `idx_plaza_audit_batch` (`batch_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='广场模板域审计（Go 独写；不写 Java 的 afv_admin_audit_log）；保留 ≥1 年';

CREATE TABLE IF NOT EXISTS `afv_plaza_channel` (
  `id`          BIGINT       NOT NULL AUTO_INCREMENT,
  `code`        VARCHAR(48)  NOT NULL COMMENT '如 meituan_head / dianping / douyin_9_16 / xhs_3_4 / taobao_1_1',
  `name`        VARCHAR(32)  NOT NULL,
  `media_type`  VARCHAR(8)   NOT NULL DEFAULT 'image' COMMENT 'image|video',
  `width`       INT          NOT NULL COMMENT '默认输出像素宽',
  `height`      INT          NOT NULL COMMENT '默认输出像素高',
  `ratio`       VARCHAR(8)   NOT NULL COMMENT '如 9:16',
  `note`        VARCHAR(120) NULL COMMENT '平台规范备注/核对日期',
  `sort_order`  INT          NOT NULL DEFAULT 0,
  `enabled`     TINYINT      NOT NULL DEFAULT 1,
  `create_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_plaza_channel_code` (`code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='投放渠道字典与默认像素规格（运营维护）';

CREATE TABLE IF NOT EXISTS `afv_plaza_industry_suggestion` (
  `id`          BIGINT       NOT NULL AUTO_INCREMENT,
  `text`        VARCHAR(80)  NULL     COMMENT '清洗后的原文（≤40 字）',
  `norm_text`   VARCHAR(80)  NULL     COMMENT '规范化文本（小写、去空白标点），用于聚合；text 为空时为 NULL',
  `sid`         CHAR(32)     NOT NULL COMMENT '页面会话随机 id，不绑定账号',
  `user_id`     BIGINT       NULL,
  `ip_hash`     CHAR(16)     NOT NULL COMMENT 'IP 的加盐哈希，只用于限频，不可逆',
  `create_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_plaza_suggest_norm` (`norm_text`, `create_time`),
  KEY `idx_plaza_suggest_sid` (`sid`, `create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='行业建议（匿名，不含联系方式）';

CREATE TABLE IF NOT EXISTS `afv_plaza_industry_suggestion_status` (
  `norm_text`   VARCHAR(80)  NOT NULL,
  `status`      VARCHAR(10)  NOT NULL DEFAULT 'NEW' COMMENT 'NEW | ADOPTED | IGNORED',
  `industry_id` BIGINT       NULL     COMMENT 'ADOPTED 时关联新建的行业',
  `handled_by`  BIGINT       NULL,
  `handled_at`  DATETIME     NULL,
  PRIMARY KEY (`norm_text`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='行业建议处理状态';

CREATE TABLE IF NOT EXISTS `afv_plaza_search_miss_daily` (
  `day`           DATE         NOT NULL,
  `industry_code` VARCHAR(64)  NOT NULL COMMENT '行业 code（一级或二级，按用户当时所选）',
  `norm_term`     VARCHAR(60)  NOT NULL,
  `cnt`           INT          NOT NULL DEFAULT 0 COMMENT '当日去重计数',
  PRIMARY KEY (`day`, `industry_code`, `norm_term`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='行业内零结果搜索词（日聚合，无任何用户标识）；保留 90 天';

CREATE TABLE IF NOT EXISTS `afv_plaza_search_miss_term` (
  `industry_code` VARCHAR(64)  NOT NULL,
  `norm_term`     VARCHAR(60)  NOT NULL,
  `total_cnt`     INT          NOT NULL DEFAULT 0,
  `first_day`     DATE         NOT NULL,
  `last_day`      DATE         NOT NULL,
  `status`        VARCHAR(10)  NOT NULL DEFAULT 'NEW' COMMENT 'NEW | HANDLED | IGNORED',
  `handled_by`    BIGINT       NULL,
  `handled_at`    DATETIME     NULL,
  PRIMARY KEY (`industry_code`, `norm_term`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='零结果搜索词汇总与处理状态';

CREATE TABLE IF NOT EXISTS `afv_plaza_sample_asset` (
  `id`          BIGINT        NOT NULL AUTO_INCREMENT,
  `scope_type`  VARCHAR(10)   NOT NULL COMMENT 'INDUSTRY | TEMPLATE',
  `industry_id` BIGINT        NULL     COMMENT 'scope_type=INDUSTRY 时必填',
  `template_id` BIGINT        NULL     COMMENT 'scope_type=TEMPLATE 时必填',
  `kind`        VARCHAR(8)    NOT NULL DEFAULT 'image' COMMENT 'image',
  `url`         VARCHAR(1024) NOT NULL COMMENT '公有桶',
  `thumb_url`   VARCHAR(1024) NULL,
  `title`       VARCHAR(40)   NOT NULL COMMENT '如「示例：足浴店门头」',
  `role`        VARCHAR(6)    NOT NULL DEFAULT 'OUTPUT' COMMENT 'INPUT=放进去的素材 | OUTPUT=做出来的效果',
  `sort_order`  INT           NOT NULL DEFAULT 0,
  `enabled`     TINYINT       NOT NULL DEFAULT 1,
  `created_by`  BIGINT        NULL,
  `create_time` DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time` DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `deleted`     TINYINT       NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  KEY `idx_plaza_sample_scope` (`scope_type`, `industry_id`, `template_id`, `deleted`, `enabled`, `sort_order`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='示例素材（行业级或模板级，运营维护）';

-- 示例素材角色：已建过表的库补 role 列（判存在，可重复执行）。
DROP PROCEDURE IF EXISTS `go_v1033_plaza_sample_role`;
DELIMITER $$
CREATE PROCEDURE `go_v1033_plaza_sample_role`()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_plaza_sample_asset' AND COLUMN_NAME = 'role') THEN
        ALTER TABLE `afv_plaza_sample_asset`
          ADD COLUMN `role` VARCHAR(6) NOT NULL DEFAULT 'OUTPUT' COMMENT 'INPUT=放进去的素材 | OUTPUT=做出来的效果' AFTER `title`;
    END IF;
END$$
DELIMITER ;
CALL `go_v1033_plaza_sample_role`();
DROP PROCEDURE IF EXISTS `go_v1033_plaza_sample_role`;
