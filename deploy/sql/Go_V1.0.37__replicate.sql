-- 视频做同款（video-replicate）第一切片：解析 / 方案 / 出片任务 / 步骤 / 分段预留镜像 / 账户偏好
--
-- 设计：fusion-aigc/.scratch/video-replicate-agent/DESIGN.md §5.3–§5.14、§9。
-- 编号：Go_V1.0.37（全分支已提交最高 Go_V1.0.32；33–36 行业 / 模板 / 头条 / 后台权限，38 画布智能体）；
--       上线前仍须在各目标库 SELECT 已执行版本核对（migrate.sh 按文件名版本记录）。
--
-- 只新增 Go 自有表（afv_replicate_*），不改任何现有表；可重复执行（建表 IF NOT EXISTS；后补的列 / 索引用存储过程查
-- information_schema，已存在则跳过，兼容跑过早期版本本文件的临时库）。排序规则统一 utf8mb4_unicode_ci（任务中心同步联表避免 1267）。
-- 钱：每段一张预留单（pkg/billing，biz_type=LOCALLIFE，request_id 见 afv_replicate_charge.request_id），本库只记镜像，
--     真实状态以 afv_billing_reservation 为准；对账查询见 db/audit/query/replicate_reconcile.sql（sqlc，只读）。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_replicate_analysis` (
  `id`           bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `analysis_no`  varchar(32)   NOT NULL COMMENT '对外解析号 VRA_ + 随机十六进制',
  `user_id`      bigint        NOT NULL COMMENT '所属用户ID',
  `source_key`   char(64)      NOT NULL COMMENT '源地址去 query 后的 SHA-256（同用户同视频只分析一次）',
  `source_url`   varchar(1024) NOT NULL COMMENT '用户提交的源视频地址（本站上传地址或 mr:// 引用）',
  `source_rid`   varchar(128)  DEFAULT NULL COMMENT '媒体服务 rid（ingest 后回填）',
  `status`       tinyint       NOT NULL DEFAULT '0' COMMENT '0-排队 1-进行中 2-成功 3-失败',
  `stage`        varchar(16)   NOT NULL DEFAULT 'ingest' COMMENT '当前阶段：ingest/probe/frames/sheet/transcribe/vision/done',
  `attempt`      int           NOT NULL DEFAULT '0' COMMENT '第几轮分析（失败后重新分析 +1，参与网关幂等键）',
  `state`        json          DEFAULT NULL COMMENT '阶段化中间结果：probe/frames/sheet/transcript/网关调用号/告警',
  `result`       json          DEFAULT NULL COMMENT '规范化后的分析结果（十一维/分镜/主体/画面文字/台词/一句话概述）',
  `error_code`   varchar(64)   DEFAULT NULL COMMENT '失败判据码（内部）',
  `error_msg`    varchar(255)  DEFAULT NULL COMMENT '失败文案（用户口径）',
  `lease_owner`  varchar(64)   DEFAULT NULL COMMENT '租约持有者（pkg/lease）',
  `lease_until`  datetime(3)   DEFAULT NULL COMMENT '租约到期（库时钟）',
  `next_run_at`  datetime(3)   DEFAULT NULL COMMENT '轮询器下次推进时间（库时钟；NULL=立即；每轮推进后按退避写回，不动 updated_at）',
  `created_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  `finished_at`  datetime(3)   DEFAULT NULL COMMENT '终态时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_analysis_no` (`analysis_no`),
  UNIQUE KEY `uk_user_source` (`user_id`, `source_key`),
  KEY `idx_status_lease` (`status`, `lease_until`),
  KEY `idx_status_next_run` (`status`, `next_run_at`),
  KEY `idx_updated` (`updated_at`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：视频分析（单飞）';

CREATE TABLE IF NOT EXISTS `afv_replicate_plan` (
  `id`           bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `plan_no`      varchar(32)   NOT NULL COMMENT '对外方案号 VRP_ + 随机十六进制',
  `user_id`      bigint        NOT NULL COMMENT '所属用户ID',
  `analysis_id`  bigint        NOT NULL COMMENT 'afv_replicate_analysis.id',
  `status`       varchar(16)   NOT NULL DEFAULT 'READY' COMMENT 'READY 可确认 / CONFIRMED 已确认（过期不落库：按 expires_at 判断，视图派生 EXPIRED）',
  `model_group`  varchar(64)   NOT NULL COMMENT '出片模型组（ai-gateway 模型组码）',
  `resolution`   varchar(16)   NOT NULL DEFAULT '720p' COMMENT '分辨率',
  `aspect_ratio` varchar(16)   NOT NULL DEFAULT '' COMMENT '画幅，空=跟随原片',
  `annotations`  json          NOT NULL COMMENT '用户改动（各项 不改/换成我的图/按我写的改、备注、参考图序号）与声音策略',
  `refs`         json          DEFAULT NULL COMMENT '参考图 [{index,rid}]（index 从 1 起）',
  `segments`     json          NOT NULL COMMENT '分段 [{index,t0,t1,genT0,genT1,genDuration,overlapIn,cutReason}]',
  `prompts`      json          NOT NULL COMMENT '每段提示词（冻结进出片任务）',
  `quote`        json          DEFAULT NULL COMMENT '方案生成时的报价快照 {lines,first,rest,total}（确认时重算）',
  `plan_hash`    char(64)      NOT NULL COMMENT '方案指纹：确认 / 继续必须带同一个值',
  `job_id`       bigint        DEFAULT NULL COMMENT '确认后的出片任务 afv_replicate_job.id',
  `expires_at`   datetime(3)   NOT NULL COMMENT '方案过期时间（默认 7 天；过期后同输入重新生成会刷新它与 quote）',
  `created_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_plan_no` (`plan_no`),
  UNIQUE KEY `uk_user_hash` (`user_id`, `plan_hash`),
  KEY `idx_analysis` (`analysis_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：方案卡（同输入同方案）';

CREATE TABLE IF NOT EXISTS `afv_replicate_job` (
  `id`                  bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`             bigint        NOT NULL COMMENT '所属用户ID',
  `workspace_id`        bigint        NOT NULL DEFAULT '0' COMMENT '工作空间ID；0=个人空间',
  `request_id`          varchar(64)   NOT NULL COMMENT '确认接口的 Idempotency-Key（同用户唯一）',
  `plan_id`             bigint        NOT NULL COMMENT 'afv_replicate_plan.id',
  `plan_no`             varchar(32)   NOT NULL COMMENT '方案号（冗余，诊断用）',
  `plan_hash`           char(64)      NOT NULL COMMENT '确认时的方案指纹；继续其余段必须一致',
  `task_id`             varchar(64)   DEFAULT NULL COMMENT '对外任务号 taskcenter.Ref(rpl,id)；网关幂等键前缀与 X-Trace-Id',
  `status`              tinyint       NOT NULL DEFAULT '6' COMMENT '0-排队 1-执行中 2-成功 3-失败 4-已取消 5-部分成功 6-提交中（逐段预留） 7-待继续（第 1 段已好） 8-继续中（逐段预留其余段）',
  `staged`              tinyint       NOT NULL DEFAULT '0' COMMENT '1=先做第 1 段（有 gate/continue 步骤）',
  `input`               json          NOT NULL COMMENT '冻结输入（模型/分辨率/段/提示词/参考），之后不再读方案表',
  `seg_total`           int           NOT NULL DEFAULT '0' COMMENT '段数',
  `seg_done`            int           NOT NULL DEFAULT '0' COMMENT '已成功段数（终态时汇总）',
  `seg_failed`          int           NOT NULL DEFAULT '0' COMMENT '最终失败段数（终态时汇总）',
  `redo_count`          int           NOT NULL DEFAULT '0' COMMENT '用户单段重做次数（全 job）',
  `continue_attempt`    int           NOT NULL DEFAULT '0' COMMENT '第几次「继续」（参与其余段预留请求号，避免复用已退回的预留单）',
  `continue_request_id` varchar(64)   DEFAULT NULL COMMENT '成功的「继续」所用 Idempotency-Key（重复点击返回同一结果）',
  `continue_deadline`   datetime(3)   DEFAULT NULL COMMENT '待继续的截止时间（默认第 1 段完成后 24 小时）；到期以仅第 1 段收尾',
  `stop_reason`         varchar(32)   DEFAULT NULL COMMENT '只交付第 1 段的原因：user_stop / continue_expired',
  `result_document`     json          DEFAULT NULL COMMENT '结果文档（各段状态与产物、失败与退点）',
  `error_reason`        varchar(255)  DEFAULT NULL COMMENT 'job 级失败原因（用户口径）',
  `lease_owner`         varchar(64)   DEFAULT NULL COMMENT '租约持有者（pkg/lease）',
  `lease_until`         datetime(3)   DEFAULT NULL COMMENT '租约到期（库时钟）',
  `next_run_at`         datetime(3)   DEFAULT NULL COMMENT '轮询器下次推进时间（库时钟；NULL=立即；每轮推进后按退避写回，不动 updated_at）',
  `created_at`          datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`          datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  `finished_at`         datetime(3)   DEFAULT NULL COMMENT '终态时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_request` (`user_id`, `request_id`),
  UNIQUE KEY `uk_task_id` (`task_id`),
  KEY `idx_plan` (`plan_id`),
  KEY `idx_status_lease` (`status`, `lease_until`),
  KEY `idx_status_next_run` (`status`, `next_run_at`),
  KEY `idx_status_deadline` (`status`, `continue_deadline`),
  KEY `idx_status_finished` (`status`, `finished_at`),
  KEY `idx_user_created` (`user_id`, `created_at`),
  KEY `idx_updated` (`updated_at`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：出片任务（确认后）';

CREATE TABLE IF NOT EXISTS `afv_replicate_step` (
  `id`                 bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `job_id`             bigint        NOT NULL COMMENT 'afv_replicate_job.id',
  `step_key`           varchar(64)   NOT NULL COMMENT '步骤键：seg/01/generate、seg/01/generate#r1、#r2…（第 n 次自动重试）、seg/01/generate#u1（用户重做）、gate/continue',
  `kind`               varchar(16)   NOT NULL COMMENT 'gateway / gate',
  `seg_index`          int           NOT NULL DEFAULT '0' COMMENT '段号（从 1 起）；gate 为 0',
  `status`             varchar(16)   NOT NULL COMMENT 'HOLDING 等预留 / PENDING / RUNNING / DELIVERING 已出片待确认扣费 / SUCCEEDED / FAILED / CANCELLED（error_code=HOLD_FAILED/HOLD_ABANDONED 为重做预留没成功，不算该段结果） / WAITING（gate）',
  `idem_key`           varchar(255)  NOT NULL COMMENT '[环境前缀]rpl-{job_id}:{step_key}：网关 Idempotency-Key（前缀确认时冻结进 job.input）',
  `billing_request_id` varchar(128)  DEFAULT NULL COMMENT '本步骤用的预留单请求号（自动重试与原步骤共用一张）',
  `attempt`            int           NOT NULL DEFAULT '0' COMMENT '抢占次数（CAS PENDING→RUNNING 时 +1）',
  `gateway_call_id`    varchar(64)   DEFAULT NULL COMMENT '网关调用号（提交后立即落库，恢复时先按它查）',
  `gateway_status`     varchar(32)   DEFAULT NULL COMMENT '网关调用状态原文',
  `output`             json          DEFAULT NULL COMMENT '产物 {videoRid,coverRid,lastFrameRid,durationMs,costCny,cutRid}',
  `error_class`        varchar(32)   DEFAULT NULL COMMENT '失败分类（内部）',
  `error_code`         varchar(64)   DEFAULT NULL COMMENT '失败判据码（内部）',
  `error_msg`          varchar(255)  DEFAULT NULL COMMENT '失败文案（用户口径，单一失败表）',
  `retry_of`           varchar(64)   DEFAULT NULL COMMENT '自动重试步骤指向原步骤键',
  `started_at`         datetime(3)   DEFAULT NULL COMMENT '开始时间',
  `finished_at`        datetime(3)   DEFAULT NULL COMMENT '终态时间',
  `lease_owner`        varchar(64)   DEFAULT NULL COMMENT '最近一次推进它的实例（诊断用；接管时改写；状态迁移只按 id+status CAS，不带 owner）',
  `created_at`         datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`         datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_job_step` (`job_id`, `step_key`),
  UNIQUE KEY `uk_idem` (`idem_key`),
  KEY `idx_billing_request` (`billing_request_id`),
  KEY `idx_status` (`status`, `updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：出片步骤（静态 DAG + 重试/重做行）';

CREATE TABLE IF NOT EXISTS `afv_replicate_charge` (
  `id`            bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `job_id`        bigint        NOT NULL COMMENT 'afv_replicate_job.id',
  `user_id`       bigint        NOT NULL COMMENT '所属用户ID（与 request_id 组成预留单 biz_id）',
  `seg_index`     int           NOT NULL COMMENT '段号（从 1 起）',
  `stage`         varchar(8)    NOT NULL COMMENT 'first / all / rest / redo',
  `request_id`    varchar(128)  NOT NULL COMMENT '预留单请求号 billing.Charge.RequestID：replicate-<job>-<kk>、-c<n>（继续）、-u<n>（重做）；从不复用',
  `client_key`    varchar(64)   DEFAULT NULL COMMENT '用户重做的 Idempotency-Key（重复点击返回同一笔）',
  `seconds`       int           NOT NULL COMMENT '计费秒数（生成时长）',
  `points`        bigint        NOT NULL DEFAULT '0' COMMENT '预留时的报价点数',
  `status`        tinyint       NOT NULL DEFAULT '0' COMMENT '0-待预留 1-已预留 2-已扣除 3-已退回 4-不计费（off/shadow/0 点） 5-预留失败 6-结算待重试（reason 前缀 confirm: 扣除待重试 / refund: 退回待重试，引擎补扫重做）',
  `reason`        varchar(255)  DEFAULT NULL COMMENT '最近一次结算/失败原因（内部）',
  `created_at`    datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`    datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  `settled_at`    datetime(3)   DEFAULT NULL COMMENT '扣除 / 退回时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_request` (`request_id`),
  UNIQUE KEY `uk_job_client` (`job_id`, `client_key`),
  KEY `idx_job` (`job_id`, `seg_index`),
  KEY `idx_status` (`status`, `updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：分段预留单镜像（真值在 afv_billing_reservation）';

CREATE TABLE IF NOT EXISTS `afv_replicate_user_pref` (
  `user_id`       bigint      NOT NULL COMMENT '用户ID',
  `auto_continue` tinyint     NOT NULL DEFAULT '0' COMMENT '1=以后直接全部做（不再先看第 1 段）',
  `created_at`    datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`    datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='视频做同款：账户级偏好';

-- ---------------------------------------------------------------- 后补列 / 索引（判存在，可重复执行）
DROP PROCEDURE IF EXISTS `go_v1037_add_column`;
DROP PROCEDURE IF EXISTS `go_v1037_add_index`;
DELIMITER $$
CREATE PROCEDURE `go_v1037_add_column`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_def TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD COLUMN `', p_col, '` ', p_def);
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
CREATE PROCEDURE `go_v1037_add_index`(IN p_table VARCHAR(64), IN p_index VARCHAR(64), IN p_cols TEXT)
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

CALL `go_v1037_add_column`('afv_replicate_analysis', 'next_run_at',
  'datetime(3) DEFAULT NULL COMMENT ''轮询器下次推进时间（库时钟；NULL=立即；每轮推进后按退避写回，不动 updated_at）'' AFTER `lease_until`');
CALL `go_v1037_add_column`('afv_replicate_job', 'next_run_at',
  'datetime(3) DEFAULT NULL COMMENT ''轮询器下次推进时间（库时钟；NULL=立即；每轮推进后按退避写回，不动 updated_at）'' AFTER `lease_until`');
CALL `go_v1037_add_index`('afv_replicate_analysis', 'idx_status_next_run', '`status`, `next_run_at`');
CALL `go_v1037_add_index`('afv_replicate_job', 'idx_status_next_run', '`status`, `next_run_at`');
CALL `go_v1037_add_index`('afv_replicate_job', 'idx_status_deadline', '`status`, `continue_deadline`');
CALL `go_v1037_add_index`('afv_replicate_job', 'idx_status_finished', '`status`, `finished_at`');

DROP PROCEDURE IF EXISTS `go_v1037_add_column`;
DROP PROCEDURE IF EXISTS `go_v1037_add_index`;
