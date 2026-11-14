-- 模板素材上传去重：confirm 时按对象内容 sha256 登记，同内容再次上传直接复用已登记的 URL。
-- Go 独占表；只在 admin-api 的 /plaza/admin/uploads/confirm 里读写。可重复执行。
CREATE TABLE IF NOT EXISTS `afv_plaza_upload_hash` (
    `id` BIGINT NOT NULL AUTO_INCREMENT COMMENT '主键',
    `sha256` CHAR(64) NOT NULL COMMENT '对象内容 sha256（小写 hex）',
    `object_key` VARCHAR(512) NOT NULL COMMENT '首次登记时的桶内对象 key',
    `url` VARCHAR(1024) NOT NULL COMMENT '首次登记时的公有地址（同内容再次上传时返回它）',
    `result_json` JSON NOT NULL COMMENT '首次 confirm 的完整结果（ConfirmResult：宽高 / 时长 / 封面 / 状态）',
    `size` BIGINT NOT NULL DEFAULT 0 COMMENT '字节数',
    `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '登记时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_plaza_upload_sha` (`sha256`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='模板素材上传内容去重登记';
