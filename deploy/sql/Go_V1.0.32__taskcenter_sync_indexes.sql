-- 任务中心新同步路径的分页索引：各源表增加 (更新时间列, id)。
--
-- 用途：pkg/taskcenter 新同步（TASKCENTER_SYNC_MODE=new）按 (update_time, id) 水位分页读源表
--   WHERE (upd > ? OR (upd = ? AND id > ?)) ORDER BY upd, id LIMIT 500
-- 现有索引多为 (status, update_time)，无法按 (更新时间, id) 有序扫描，只能 filesort；afv_copywriting 连更新时间索引都没有。
-- 更新时间列：studio / canvas 用 updated_at，其余用 update_time（见各服务 taskcenter.Source.UpdateTimeCol）。
--
-- 安全：只加二级索引，ALGORITHM=INPLACE, LOCK=NONE 在线建，不阻塞读写。
-- 幂等：每个索引先查 information_schema，存在则跳过；表不存在（某环境未部署该服务）也跳过。
-- 上线顺序：建议在把任何实例切到 TASKCENTER_SYNC_MODE=new 之前执行；对 legacy 模式无影响。
SET NAMES utf8mb4;

DROP PROCEDURE IF EXISTS `go_v1032_add_sync_index`;
DELIMITER $$
CREATE PROCEDURE `go_v1032_add_sync_index`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_index VARCHAR(64))
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col)
       AND NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND INDEX_NAME = p_index) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD KEY `', p_index, '` (`', p_col, '`, `id`), ALGORITHM=INPLACE, LOCK=NONE');
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
DELIMITER ;

CALL `go_v1032_add_sync_index`('afv_studio_task',         'updated_at',  'idx_sync_updated_id');
CALL `go_v1032_add_sync_index`('afv_canvas_node_run',     'updated_at',  'idx_sync_updated_id');
CALL `go_v1032_add_sync_index`('afv_copywriting',         'update_time', 'idx_sync_update_id');
CALL `go_v1032_add_sync_index`('afv_image_text_note',     'update_time', 'idx_sync_update_id');
CALL `go_v1032_add_sync_index`('afv_digital_human',       'update_time', 'idx_sync_update_id');
CALL `go_v1032_add_sync_index`('afv_local_life_creation', 'update_time', 'idx_sync_update_id');
CALL `go_v1032_add_sync_index`('afv_video_creation',      'update_time', 'idx_sync_update_id');

DROP PROCEDURE IF EXISTS `go_v1032_add_sync_index`;
