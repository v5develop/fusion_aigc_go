-- afv_creation_task（Java 任务中心表）审计查询用到的列，仅供 sqlc 解析，不会被执行。
-- 来源：Java 仓库 fusion-aigc/src/main/resources/db/migration/
--   V1.0.1__init_schema.sql                       初始定义（task_id / user_id / status / reservation_no / billing_status / deleted / create_time / update_time）
--   V1.0.53__creation_task_entry_code.sql         entry_code
--   V1.0.189__creation_task_origin_service.sql    origin_service（NULL=Java 自建；LOCAL_LIFE=Go 同步）
--   V1.0.190__creation_task_origin_ref.sql        origin_ref（如 tyo:123）
--   V1.0.111__creation_task_mode_nullable.sql     mode 可空（此处不收录）
-- 其余列（request_params、result_url 等）审计不读，省略。

CREATE TABLE `afv_creation_task` (
  `id`             bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `task_id`        varchar(64)  NOT NULL COMMENT '任务编号；Go 同步行为 taskcenter.Ref(prefix, 源主键) = AiCreation_LF_<md5>',
  `origin_service` varchar(32)  DEFAULT NULL COMMENT 'V1.0.189：来源服务',
  `origin_ref`     varchar(64)  DEFAULT NULL COMMENT 'V1.0.190：来源记录坐标 <prefix>:<主键>',
  `user_id`        bigint       NOT NULL COMMENT '发起用户ID',
  `status`         int          NOT NULL DEFAULT '0' COMMENT '0-已提交 1-处理中 2-已完成 3-失败 4-取消',
  `entry_code`     varchar(64)  NOT NULL DEFAULT '' COMMENT 'V1.0.53：创作入口编码',
  `reservation_no` varchar(64)  DEFAULT NULL COMMENT '扣费单号',
  `billing_status` varchar(32)  DEFAULT NULL COMMENT '计费状态',
  `price_points`   bigint       DEFAULT NULL COMMENT '预扣点数',
  `deleted`        tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`    datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`    datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_task_id` (`task_id`)
);
