-- 素材库：为本地生活的三类成品新增聚合分类与入口归属
--
-- 只插配置数据，不改任何表结构。涉及的是现有系统的两张配置表：
--   afv_asset_category        素材库两级分类（图片/视频 → 子类）
--   afv_asset_category_entry  子类包含哪些创作入口（entry_code）
-- 运营也可在现有后台维护（权限 asset_category:*）。
--
-- entry_code 与计费 Offer 的 entry_code 同名（见 afv_creation_task.entry_code 注释），
-- 计费方案确认后若需改名，改各服务配置里的 Asset.EntryCode，并同步更新下面两处数据。
SET NAMES utf8mb4;

-- 子类挂在一级分类「图片」(id=1) / 「视频」(id=2) 下
INSERT INTO `afv_asset_category` (`category_code`,`category_name`,`parent_id`,`level`,`media_type`,`sort_order`,`status`)
SELECT * FROM (
  SELECT 'imageTextNote'        AS category_code, '图文笔记'   AS category_name, (SELECT c.id FROM afv_asset_category c WHERE c.category_code='image' AND c.level=1 AND c.deleted=0 LIMIT 1) AS parent_id, 2 AS level, '' AS media_type, 40 AS sort_order, 1 AS status
  UNION ALL SELECT 'digitalHumanPortrait', '数字人形象', (SELECT c.id FROM afv_asset_category c WHERE c.category_code='image' AND c.level=1 AND c.deleted=0 LIMIT 1), 2, '', 50, 1
  UNION ALL SELECT 'digitalHuman',         '数字人',     (SELECT c.id FROM afv_asset_category c WHERE c.category_code='video' AND c.level=1 AND c.deleted=0 LIMIT 1), 2, '', 40, 1
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_asset_category` c WHERE c.`category_code` = t.category_code AND c.`deleted` = 0);

-- 入口归属：一个子类对应一个创作入口
INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, c.`category_code`, 10 FROM `afv_asset_category` c
WHERE c.`category_code` IN ('imageTextNote','digitalHumanPortrait','digitalHuman') AND c.`deleted` = 0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id` = c.`id` AND e.`entry_code` = c.`category_code`);
