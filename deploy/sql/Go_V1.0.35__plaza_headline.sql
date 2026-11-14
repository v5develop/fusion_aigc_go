-- ============================================================
-- 「本周头条」按行业推荐（设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md §14）
--
-- 表所有权（§9.2，一个表一个写者）：
--   接管表（Java Flyway V1.0.217 创建）：afv_plaza_headline —— 变成「运营人工推荐规则表」，只 ADD COLUMN / ADD KEY；
--     Java 的 PlazaHeadlineService.listCurrent（v1 回滚目标）只读既有列，新列对它不可见。
--   Go 独写：afv_plaza_headline_block（取消 / 屏蔽）、afv_plaza_headline_ctl（参数 / 摄入水位 / 调度租约）、
--            afv_plaza_headline_snapshot + _item（版本化榜单，条目带 scope_code）。
--   Go 只读：afv_agent_turn（Java 独写；摄入与校准都是业务读，不写）。
--
-- 编号：本文件 1.0.35（全分支已提交最高 Go_V1.0.32；33 行业表 → 34 种子回填 → 35 头条 → 36 后台权限 → 37 做同款 → 38 画布智能体）。
-- 依赖：DDL 不依赖行业表（范围是字符串 scope_code，无外键）；运行时依赖行业与关系数据（Go_V1.0.33/34）。
-- 幂等：ALTER 用存储过程查 information_schema，列 / 索引已存在则跳过；建表均 IF NOT EXISTS，可重复执行，
--       因此不需要在 migrate.sh 的 guard_done 里登记。
-- ============================================================
SET NAMES utf8mb4;

