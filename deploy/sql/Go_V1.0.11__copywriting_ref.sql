-- 文案 → 衍生内容：记下「这条内容是用哪段 AI文案 生成的」
--
-- 用来支撑文案详情页的「生成历史 / 已生成的内容」（GET /copywriting/:id/derivatives）。
-- 之前打算靠 afv_creation_task.external_ref 关联，但那张表的 mode 等枚举一直没定，
-- 而且它串到的是素材库 id，前端拿着打不开自己的成品页。所以改成最直接的做法：
-- 在各业务表上记一列 copywriting_id，查的时候 UNION 四张表。
--
-- 选填：用户不是从「选择AI文案」进来的，这列就是 NULL。

ALTER TABLE `afv_image_text_note`
  ADD COLUMN `copywriting_id` bigint DEFAULT NULL COMMENT '用的哪条 AI文案（afv_copywriting.id），选填' AFTER `shop_profile_id`,
  ADD INDEX `idx_copywriting` (`copywriting_id`);

ALTER TABLE `afv_image_creation`
  ADD COLUMN `copywriting_id` bigint DEFAULT NULL COMMENT '用的哪条 AI文案（afv_copywriting.id），选填' AFTER `content`,
  ADD INDEX `idx_copywriting` (`copywriting_id`);

ALTER TABLE `afv_video_creation`
  ADD COLUMN `copywriting_id` bigint DEFAULT NULL COMMENT '用的哪条 AI文案（afv_copywriting.id），选填' AFTER `shop_profile_id`,
  ADD INDEX `idx_copywriting` (`copywriting_id`);

ALTER TABLE `afv_digital_human`
  ADD COLUMN `copywriting_id` bigint DEFAULT NULL COMMENT '用的哪条 AI文案（afv_copywriting.id），选填' AFTER `script`,
  ADD INDEX `idx_copywriting` (`copywriting_id`);
