-- ============================================================
-- 视频做同款：任务来源 + 待决策提醒（CONTRACT §O2）
--   afv_replicate_job.origin_json        不透明来源 {kind, conversation_id, turn_id}：只有智能体入口确认的任务才有，
--                                        只用来拼通知链接（回到原对话），不参与任何业务判断
--   afv_replicate_job.decision_remind_at 待决策提醒时刻 = continue_deadline - 提前量：第 1 段做好进入「待继续」时写入，
--                                        提醒发出（或任务离开待继续）后置空；轮询器据此在到点时唤醒该任务。
--                                        提醒本身的幂等靠通知唯一键 replicate:<job>:replicate.decision_reminder，不靠这一列
--
-- 编号：Go_V1.0.56（所有分支 deploy/sql 最高 Go_V1.0.52；本工作区未提交的 Go_V1.0.53~55 已占前三号）。上线前在各目标库核对已执行版本。
-- 只给 Go 自有表加列；可重复执行。
-- ============================================================
SET NAMES utf8mb4;

DROP PROCEDURE IF EXISTS `go_v1056_add_column`;
DROP PROCEDURE IF EXISTS `go_v1056_add_index`;
DELIMITER $$
CREATE PROCEDURE `go_v1056_add_column`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_def TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD COLUMN `', p_col, '` ', p_def);
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
CREATE PROCEDURE `go_v1056_add_index`(IN p_table VARCHAR(64), IN p_index VARCHAR(64), IN p_cols TEXT)
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

CALL `go_v1056_add_column`('afv_replicate_job', 'origin_json',
  'json DEFAULT NULL COMMENT ''不透明来源 {kind,conversation_id,turn_id}：仅智能体入口确认的任务有；只用于拼通知链接，不参与业务判断''');
CALL `go_v1056_add_column`('afv_replicate_job', 'decision_remind_at',
  'datetime(3) DEFAULT NULL COMMENT ''待决策提醒时刻（continue_deadline - 提前量）：进入待继续时写入，提醒发出或离开待继续后置空''');
CALL `go_v1056_add_index`('afv_replicate_job', 'idx_status_remind', '`status`, `decision_remind_at`');

DROP PROCEDURE IF EXISTS `go_v1056_add_column`;
DROP PROCEDURE IF EXISTS `go_v1056_add_index`;
