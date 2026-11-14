-- 后台模板素材上传的废弃对象清理：
--   1. afv_plaza_upload_orphan：confirm 拒收 / 去重命中产生的孤儿对象待清理队列（Go 独占，只在 admin-api 读写）
--   2. afv_plaza_upload_hash 补列 last_hit_time：去重命中时刷新，清理按 GREATEST(create_time, last_hit_time) 判宽限期，
--      避免「编辑器刚拿到旧对象的 URL、还没保存就被清掉」
-- 清理本身由配置 Plaza.Upload.Cleanup.Enabled 控制（默认关闭）；本迁移只建表 / 补列，不删任何对象。可重复执行。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_plaza_upload_orphan` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `object_key` VARCHAR(512) NOT NULL COMMENT '桶内对象 key（plaza-template/ 下）',
    `url` VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '对象公有地址（仅供排查）',
    `reason` VARCHAR(32) NOT NULL COMMENT 'REJECTED（confirm 拒收）/ DUPLICATE（内容去重命中）',
    `attempts` INT NOT NULL DEFAULT 0 COMMENT '已尝试删除次数',
    `last_error` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '最近一次删除失败原因',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '入队时间（宽限期起点）',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_upload_orphan_key` (`object_key`(255)),
    KEY `idx_plaza_upload_orphan_ct` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模板素材上传孤儿对象待清理队列';

DROP PROCEDURE IF EXISTS `go_v1048_upload_hash_hit`;
DELIMITER $$
CREATE PROCEDURE `go_v1048_upload_hash_hit`()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_plaza_upload_hash' AND COLUMN_NAME = 'last_hit_time') THEN
        ALTER TABLE `afv_plaza_upload_hash`
          ADD COLUMN `last_hit_time` DATETIME NULL COMMENT '最近一次去重命中时间（清理宽限期取 GREATEST(create_time, last_hit_time)）';
    END IF;
END$$
DELIMITER ;
CALL `go_v1048_upload_hash_hit`();
DROP PROCEDURE IF EXISTS `go_v1048_upload_hash_hit`;
