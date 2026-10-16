-- 数字人口播（执行面 92 / 91）：复用 afv_image_creation，新增背景图与时长两列。
-- 背景预设复用模板素材表（localLife/voiceover/background），运营在现有后台维护。
SET NAMES utf8mb4;

ALTER TABLE `afv_image_creation`
  ADD COLUMN `input_bg_url` varchar(1024) DEFAULT NULL COMMENT '输入背景图（数字人口播：预设背景快照或用户上传；空=AI智能匹配）' AFTER `input_video_url`,
  ADD COLUMN `duration` int NOT NULL DEFAULT '0' COMMENT '视频时长秒数（数字人口播 15 / 30；其余类型为 0）' AFTER `size`;

ALTER TABLE `afv_image_creation`
  MODIFY COLUMN `kind` varchar(16) NOT NULL COMMENT '类型：poster-海报(78) longImage-长图(76) actionTransfer-动作迁移(9/86) voiceover-数字人口播(92/91)',
  MODIFY COLUMN `content` text COMMENT '海报信息 / 长图内容 / 口播文案（动作迁移不用）',
  MODIFY COLUMN `template_asset_id` bigint DEFAULT NULL COMMENT '所选模板（afv_public_template_assets.id）；数字人口播为所选预设背景；空=AI智能匹配',
  MODIFY COLUMN `size` varchar(16) NOT NULL DEFAULT '' COMMENT '比例：海报 9:16 等；长图默认 1:4；数字人口播 9:16 / 16:9',
  MODIFY COLUMN `model` varchar(64) NOT NULL DEFAULT '' COMMENT '模型档位：海报 max；长图不传；动作迁移 doubao-seedance-2-0-260128 或 lite；数字人口播 seedance2.5 或 lite';

-- 素材库聚合分类：数字人口播（成品是视频）
INSERT INTO `afv_asset_category` (`category_code`,`category_name`,`parent_id`,`level`,`media_type`,`sort_order`,`status`)
SELECT 'voiceover','数字人口播',(SELECT c.id FROM afv_asset_category c WHERE c.category_code='video' AND c.level=1 AND c.deleted=0 LIMIT 1),2,'',45,1
WHERE NOT EXISTS (SELECT 1 FROM `afv_asset_category` c WHERE c.`category_code`='voiceover' AND c.`deleted`=0);

INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, 'voiceover', 10 FROM `afv_asset_category` c
WHERE c.`category_code`='voiceover' AND c.`deleted`=0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id`=c.`id` AND e.`entry_code`='voiceover');
