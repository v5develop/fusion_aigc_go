-- 图文笔记关联店铺档案（设计稿表单里的「关联店铺档案（选填）」）：构思时拼进 74 的 shop_profile
SET NAMES utf8mb4;

ALTER TABLE `afv_image_text_note`
  ADD COLUMN `shop_profile_id` bigint DEFAULT NULL COMMENT '关联店铺档案ID（afv_shop_profile.id），选填' AFTER `person_url`;
