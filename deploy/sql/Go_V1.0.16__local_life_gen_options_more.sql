-- ============================================================
-- 本地生活生成配置（续）：海报风格、长图比例、视频时长，以及默认比例 / 模板必选。
--
-- 与 Go_V1.0.15 同一规则：只能改启用 / 名称 / 排序 / 默认，不能新增编码。
-- 例外：海报风格的名称会直接进入出图提示词，后台不允许改名（服务端校验），改名要改代码。
-- 只增不改：全部用 IF NOT EXISTS / NOT EXISTS 守卫，重复执行无副作用。
-- ============================================================

-- 模板必选：海报、长图、动作迁移（与 YAML 原值一致）。没有行的类型回落 YAML。
CREATE TABLE IF NOT EXISTS `afv_local_life_gen_kind` (
  `kind_code`         varchar(32) NOT NULL COMMENT '类型编码：poster / longImage / actionTransfer',
  `template_required` tinyint     NOT NULL DEFAULT '0' COMMENT '是否必须选模板：0-否 1-是',
  `update_time`       datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`kind_code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='本地生活生成类型的运营开关';

INSERT INTO `afv_local_life_gen_kind` (`kind_code`, `template_required`)
SELECT t.k, t.r FROM (
  SELECT 'poster' AS k, 1 AS r
  UNION ALL SELECT 'longImage', 1
  UNION ALL SELECT 'actionTransfer', 0
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_local_life_gen_kind` g WHERE g.`kind_code` = t.k);

-- 选项种子：海报风格（与代码列表一致，'' = AI 自动匹配）、长图比例、视频时长
INSERT INTO `afv_local_life_gen_option` (`option_group`, `code`, `label`, `sort_order`, `enabled`, `is_default`)
SELECT t.g, t.c, t.l, t.s, 1, t.d FROM (
  SELECT 'poster_styles' AS g, '' AS c, 'AI自动匹配' AS l, 10 AS s, 1 AS d
  UNION ALL SELECT 'poster_styles', 'portrait', '人像摄影', 20, 0
  UNION ALL SELECT 'poster_styles', 'film', '电影', 30, 0
  UNION ALL SELECT 'poster_styles', 'comic', '漫画', 40, 0
  UNION ALL SELECT 'poster_styles', 'flat', '扁平插画', 50, 0
  UNION ALL SELECT 'poster_styles', 'pencil', '彩铅', 60, 0
  UNION ALL SELECT 'poster_styles', 'cyberpunk', '赛博朋克', 70, 0
  UNION ALL SELECT 'poster_styles', 'cg3d', '3D CG', 80, 0
  UNION ALL SELECT 'poster_styles', 'clay', '橡皮泥', 90, 0
  UNION ALL SELECT 'poster_styles', 'watercolor', '水彩', 100, 0
  UNION ALL SELECT 'poster_styles', 'ghibli', '吉卜力', 110, 0
  UNION ALL SELECT 'poster_styles', 'chinese', '中国风', 120, 0
  UNION ALL SELECT 'poster_styles', 'oil', '油画', 130, 0
  UNION ALL SELECT 'poster_styles', 'ink', '水墨画', 140, 0
  UNION ALL SELECT 'poster_styles', 'sketch', '简笔画', 150, 0
  UNION ALL SELECT 'longimage_sizes', '1:3', '1:3', 10, 1
  UNION ALL SELECT 'video_durations', '15', '15 秒', 10, 1
  UNION ALL SELECT 'video_durations', '30', '30 秒', 20, 0
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_local_life_gen_option` o WHERE o.option_group = t.g AND o.code = t.c);

-- 默认比例：与 YAML 原值一致（海报 2:3、口播 9:16、视频 9:16）。组内已有默认则不动。
UPDATE `afv_local_life_gen_option` SET `is_default` = 1
WHERE `option_group` = 'poster_sizes' AND `code` = '2:3'
  AND NOT EXISTS (SELECT 1 FROM (SELECT 1 FROM `afv_local_life_gen_option` x WHERE x.`option_group` = 'poster_sizes' AND x.`is_default` = 1) d);
UPDATE `afv_local_life_gen_option` SET `is_default` = 1
WHERE `option_group` = 'voiceover_sizes' AND `code` = '9:16'
  AND NOT EXISTS (SELECT 1 FROM (SELECT 1 FROM `afv_local_life_gen_option` x WHERE x.`option_group` = 'voiceover_sizes' AND x.`is_default` = 1) d);
UPDATE `afv_local_life_gen_option` SET `is_default` = 1
WHERE `option_group` = 'video_sizes' AND `code` = '9:16'
  AND NOT EXISTS (SELECT 1 FROM (SELECT 1 FROM `afv_local_life_gen_option` x WHERE x.`option_group` = 'video_sizes' AND x.`is_default` = 1) d);
