-- studio 付费成品异步转存素材库（studio library copy outbox）
--
-- 背景：studio 付费档子任务出图后，原先要先把成品从网关私有桶复制到素材库公有桶（媒体服务 resources/copy：取源 + 上传 + probe），
-- 复制完才进交付中、结算、置成功、入素材库。转存是记账性质的操作，不该挡在交付路径上（与 Go_V1.0.29 画布同一思路）。
--
-- 现在：付费成品以私有资源引用 mr://{rid}.{ext}（长期有效，读时现签，从不落库签名地址）进交付中，**同一事务**落一行本表；
-- 结算、置成功后用户立即可见；后台复制到素材库公有桶后，先入素材库（afv_asset 只写公有地址，绝不写 mr://），
-- 再按旧值 CAS 把子任务的 result_url 换成公有地址。
--   * 每个子任务的每张产物至多一行（uk_task_src：重新生成后产物 rid 变了，是新的一行）；
--     复制幂等键 studio-copy:{src_rid}，重试 / 多实例并发得到同一个素材库 rid。
--   * status：0-待转存 1-已完成 2-永久失败（成品仍以 mr:// 可看可下载，接口 library_status=failed，不在素材库）
--     3-作废（子任务已不是成功态 / 已删除 / 成品已换成别的，例如交付失败、重新生成、运营重做）。
--   * 认领：attempt CAS（WHERE id=? AND status=0 AND attempt=?）+ next_at 推到认领超时之后；进程崩溃后到点由任一实例的扫描接手。
--   * 退避：失败按 Library.BackoffSeconds 逐次加长，超过 Library.MaxAttempts 记永久失败。
--
-- 只新增一张表，不改现有表结构；可重复执行。上线顺序无要求：表不存在时 studio-rpc 落 outbox 失败会自动退回同步转存（旧行为）。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_studio_copy_job` (
  `id`          bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `task_pk`     bigint        NOT NULL COMMENT 'afv_studio_task.id',
  `task_id`     varchar(64)   NOT NULL COMMENT '子任务对外任务号（素材库 source_task_id）',
  `user_id`     bigint        NOT NULL COMMENT '所属用户ID',
  `src_rid`     varchar(128)  NOT NULL COMMENT '待转存的私有产物 rid（网关产物）',
  `ref`         varchar(255)  NOT NULL COMMENT '进交付中时写进子任务 result_url 的引用 mr://{src_rid}.{ext}（换地址按它 CAS）',
  `status`      tinyint       NOT NULL DEFAULT '0' COMMENT '0-待转存 1-已完成 2-永久失败 3-作废',
  `attempt`     int           NOT NULL DEFAULT '0' COMMENT '已认领（尝试）次数，认领时 CAS +1',
  `next_at`     datetime(3)   NOT NULL COMMENT '下次可被扫描认领的时间（认领后推到认领超时之后；失败后按退避）',
  `last_error`  varchar(255)  DEFAULT NULL COMMENT '最近一次失败原因（排障用，不对用户展示）',
  `created_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间（= 子任务进交付中的时间）',
  `updated_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  `finished_at` datetime(3)   DEFAULT NULL COMMENT '完成 / 永久失败 / 作废的时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_task_src` (`task_pk`,`src_rid`) USING BTREE,
  KEY `idx_status_next` (`status`,`next_at`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='studio 付费成品转存素材库 outbox';
