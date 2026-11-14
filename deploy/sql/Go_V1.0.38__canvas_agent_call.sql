-- 画布智能体工具调用：审计 + 幂等（canvas agent tools，Function Call / MCP 共用）
--
-- 设计见 docs/design/canvas-agent-tools.md §5 / §7。
--   * 每次写 / 生成类工具调用一行（只读调用只打日志）；记录谁（user_id + client + agent + client_ip）、对哪张画布、
--     用什么参数（args_hash + args）、改前改后的 revision、触发的生成（run_request_id）、结果与耗时。
--   * 幂等：带 call_id 的调用先插一行 status=0、idem_key=call_id 占位（uk_user_idem），执行完收尾为 1/2；
--     同 key 同参数已成功 → 回放 result；失败时 idem_key 置 NULL 释放，可用同 call_id 重试；占位超过 2 分钟视为崩溃残留可接管。
--     每个占位带 lease_token：执行前复核占位仍属于自己，收尾按 (id, lease_token, status=0) CAS——被接管后旧执行者既不执行也写不进。
--   * args / result 超过 64KB 不落（置 NULL，args_hash 仍在）。
--
-- 版本号：Go_V1.0.38（全分支已提交最高 Go_V1.0.32，33–37 为同批的行业 / 模板 / 头条 / 后台权限 / 做同款）。只新增一张表（另带补列过程，给早期已建表的环境补 lease_token），不改现有表；可重复执行。
-- 上线顺序无要求：表不存在时写类工具会因幂等占位失败而拒绝执行（报 SERVER_ERROR），只读工具不受影响。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_canvas_agent_call` (
  `id`              bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`         bigint        NOT NULL COMMENT '调用者（来自网关鉴权，不来自工具参数）',
  `canvas_id`       varchar(32)   DEFAULT NULL COMMENT '操作的画布（canvas_create 收尾时回填）',
  `tool`            varchar(64)   NOT NULL COMMENT '工具名（canvas_add_nodes …）',
  `client`          varchar(16)   NOT NULL DEFAULT '' COMMENT '接入形式：mcp / function / grpc',
  `agent`           varchar(64)   NOT NULL DEFAULT '' COMMENT '客户端自报名（X-Agent-Name / User-Agent）',
  `call_id`         varchar(64)   DEFAULT NULL COMMENT '调用方给的幂等键（原样留档）',
  `idem_key`        varchar(64)   DEFAULT NULL COMMENT '生效中的幂等键（= call_id；失败后置 NULL 释放）',
  `lease_token`     varchar(32)   NOT NULL DEFAULT '' COMMENT '占位令牌（执行前复核、收尾 CAS）',
  `args_hash`       char(64)      NOT NULL COMMENT '归一后入参的 sha256（幂等比对）',
  `args`            json          DEFAULT NULL COMMENT '归一后入参（≤64KB）',
  `status`          tinyint       NOT NULL DEFAULT '0' COMMENT '0-执行中 1-成功 2-失败',
  `error_code`      varchar(32)   DEFAULT NULL COMMENT '失败码（PARAM_INVALID / CANVAS_CONFLICT / NOT_FOUND …）',
  `error_msg`       varchar(255)  DEFAULT NULL COMMENT '失败原因',
  `revision_before` varchar(32)   DEFAULT NULL COMMENT '写入前 revision',
  `revision_after`  varchar(32)   DEFAULT NULL COMMENT '写入后 revision',
  `run_request_id`  bigint        DEFAULT NULL COMMENT '触发 / 操作的生成（afv_canvas_node_run.id）',
  `result`          json          DEFAULT NULL COMMENT '成功结果（≤64KB，幂等回放用）',
  `duration_ms`     int           NOT NULL DEFAULT '0' COMMENT '执行耗时（毫秒）',
  `client_ip`       varchar(64)   NOT NULL DEFAULT '' COMMENT '调用方 IP',
  `created_at`      datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`      datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_idem` (`user_id`,`idem_key`) USING BTREE,
  KEY `idx_canvas_time` (`canvas_id`,`created_at`) USING BTREE,
  KEY `idx_user_time` (`user_id`,`created_at`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='画布智能体工具调用审计与幂等';

-- 早期本地已建表的环境补 lease_token 列（判存在，可重复执行）
DROP PROCEDURE IF EXISTS `go_v1038_add_agent_lease_token`;
DELIMITER $$
CREATE PROCEDURE `go_v1038_add_agent_lease_token`()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_canvas_agent_call' AND COLUMN_NAME = 'lease_token') THEN
        SET @ddl := CONCAT('ALTER TABLE `afv_canvas_agent_call` ADD COLUMN `lease_token` varchar(32) NOT NULL DEFAULT ''''',
                           ' COMMENT ''占位令牌（执行前复核、收尾 CAS）'' AFTER `idem_key`');
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
DELIMITER ;

CALL `go_v1038_add_agent_lease_token`();
DROP PROCEDURE IF EXISTS `go_v1038_add_agent_lease_token`;
