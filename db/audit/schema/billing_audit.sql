-- 计费配置检查（pkg/billing/audit.go）读的 Java 表，当前用到的列的最小 DDL，仅供 sqlc 解析，不会被执行。
-- 来源：Java 仓库 fusion-aigc/src/main/resources/db/migration/
--   V1.0.1__init_schema.sql   afv_billing_offer / afv_billing_offer_variant / afv_ai_model / afv_app_user_feature 的初始定义
--   V1.0.159__ai_model_gateway_model_group.sql   afv_ai_model 加 gateway_model_group
-- 只保留审计查询用到的列；Java 侧新增列时按需补。

CREATE TABLE `afv_billing_offer` (
  `id`         bigint      NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `offer_code` varchar(64) NOT NULL COMMENT '售卖产品编码，唯一',
  `entry_code` varchar(64) NOT NULL COMMENT '前端入口编码（type）',
  `status`     varchar(16) NOT NULL DEFAULT 'DRAFT' COMMENT '状态：DRAFT/ENABLED/DISABLED',
  `deleted`    tinyint     NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_offer_code` (`offer_code`),
  KEY `idx_entry_code` (`entry_code`)
);

CREATE TABLE `afv_billing_offer_variant` (
  `id`               bigint      NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `offer_code`       varchar(64) NOT NULL COMMENT '所属offer编码',
  `default_model_id` bigint      DEFAULT NULL COMMENT '默认模型ID',
  `status`           varchar(16) NOT NULL DEFAULT 'DRAFT' COMMENT '状态：DRAFT/ENABLED/DISABLED',
  `deleted`          tinyint     NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  PRIMARY KEY (`id`),
  KEY `idx_offer_code` (`offer_code`)
);

CREATE TABLE `afv_ai_model` (
  `id`                  bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `gateway_model_group` varchar(128) DEFAULT NULL COMMENT 'AI 网关模型组编码',
  `deleted`             tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  PRIMARY KEY (`id`)
);

CREATE TABLE `afv_app_user_feature` (
  `id`           bigint      NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`      bigint      NOT NULL COMMENT '用户ID',
  `feature_code` varchar(64) NOT NULL COMMENT '创作能力编码',
  `status`       varchar(32) NOT NULL DEFAULT 'ACTIVE' COMMENT '状态',
  `expire_time`  datetime    DEFAULT NULL COMMENT '到期时间',
  `deleted`      tinyint     NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  PRIMARY KEY (`id`)
);
