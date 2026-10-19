-- 执行面提交快照：每次提交执行面（成功、失败、未配置）写一行，按执行面任务号 upsert。
-- 任务中心的 execution_params 从这里取，各服务统一口径：记的就是实际发出去的入参。
CREATE TABLE IF NOT EXISTS afv_exec_submission (
    exec_task_id     VARCHAR(128) NOT NULL COMMENT '执行面任务号',
    task_type        INT          NOT NULL COMMENT '提交的类型，如动作迁移 9',
    routed_task_type INT          NULL     COMMENT '执行面路由后的类型，如 9 → 86；未受理为空',
    input            JSON         NULL     COMMENT '实际提交的入参（不含 task_id / task_type）',
    error_msg        VARCHAR(512) NULL     COMMENT '未受理的原因；已受理为空',
    created_at       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (exec_task_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COMMENT = '执行面提交快照';
