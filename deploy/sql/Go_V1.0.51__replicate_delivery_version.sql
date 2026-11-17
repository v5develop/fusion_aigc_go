-- ============================================================
-- 视频做同款：入素材库发件箱加版本号
--
-- 已成功的段可「精修」，精修出的新视频要另占一行入素材库，旧版本保留。
-- 原先唯一键是 (job_id, kind, seg_index)，只能靠改写段号区分版本；改为加 version 列并入唯一键，seg_index 始终是真实段号。
--   afv_replicate_delivery.version  int NOT NULL DEFAULT 0  存量行全部为 0（原片 / 重做出的第一个成功）
--   唯一键 uk_job_kind_seg (job_id, kind, seg_index) → uk_job_kind_seg_ver (job_id, kind, seg_index, version)
--
-- 编号：Go_V1.0.51（所有分支 deploy/sql 最高 Go_V1.0.50）。
-- 幂等：每步先查 information_schema 再执行，可重复执行；先加列、再建新唯一键、最后删旧唯一键，
--       任一时刻表上都有唯一约束，存量行 version=0 与旧键等价，不会因加键失败。
-- ============================================================
SET NAMES utf8mb4;

SET @tbl = 'afv_replicate_delivery';

-- 1) 加 version 列
SET @sql = IF((SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = @tbl AND COLUMN_NAME = 'version') = 0,
  'ALTER TABLE `afv_replicate_delivery` ADD COLUMN `version` int NOT NULL DEFAULT 0 COMMENT ''版本号：同一段 / 成片被精修后的新视频另占一行（0 = 原片）'' AFTER `seg_index`',
  'SELECT 1');
PREPARE s FROM @sql; EXECUTE s; DEALLOCATE PREPARE s;

-- 2) 建新唯一键（含 version）
SET @sql = IF((SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = @tbl AND INDEX_NAME = 'uk_job_kind_seg_ver') = 0,
  'ALTER TABLE `afv_replicate_delivery` ADD UNIQUE KEY `uk_job_kind_seg_ver` (`job_id`, `kind`, `seg_index`, `version`)',
  'SELECT 1');
PREPARE s FROM @sql; EXECUTE s; DEALLOCATE PREPARE s;

-- 3) 删旧唯一键
SET @sql = IF((SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = @tbl AND INDEX_NAME = 'uk_job_kind_seg') > 0,
  'ALTER TABLE `afv_replicate_delivery` DROP INDEX `uk_job_kind_seg`',
  'SELECT 1');
PREPARE s FROM @sql; EXECUTE s; DEALLOCATE PREPARE s;
