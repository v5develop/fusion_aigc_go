-- 画布 MCP 访问令牌：外部 MCP 客户端（Claude Desktop / Cursor …）专用的、可吊销的长期令牌
--
-- 设计见 docs/design/canvas-agent-tools.md §13。
--   * 明文（mcp_ + 43 位 base64url）只在创建时返回一次；库里只存 sha256(明文)（uk_token_hash）与明文前 12 位（token_prefix，列表里便于识别）。
--   * scope：canvas_rw（画布读写）/ canvas_rw_generate（画布读写 + 生成）；范围外的工具网关回 is_error FORBIDDEN_SCOPE。
--   * 有效期 7 / 30 / 90 天（expires_at）；每人最多 10 个生效中（未吊销、未过期）的令牌（创建时在事务里加锁计数）。
--   * 吊销 = revoked_at 置时间（不物理删，留审计）；网关校验结果缓存 ≤60 秒，吊销时网关同步删缓存，即时生效。
--   * last_used_at / last_used_ip：网关缓存未命中时（约每分钟一次）由校验回写。
--
-- 版本号：Go_V1.0.42（全分支已提交最高 Go_V1.0.40；41 留给同批其他改动）。只新增一张表，不改现有表；可重复执行。
-- 上线顺序无要求：表不存在时创建 / 校验 MCP 令牌报错（登录令牌访问 MCP 不受影响）。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_canvas_mcp_token` (
  `id`           bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`      bigint       NOT NULL COMMENT '令牌所有者',
  `name`         varchar(64)  NOT NULL DEFAULT '' COMMENT '用户起的名字（≤50 字）',
  `token_prefix` varchar(16)  NOT NULL COMMENT '明文前 12 位（mcp_xxxxxxxx），便于识别',
  `token_hash`   char(64)     NOT NULL COMMENT 'sha256(明文) 小写 hex；明文不落库',
  `scope`        varchar(32)  NOT NULL COMMENT 'canvas_rw / canvas_rw_generate',
  `expires_at`   datetime(3)  NOT NULL COMMENT '过期时间',
  `last_used_at` datetime(3)  DEFAULT NULL COMMENT '最后使用时间',
  `last_used_ip` varchar(64)  NOT NULL DEFAULT '' COMMENT '最后使用 IP',
  `created_ip`   varchar(64)  NOT NULL DEFAULT '' COMMENT '创建时 IP',
  `revoked_at`   datetime(3)  DEFAULT NULL COMMENT '吊销时间（NULL = 未吊销）',
  `created_at`   datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`   datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_token_hash` (`token_hash`) USING BTREE,
  KEY `idx_user_revoked` (`user_id`,`revoked_at`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='画布 MCP 访问令牌';
