-- ============================================================
-- 广场模板域「用户的库」：收藏、我的模板、营销节点（设计：fusion-aigc/.scratch/agent-template-industry
-- DESIGN.md 16.5 / 附章 A、EXPERIENCE.md 3.9 / 7 节）
--
-- 表所有权（一个表一个写者）：以下四张表全是 Go 新建、Go 独写，Java 不读不写。
--   afv_plaza_favorite        用户收藏（user_id + template_id 唯一；只存关系，不存行业——行业按模板当前归属在读取时过滤）
--   afv_plaza_my_template     用户的「我的模板」（一次做同款的设置：模板 id、入参值、版数、文字；industry_id 记创建时所选行业）
--   afv_plaza_node            营销节点（年货节、七夕、换季……），运营维护；本迁移只种子，后台写接口未做
--   afv_plaza_user_lock       用户维度锁行（上限检查串行化）
--   afv_plaza_node_template   节点关联的模板（行业归属沿用模板自己的行业关系，不另存）
--
-- 编号：Go_V1.0.43（deploy/sql 已提交最高 Go_V1.0.42；已确认全仓库无 43 号文件）。
-- 幂等：全部 CREATE TABLE IF NOT EXISTS；种子用「按 code 判存在」的 INSERT ... SELECT，可重复执行，不需要在 migrate.sh 的 guard_done 里登记。
-- 种子日期是 2026-11 ~ 2027-11 一个年度的节点；节点过期后不再出现，下一年度由运营补（后台写接口未做前，用 SQL 补）。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_plaza_favorite` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `user_id` BIGINT NOT NULL COMMENT '用户 id',
    `template_id` BIGINT NOT NULL COMMENT '广场模板 id（afv_compat_plaza_template.id）；模板下线不删记录',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '收藏时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_favorite_user_tpl` (`user_id`, `template_id`),
    KEY `idx_plaza_favorite_user_id` (`user_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='广场模板收藏（每人上限 500，由接口保证）';

CREATE TABLE IF NOT EXISTS `afv_plaza_my_template` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `user_id` BIGINT NOT NULL COMMENT '用户 id',
    `name` VARCHAR(60) NOT NULL COMMENT '名称（用户可改，接口限 ≤40 字）',
    `template_id` BIGINT NOT NULL COMMENT '广场模板 id',
    `industry_id` BIGINT NOT NULL DEFAULT 0 COMMENT '创建时所选行业（afv_template_industry.id）；0 = 未知',
    `inputs_json` JSON NULL COMMENT '做同款入参值（按保存时模板 slots schema 规整后的 key→值）',
    `variants` INT NOT NULL DEFAULT 1 COMMENT '版数',
    `resolution` VARCHAR(8) NOT NULL DEFAULT '1k' COMMENT '出图档位 1k / 2k / 4k',
    `text` VARCHAR(500) NOT NULL DEFAULT '' COMMENT '用户填的文字（一句话需求）',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    KEY `idx_plaza_my_tpl_user` (`user_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='用户的「我的模板」（私有，每人上限 100，由接口保证）';

CREATE TABLE IF NOT EXISTS `afv_plaza_node` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `code` VARCHAR(64) NOT NULL COMMENT '节点代码（稳定，前端据此记「已关闭」；一个节点每年一行时带年份）',
    `name` VARCHAR(40) NOT NULL COMMENT '节点名，如 年货节',
    `why` VARCHAR(60) NOT NULL DEFAULT '' COMMENT '一句推荐语（可空；仅内容推荐，不含任何优惠 / 折扣 / 赠送）',
    `event_date` DATE NOT NULL COMMENT '节点当天',
    `lead_days` INT NOT NULL DEFAULT 14 COMMENT '节点前多少天起展示（含节点当天），EXPERIENCE 7 节为 14',
    `sort_order` INT NOT NULL DEFAULT 0 COMMENT '同日节点的先后，升序',
    `enabled` TINYINT NOT NULL DEFAULT 1 COMMENT '1 启用 / 0 停用',
    `deleted` TINYINT NOT NULL DEFAULT 0 COMMENT '逻辑删除',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_node_code` (`code`),
    KEY `idx_plaza_node_window` (`enabled`, `deleted`, `event_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='营销节点（节点内容推荐条）';

CREATE TABLE IF NOT EXISTS `afv_plaza_node_template` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `node_id` BIGINT NOT NULL COMMENT '节点 id（afv_plaza_node.id）',
    `template_id` BIGINT NOT NULL COMMENT '广场模板 id；行业归属看模板自己的行业关系',
    `sort_order` INT NOT NULL DEFAULT 0 COMMENT '节点内先后，升序',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_node_tpl` (`node_id`, `template_id`),
    KEY `idx_plaza_node_tpl_tpl` (`template_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='营销节点关联的模板';

-- 用户维度的串行化锁行：收藏 / 我的模板的「数量上限」检查在事务里先 SELECT … FOR UPDATE 锁这一行，再计数、插入
-- （INSERT … SELECT COUNT(*) 在 RR 下并发会死锁、RC 下会超限）。行由接口 INSERT IGNORE 按需创建，不需要种子。
CREATE TABLE IF NOT EXISTS `afv_plaza_user_lock` (
    `user_id` BIGINT NOT NULL COMMENT '用户 id',
    PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='广场用户维度串行化锁行（收藏 / 我的模板上限）';

-- 种子：按 code 判存在，已有（含运营改过的）不覆盖
INSERT INTO `afv_plaza_node` (`code`, `name`, `why`, `event_date`, `lead_days`, `sort_order`)
SELECT s.code, s.name, s.why, s.event_date, s.lead_days, s.sort_order FROM (
    SELECT 'double12_2026' AS code, '双十二' AS name, '年终活动图该上新了' AS why, DATE '2026-12-12' AS event_date, 14 AS lead_days, 10 AS sort_order
    UNION ALL SELECT 'new_year_2027', '元旦', '新年第一波图，提前备好', DATE '2027-01-01', 14, 10
    UNION ALL SELECT 'nianhuo_2027', '年货节', '年货节海报和短视频，现在就能做', DATE '2027-01-20', 14, 10
    UNION ALL SELECT 'spring_festival_2027', '春节', '拜年、营业时间通知，一键出图', DATE '2027-02-06', 14, 10
    UNION ALL SELECT 'valentine_2027', '情人节', '情人节双人套餐图正当时', DATE '2027-02-14', 14, 10
    UNION ALL SELECT 'spring_season_2027', '春季换季上新', '换季上新，新品图一起换', DATE '2027-03-01', 14, 20
    UNION ALL SELECT 'womens_day_2027', '三八妇女节', '女神节活动图提前备好', DATE '2027-03-08', 14, 10
    UNION ALL SELECT 'day520_2027', '520', '520 表白套餐图', DATE '2027-05-20', 14, 10
    UNION ALL SELECT 'mid_year_618_2027', '618', '年中大促活动图', DATE '2027-06-18', 14, 10
    UNION ALL SELECT 'qixi_2027', '七夕', '七夕前，团购图该上新了', DATE '2027-08-08', 14, 10
    UNION ALL SELECT 'mid_autumn_2027', '中秋', '中秋节日氛围图', DATE '2027-09-15', 14, 10
    UNION ALL SELECT 'national_day_2027', '国庆', '国庆假期出行与到店图', DATE '2027-10-01', 14, 10
    UNION ALL SELECT 'double11_2027', '双十一', '双十一大促图提前备好', DATE '2027-11-11', 14, 10
) s
WHERE NOT EXISTS (SELECT 1 FROM `afv_plaza_node` n WHERE n.`code` = s.`code`);

-- 我的模板记住清晰度：本迁移未提交，建表已含该列；已按旧版建过表的库在这里补列（判存在，可重复执行）
DROP PROCEDURE IF EXISTS `go_v1043_add_resolution`;
DELIMITER $$
CREATE PROCEDURE `go_v1043_add_resolution`()
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'afv_plaza_my_template')
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'afv_plaza_my_template' AND COLUMN_NAME = 'resolution') THEN
        ALTER TABLE `afv_plaza_my_template` ADD COLUMN `resolution` VARCHAR(8) NOT NULL DEFAULT '1k' COMMENT '出图档位 1k / 2k / 4k' AFTER `variants`;
    END IF;
END$$
DELIMITER ;
CALL `go_v1043_add_resolution`();
DROP PROCEDURE IF EXISTS `go_v1043_add_resolution`;
