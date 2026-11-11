-- 画布产物异步转存素材库（canvas library copy outbox）
--
-- 背景：画布 run 成功收尾原先要先把每张产物（含视频）从私有桶复制到素材库公有桶（媒体服务 resources/copy：下载 + 上传 + probe），
-- 全部转存完才置成功，用户在这段时间里看不到已经生成好的图 / 视频。转存是记账性质的操作，不该挡在交付路径上。
--
-- 现在：run 先以私有资源引用 mr://{rid}.{ext}（长期有效，读时现签 10 分钟地址，从不落库签名地址）置成功、结算，
-- 同一时刻落一行本表（outbox）；转存在后台完成后把 run 的候选 / 视频地址换成素材库公有地址并入素材库（afv_asset）。
--   * 每个 run 至多一行（uk_run）；幂等键沿用 canvas-copy:{task_id}:{i}（视频 i=99），重试 / 多实例并发得到同一个 rid。
--   * status：0-待转存 1-已完成 2-永久失败（产物仍以 mr:// 可看可下载，run.error_code=LIBRARY_COPY_FAILED） 3-作废（run 不再是成功态 / 已删除）。
--   * 认领：attempt CAS（WHERE id=? AND status=0 AND attempt=?）+ next_at 推到认领超时之后；进程崩溃后到点由任一实例的扫描接手。
--   * 退避：失败按 Library.BackoffSeconds 逐次加长，超过 Library.MaxAttempts 记永久失败。
--
-- 只新增一张表，不改现有表结构；可重复执行。上线顺序无要求：表不存在时 canvas-rpc 落 outbox 失败会自动退回同步转存（旧行为）。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_canvas_copy_job` (
  `id`          bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `run_pk`      bigint        NOT NULL COMMENT 'afv_canvas_node_run.id（对外 request_id）',
  `task_id`     varchar(64)   NOT NULL COMMENT 'run 的对外任务号（幂等键 canvas-copy:{task_id}:{i} 的来源）',
  `user_id`     bigint        NOT NULL COMMENT '所属用户ID',
  `status`      tinyint       NOT NULL DEFAULT '0' COMMENT '0-待转存 1-已完成 2-永久失败 3-作废',
  `attempt`     int           NOT NULL DEFAULT '0' COMMENT '已认领（尝试）次数，认领时 CAS +1',
  `next_at`     datetime(3)   NOT NULL COMMENT '下次可被扫描认领的时间（认领后推到认领超时之后；失败后按退避）',
  `last_error`  varchar(255)  DEFAULT NULL COMMENT '最近一次失败原因（排障用，不对用户展示）',
  `created_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间（= run 置成功的时间）',
  `updated_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  `finished_at` datetime(3)   DEFAULT NULL COMMENT '完成 / 永久失败 / 作废的时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_run` (`run_pk`) USING BTREE,
  KEY `idx_status_next` (`status`,`next_at`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='画布产物转存素材库 outbox';
