-- 广场模板：视频处理流水线（PROCESSING / 重试 / 失败原因 / 行租约）+ 渠道字典逻辑删除。
-- 可重复执行：加列包在存储过程里，各按「该列是否已存在」判断；一次性回填只在刚加列时做。
--
-- 1) afv_compat_plaza_template（接管表，只加列 / 放宽列，不改既有语义）
--    media_status 放宽到 VARCHAR(16)（新增状态 PROCESSING，10 个字符）；
--    media_error      最近一次失败原因（给运营看，FAILED 时展示）；
--    media_note       处理结果备注（如「未转码：没有转码能力」）；
--    media_attempts   已尝试次数（任务认领时 +1，3 次仍失败 → FAILED）；
--    media_next_at    下次可重试时间（退避）；
--    media_lease_until / media_lease_token  行租约：多副本靠条件 UPDATE 认领，写回按 token 围栏。
-- 2) afv_plaza_channel 加 deleted / deleted_at（逻辑删除；code 唯一键不变，已删除的 code 不可复用）。
-- 存量：media_status=PENDING 的视频模板（旧语义「封面没抽出来」，无人推进）回填为 PROCESSING，交给新任务处理。
DROP PROCEDURE IF EXISTS `go_v1046_plaza_media_columns`;
DELIMITER $$
CREATE PROCEDURE `go_v1046_plaza_media_columns`()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_compat_plaza_template' AND COLUMN_NAME = 'media_attempts') THEN
        ALTER TABLE `afv_compat_plaza_template`
          MODIFY COLUMN `media_status` VARCHAR(16) NOT NULL DEFAULT 'READY' COMMENT '视频处理状态 PROCESSING|READY|FAILED（PENDING 为旧值，已回填）；视频模板只有 READY 才可发布',
          ADD COLUMN `media_error`       VARCHAR(255) NULL COMMENT '最近一次处理失败原因',
          ADD COLUMN `media_note`        VARCHAR(255) NULL COMMENT '处理结果备注（如未转码的原因）',
          ADD COLUMN `media_attempts`    TINYINT      NOT NULL DEFAULT 0 COMMENT '已尝试次数，认领时 +1，满 3 次失败转 FAILED',
          ADD COLUMN `media_next_at`     DATETIME     NULL COMMENT '下次可重试时间（退避）',
          ADD COLUMN `media_lease_until` DATETIME     NULL COMMENT '行租约到期时间',
          ADD COLUMN `media_lease_token` VARCHAR(32)  NULL COMMENT '行租约持有者 token（写回围栏）',
          ADD KEY `idx_compat_plaza_tpl_mediajob` (`media_status`, `media_next_at`, `id`);
        UPDATE `afv_compat_plaza_template` SET `media_status` = 'PROCESSING'
         WHERE `media_status` = 'PENDING' AND `media_type` = 'video' AND `deleted` = 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_plaza_channel' AND COLUMN_NAME = 'deleted') THEN
        ALTER TABLE `afv_plaza_channel`
          ADD COLUMN `deleted`    TINYINT  NOT NULL DEFAULT 0 COMMENT '逻辑删除',
          ADD COLUMN `deleted_at` DATETIME NULL COMMENT '删除时间';
    END IF;
END$$
DELIMITER ;
CALL `go_v1046_plaza_media_columns`();
DROP PROCEDURE IF EXISTS `go_v1046_plaza_media_columns`;
