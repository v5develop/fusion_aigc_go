-- Go 侧计费（pkg/billing）读写的 Java 表，当前列的最小 DDL，仅供 sqlc 解析，不会被执行。
-- 来源：Java 仓库 fusion-aigc/src/main/resources/db/migration/
--   V1.0.1__init_schema.sql   afv_billing_reservation / afv_points_account / afv_points_log / afv_points_log_line / afv_billing_reservation_line 的初始定义
--   V1.0.185__billing_reservation_attempt_no.sql   afv_billing_reservation 加 attempt_no，唯一键变为 uk_biz(biz_type,biz_id,attempt_no)
--   V1.0.155__billing_reservation_line_billed_units.sql   afv_billing_reservation_line 加 billed_units
--   V1.0.166__billing_reservation_line_status_width.sql   afv_billing_reservation_line.status 放宽到 varchar(32)
-- 只保留审计查询可能用到的列；Java 侧新增列时按需补，不必全量同步。

CREATE TABLE `afv_billing_reservation` (
  `id`              bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `reservation_no`  varchar(64)  NOT NULL COMMENT '扣费单号（BR + 32 hex）',
  `user_id`         bigint       NOT NULL COMMENT '用户ID',
  `feature_code`    varchar(64)  NOT NULL COMMENT '创作能力编码',
  `model_id`        bigint       NOT NULL COMMENT '模型ID',
  `pricing_id`      bigint       DEFAULT NULL COMMENT '定价规则ID',
  `price_points`    bigint       NOT NULL COMMENT '价格快照点数',
  `price_snapshot`  json         DEFAULT NULL COMMENT '完整价格快照',
  `biz_type`        varchar(32)  NOT NULL COMMENT '业务类型；Go 侧恒为 LOCALLIFE',
  `biz_id`          varchar(64)  NOT NULL COMMENT '业务ID：{userId}_{requestId}，超 64 字节折叠成 SHA-256（pkg/billing bizID）',
  `attempt_no`      int          NOT NULL DEFAULT '0' COMMENT 'V1.0.185：同一 biz 的第几次预留；Go 侧恒为 0',
  `status`          varchar(32)  NOT NULL COMMENT 'HELD/CONFIRMED/PARTIALLY_REFUNDED/REFUNDED/CANCELLED/EXPIRED',
  `retry_count`     int          NOT NULL DEFAULT '0' COMMENT '结算重试次数',
  `idempotency_key` varchar(128) NOT NULL COMMENT '幂等键',
  `expire_time`     datetime     DEFAULT NULL COMMENT '预扣过期时间',
  `confirmed_time`  datetime     DEFAULT NULL COMMENT '确认时间',
  `refunded_time`   datetime     DEFAULT NULL COMMENT '退款时间',
  `deleted`         tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_reservation_no` (`reservation_no`),
  UNIQUE KEY `uk_biz` (`biz_type`,`biz_id`,`attempt_no`),
  UNIQUE KEY `uk_idempotency_key` (`idempotency_key`),
  KEY `idx_user_status` (`user_id`,`status`),
  KEY `idx_status_expire` (`status`,`expire_time`,`id`)
);

CREATE TABLE `afv_billing_reservation_line` (
  `id`               bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `reservation_no`   varchar(64)  NOT NULL COMMENT '预扣单号',
  `line_no`          int          NOT NULL COMMENT '行号',
  `component_code`   varchar(64)  DEFAULT NULL COMMENT '收费项编码',
  `step_code`        varchar(64)  DEFAULT NULL COMMENT '步骤编码',
  `mode`             int          DEFAULT NULL COMMENT 'AIGC模式',
  `billed_units`     int          DEFAULT NULL COMMENT 'V1.0.155：该行计费份数',
  `reserved_points`  bigint       NOT NULL DEFAULT '0' COMMENT '预扣点数',
  `confirmed_points` bigint       NOT NULL DEFAULT '0' COMMENT '已确认点数',
  `refunded_points`  bigint       NOT NULL DEFAULT '0' COMMENT '已退款点数',
  `status`           varchar(32)  NOT NULL DEFAULT 'HELD' COMMENT 'V1.0.166：HELD/CONFIRMED/REFUNDED/PARTIALLY_REFUNDED/CANCELLED',
  `deleted`          tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除',
  `create_time`      datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`      datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`),
  KEY `idx_reservation_no` (`reservation_no`)
);

CREATE TABLE `afv_points_account` (
  `id`              bigint   NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`         bigint   NOT NULL COMMENT '用户ID',
  `balance`         bigint   NOT NULL DEFAULT '0' COMMENT '当前可用余额',
  `frozen_balance`  bigint   NOT NULL DEFAULT '0' COMMENT '当前冻结余额',
  `total_received`  bigint   NOT NULL DEFAULT '0' COMMENT '累计获得点数',
  `total_consumed`  bigint   NOT NULL DEFAULT '0' COMMENT '累计确认消费点数',
  `total_refunded`  bigint   NOT NULL DEFAULT '0' COMMENT '累计退款/补偿点数',
  `total_expired`   bigint   NOT NULL DEFAULT '0' COMMENT '累计过期点数',
  `version`         bigint   NOT NULL DEFAULT '0' COMMENT '乐观锁版本',
  `deleted`         tinyint  NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`     datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`     datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_id` (`user_id`)
);

CREATE TABLE `afv_points_log` (
  `id`              bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `ledger_no`       varchar(64)  NOT NULL COMMENT '流水号',
  `user_id`         bigint       NOT NULL COMMENT '用户ID',
  `change_type`     varchar(32)  NOT NULL COMMENT '变更类型',
  `amount`          bigint       NOT NULL COMMENT '变更点数，正数收入，负数支出',
  `balance_after`   bigint       NOT NULL COMMENT '变更后可用余额',
  `frozen_after`    bigint       NOT NULL DEFAULT '0' COMMENT '变更后冻结余额',
  `biz_type`        varchar(32)  DEFAULT NULL COMMENT '业务类型',
  `biz_id`          varchar(64)  DEFAULT NULL COMMENT '业务ID',
  `idempotency_key` varchar(128) NOT NULL COMMENT '幂等键',
  `description`     varchar(512) DEFAULT NULL COMMENT '描述',
  `deleted`         tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_ledger_no` (`ledger_no`),
  UNIQUE KEY `uk_idempotency_key` (`idempotency_key`),
  KEY `idx_user_time` (`user_id`,`create_time`),
  KEY `idx_biz` (`biz_type`,`biz_id`)
);

CREATE TABLE `afv_points_log_line` (
  `id`               bigint   NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `ledger_id`        bigint   NOT NULL COMMENT '流水ID',
  `batch_id`         bigint   NOT NULL COMMENT '点数批次ID',
  `user_id`          bigint   NOT NULL COMMENT '用户ID',
  `amount`           bigint   NOT NULL COMMENT '本批次变化点数',
  `before_remaining` bigint   NOT NULL COMMENT '变更前批次可用余额',
  `after_remaining`  bigint   NOT NULL COMMENT '变更后批次可用余额',
  `before_frozen`    bigint   NOT NULL DEFAULT '0' COMMENT '变更前冻结余额',
  `after_frozen`     bigint   NOT NULL DEFAULT '0' COMMENT '变更后冻结余额',
  `deleted`          tinyint  NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`      datetime NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  PRIMARY KEY (`id`),
  KEY `idx_ledger` (`ledger_id`),
  KEY `idx_batch` (`batch_id`)
);
