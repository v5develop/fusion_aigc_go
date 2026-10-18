-- 运营强制失败：失败记录置 admin_locked=1 后，运营重做一律拒绝（终态）。
-- 点数在失败时已经退回，强制失败本身不动账；只封住重做。
SET NAMES utf8mb4;

ALTER TABLE `afv_local_life_creation`
  ADD COLUMN `admin_locked` TINYINT NOT NULL DEFAULT 0 COMMENT '运营强制失败：1=封禁运营重做' AFTER `status`;

ALTER TABLE `afv_video_creation`
  ADD COLUMN `admin_locked` TINYINT NOT NULL DEFAULT 0 COMMENT '运营强制失败：1=封禁运营重做' AFTER `status`;

ALTER TABLE `afv_image_text_note`
  ADD COLUMN `admin_locked` TINYINT NOT NULL DEFAULT 0 COMMENT '运营强制失败：1=封禁运营重做' AFTER `status`;

ALTER TABLE `afv_digital_human`
  ADD COLUMN `admin_locked` TINYINT NOT NULL DEFAULT 0 COMMENT '运营强制失败：1=封禁运营重做' AFTER `status`;