-- 新环境（没有跑过 Java V1.0.217）也能起：结构与 Java 一致，外加本迁移的新列
CREATE TABLE IF NOT EXISTS `afv_plaza_headline` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `plaza_template_id` BIGINT NOT NULL COMMENT '广场模板 id（afv_compat_plaza_template.id）',
    `start_date` DATE NOT NULL COMMENT '生效起始日（含）',
    `end_date` DATE NOT NULL COMMENT '生效截止日（含）',
    `why` VARCHAR(60) NOT NULL COMMENT '一句推荐语（≤60 字）',
    `sort_order` INT NOT NULL DEFAULT 0 COMMENT '同热度时的先后，升序',
    `enabled` TINYINT NOT NULL DEFAULT 1 COMMENT '1 启用 / 0 停用',
    `created_by` BIGINT NULL COMMENT '创建人（管理员 id）',
    `updated_by` BIGINT NULL COMMENT '最后修改人（管理员 id）',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    `deleted` TINYINT NOT NULL DEFAULT 0 COMMENT '逻辑删除',
    PRIMARY KEY (`id`),
    KEY `idx_plaza_headline_window` (`enabled`, `deleted`, `end_date`, `start_date`),
    KEY `idx_plaza_headline_template` (`plaza_template_id`),
    CONSTRAINT `ck_plaza_headline_window` CHECK (`end_date` >= `start_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='智能体首页本周头条（运营精选广场模板）';

DROP PROCEDURE IF EXISTS `go_v1035_add_column`;
DROP PROCEDURE IF EXISTS `go_v1035_add_index`;
DELIMITER $$
CREATE PROCEDURE `go_v1035_add_column`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_def TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD COLUMN `', p_col, '` ', p_def);
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
CREATE PROCEDURE `go_v1035_add_index`(IN p_table VARCHAR(64), IN p_index VARCHAR(64), IN p_cols TEXT)
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

-- 存量行不需要回填：默认范围 * =「所有包含该模板的行业」，与原来的「全局推荐」语义最接近
CALL `go_v1035_add_column`('afv_plaza_headline', 'scope_code',
    'VARCHAR(64) NOT NULL DEFAULT ''*'' COMMENT ''范围：行业 code | *（所有包含该模板的行业范围）；存量行=*；不接受 ALL''');
CALL `go_v1035_add_column`('afv_plaza_headline', 'batch_id',
    'CHAR(36) NULL COMMENT ''同一次「多范围」操作共用，后台据此合并展示''');
CALL `go_v1035_add_column`('afv_plaza_headline', 'pin_position',
    'SMALLINT UNSIGNED NULL COMMENT ''范围内显式名次（1 起）；NULL=按规则排在算法项之前''');
CALL `go_v1035_add_column`('afv_plaza_headline', 'adopted_from_auto',
    'TINYINT NOT NULL DEFAULT 0 COMMENT ''1=运营接管算法项时创建，仅用于展示来源''');
CALL `go_v1035_add_index`('afv_plaza_headline', 'idx_plaza_headline_scope', '`scope_code`, `enabled`, `deleted`, `end_date`');
CALL `go_v1035_add_index`('afv_plaza_headline', 'idx_plaza_headline_batch', '`batch_id`');

DROP PROCEDURE IF EXISTS `go_v1035_add_column`;
DROP PROCEDURE IF EXISTS `go_v1035_add_index`;

CREATE TABLE IF NOT EXISTS `afv_plaza_headline_block` (
  `id`                BIGINT       NOT NULL AUTO_INCREMENT,
  `plaza_template_id` BIGINT       NOT NULL,
  `scope_code`        VARCHAR(64)  NOT NULL DEFAULT '*' COMMENT '范围：行业 code | *；精确匹配，不连带上级；不接受 ALL',
  `kind`              VARCHAR(8)   NOT NULL COMMENT 'CANCEL=取消到某时刻 | BLOCK=永久屏蔽（展示与审计用，生效判据只看 until_at）',
  `until_at`          DATETIME     NULL     COMMENT 'NULL=永久；否则到该时刻前不得出现在该范围的榜单',
  `reason`            VARCHAR(200) NOT NULL,
  `batch_id`          CHAR(36)     NULL,
  `created_by`        BIGINT       NULL,
  `create_time`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time`       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_plaza_headline_block` (`plaza_template_id`, `scope_code`),
  KEY `idx_plaza_headline_block_scope` (`scope_code`, `until_at`),
  KEY `idx_plaza_headline_block_batch` (`batch_id`),
  CONSTRAINT `ck_plaza_headline_block_kind` CHECK (`kind` IN ('CANCEL','BLOCK'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='头条抑制规则（每模板×范围一条，再次操作覆盖；解除=物理删除；过期行读取时忽略）';

CREATE TABLE IF NOT EXISTS `afv_plaza_headline_ctl` (
  `id`              BIGINT       NOT NULL COMMENT '固定 1',
  `params_json`     JSON         NULL     COMMENT '{"global":{…},"overrides":{"<scope_code>":{…}}}；NULL=全用 yaml 默认',
  `params_version`  INT          NOT NULL DEFAULT 0,
  `updated_by`      BIGINT       NULL,
  `ingest_last_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '已摄入的 afv_agent_turn.id 水位；0=未初始化（首次从当前最大 id 起，历史由校准补）',
  `calibrated_at`   DATETIME(3)  NULL     COMMENT '最近一次小时校准完成时刻',
  `lease_owner`     VARCHAR(64)  NULL     COMMENT 'pkg/lease 行租约：本行同时是头条调度任务的选主锁',
  `lease_until`     DATETIME(3)  NULL,
  `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='头条控制行（参数 / 水位 / 调度租约）';
INSERT IGNORE INTO `afv_plaza_headline_ctl` (`id`) VALUES (1);

CREATE TABLE IF NOT EXISTS `afv_plaza_headline_snapshot` (
  `id`              BIGINT       NOT NULL AUTO_INCREMENT COMMENT '即版本号，单调递增；一次刷新=一个版本，含所有范围',
  `computed_at`     DATETIME(3)  NOT NULL,
  `trigger_type`    VARCHAR(12)  NOT NULL COMMENT 'SCHEDULED | OPS | REFRESH_NOW | STARTUP',
  `operator_id`     BIGINT       NULL,
  `params_hash`     CHAR(64)     NOT NULL,
  `params_json`     JSON         NOT NULL COMMENT '生效参数快照（含行业覆盖），复盘「当时为什么是这个榜」',
  `signal_source`   VARCHAR(8)   NOT NULL COMMENT 'REDIS | DB（Redis 不可用时降级）',
  `window_start`    DATETIME     NOT NULL,
  `window_end`      DATETIME     NOT NULL,
  `candidate_count` INT          NOT NULL COMMENT '有复刻信号的模板数（跨范围去重）',
  `scope_count`     INT          NOT NULL COMMENT '本版计算的范围数',
  `item_count`      INT          NOT NULL COMMENT '全部范围的条目总数',
  `scope_stats`     JSON         NULL     COMMENT '{scopeCode:{candidates,eligible,auto,manual,fill,skip:{原因:数},nearMiss:[10 个]}}',
  `duration_ms`     INT          NULL,
  PRIMARY KEY (`id`),
  KEY `idx_plaza_headline_snapshot_time` (`computed_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='头条榜单版本（每次刷新一版，含所有行业范围；保留策略见 §14.6）';

CREATE TABLE IF NOT EXISTS `afv_plaza_headline_snapshot_item` (
  `id`                BIGINT        NOT NULL AUTO_INCREMENT,
  `version`           BIGINT        NOT NULL,
  `scope_code`        VARCHAR(64)   NOT NULL COMMENT '榜单所属范围：行业 code | ALL（ALL 仅后台，用户侧接口不可读）',
  `plaza_template_id` BIGINT        NOT NULL,
  `rank_no`           SMALLINT UNSIGNED NOT NULL COMMENT '该范围内最终名次（rank 是 MySQL 8 保留字，故用 rank_no）',
  `source`            VARCHAR(8)    NOT NULL COMMENT 'AUTO | MANUAL | FILL（冷启动补位：同范围最新上架，无热度）',
  `headline_id`       BIGINT        NULL     COMMENT 'MANUAL 时指向 afv_plaza_headline.id',
  `score`             DECIMAL(10,4) NULL     COMMENT 'AUTO 总分；MANUAL / FILL 为 NULL',
  `score_detail`      JSON          NULL,
  `why`               VARCHAR(60)   NOT NULL,
  `uses7d`            INT           NOT NULL COMMENT 'FILL 为 0',
  `prev_uses`         INT           NOT NULL,
  `trend`             INT           NULL,
  `is_new`            TINYINT       NOT NULL,
  `unique_users`      INT           NULL,
  `entered_at`        DATETIME(3)   NOT NULL COMMENT '本模板在该范围连续在榜的起点（驻留判断、历史回看）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_plaza_headline_item` (`version`, `scope_code`, `plaza_template_id`),
  KEY `idx_plaza_headline_item_rank` (`version`, `scope_code`, `rank_no`),
  KEY `idx_plaza_headline_item_tpl` (`plaza_template_id`, `version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='头条榜单条目（按范围分组）';
