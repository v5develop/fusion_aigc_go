-- ============================================================
-- 模板文字叠加：服务端自动叠字（不再依赖用户页面开着）
--
-- 表所有权：两张表都是 Go 新建、Go 独写（gateway-api），Java 不读不写。
--   afv_plaza_overlay_intent：做同款提交时冻结的叠字意图（键 = 用户 + 本轮 X-Request-Id），冻结「是否叠字 + 文字值 + 模板文字位版式」；
--   afv_plaza_overlay_job：每张已入库产物一条叠字作业（行租约认领、退避重试、次数用完记最终失败）。
-- 产物来源：Java 智能体工具产物（afv_creation_task.request_params.source.runId = 'run-' + requestId）入库后的 afv_asset 行。
--
-- 编号：Go_V1.0.49（deploy/sql 最高 Go_V1.0.48）。幂等：CREATE TABLE IF NOT EXISTS，可重复执行。
-- ============================================================
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_plaza_overlay_intent` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `user_id` BIGINT NOT NULL COMMENT '用户 id',
    `request_id` VARCHAR(64) NOT NULL COMMENT '本轮智能体请求的 X-Request-Id（前端生成）',
    `run_id` VARCHAR(80) NOT NULL COMMENT 'Java 轮次 runId = run- + request_id（afv_agent_turn.run_id / 工具产物 request_params.source.runId）',
    `template_id` BIGINT NOT NULL COMMENT '广场模板 id（afv_compat_plaza_template.id）',
    `enabled` TINYINT NOT NULL DEFAULT 1 COMMENT '1=本次给图片加字 0=本次不加 / 已作废（轮次模板对不上）',
    `values_json` JSON NULL COMMENT '冻结的文字位取值 {slotKey: 文字}（enabled=0 时为空）',
    `slots_json` JSON NULL COMMENT '冻结的文字位版式 [{key,layout,maxLength}]（提交后模板改版 / 下架不影响本轮）',
    `scan_count` INT NOT NULL DEFAULT 0 COMMENT '已扫描次数',
    `next_scan_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '下次扫描产物的时间（认领时条件更新推后）',
    `expire_at` DATETIME NOT NULL COMMENT '过了这个时间不再找它的产物（Plaza.Overlay.Auto.MatchWindowMinutes）',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_overlay_intent_req` (`user_id`, `request_id`),
    KEY `idx_plaza_overlay_intent_scan` (`enabled`, `next_scan_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模板自动叠字意图（提交时冻结）';

CREATE TABLE IF NOT EXISTS `afv_plaza_overlay_job` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `user_id` BIGINT NOT NULL COMMENT '用户 id',
    `intent_id` BIGINT NOT NULL COMMENT '叠字意图 id（afv_plaza_overlay_intent.id）',
    `template_id` BIGINT NOT NULL COMMENT '广场模板 id',
    `asset_id` BIGINT NOT NULL COMMENT '无字版素材行 id（afv_asset.id，仅供排查）',
    `source_url` VARCHAR(1024) NOT NULL COMMENT '无字成品图地址（素材行上用户看到的那张：有水印图用水印图）',
    `source_hash` CHAR(64) NOT NULL COMMENT 'source_url 的 sha256，与 afv_plaza_overlay 同口径',
    `state` TINYINT NOT NULL DEFAULT 0 COMMENT '0=待处理（含退避中） 1=完成 2=最终失败（已落 failed 叠加记录，可手动重试）',
    `attempts` INT NOT NULL DEFAULT 0 COMMENT '已尝试次数（认领时 +1）',
    `next_at` DATETIME NULL COMMENT '下次可重试时间（退避）',
    `lease_until` DATETIME NULL COMMENT '租约截止',
    `lease_token` VARCHAR(32) NULL COMMENT '租约令牌（写回围栏）',
    `last_error` VARCHAR(200) NULL COMMENT '最近一次失败原因（不含供应商原文）',
    `overlay_id` BIGINT NULL COMMENT '关联的叠加记录 afv_plaza_overlay.id',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_overlay_job_src` (`user_id`, `template_id`, `source_hash`),
    KEY `idx_plaza_overlay_job_pick` (`state`, `next_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模板自动叠字作业（叠字不收费）';
