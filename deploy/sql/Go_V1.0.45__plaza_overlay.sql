-- ============================================================
-- 模板文字叠加记录（设计：fusion-aigc/.scratch/agent-template-industry/DESIGN.md 17.3，P1）
--
-- 表所有权：afv_plaza_overlay 是 Go 新建、Go 独写，Java 不读不写。
-- 一行 = 「某用户的一张无字成品图 + 某模板 + 用户填的文字」的叠字结果；改字 / 重试总是从无字原图重渲染（非破坏性）。
--
-- 编号：Go_V1.0.45（deploy/sql 已提交最高 Go_V1.0.44；设计稿写的 37 号早已被 replicate 占用）。
-- 幂等：CREATE TABLE IF NOT EXISTS，可重复执行。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_plaza_overlay` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `user_id` BIGINT NOT NULL COMMENT '用户 id',
    `template_id` BIGINT NOT NULL COMMENT '广场模板 id（afv_compat_plaza_template.id）',
    `source_url` VARCHAR(1024) NOT NULL COMMENT '无字原成品图地址（用户自己的成品）',
    `source_hash` CHAR(64) NOT NULL COMMENT 'source_url 的 sha256（小写 hex），唯一键用',
    `values_json` JSON NOT NULL COMMENT '文字位取值 {slotKey: 文字}',
    `plain_rid` VARCHAR(64) NULL COMMENT '无字图在媒体服务里的 rid（摄入后记下，重渲染复用）',
    `rendered_rid` VARCHAR(64) NULL COMMENT '叠字图的媒体 rid（中间产物）',
    `rendered_url` VARCHAR(1024) NULL COMMENT '叠字图公有地址（素材库桶，持久）',
    `asset_synced` TINYINT NOT NULL DEFAULT 0 COMMENT '叠字版是否已登记进素材库：0=待登记（登记失败 / 素材行还没落库，之后 GET 会补登记）1=已登记',
    `status` TINYINT NOT NULL DEFAULT 2 COMMENT '1=叠字成功 2=叠字失败（可重试）',
    `fail_reason` VARCHAR(200) NULL COMMENT '失败原因（给运营排查；不含供应商原文）',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_overlay_src` (`user_id`, `template_id`, `source_hash`),
    KEY `idx_plaza_overlay_user_src` (`user_id`, `source_hash`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模板文字叠加记录（叠字不收费）';
