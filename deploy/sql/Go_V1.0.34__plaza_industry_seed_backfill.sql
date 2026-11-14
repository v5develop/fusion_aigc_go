-- ============================================================
-- 智能体模板页：种子行业 / 渠道与存量回填（DDL 在 Go_V1.0.33）。可重复执行（ON DUPLICATE KEY / INSERT IGNORE / 条件 UPDATE）。
-- 设计：DESIGN.md 4.3、16.2。
-- 回填只做确定性的两件事：封面后缀是视频 → media_type=video；标签与一级行业名完全相等 → 挂该行业。
-- 其余（含足浴二级）一律留空，由运营在后台「未分配行业」筛选批量归类。
-- ============================================================

-- 1) 种子行业：全部是普通行业行，没有任何特殊语义（没有虚拟的「全部」；「其他」只是一个运营精选的真实行业，可改可删）
INSERT INTO `afv_template_industry` (`code`, `name`, `parent_id`, `sort_order`, `enabled`, `show_when_empty`, `hint`, `audience_name`) VALUES
 ('spa',        'SPA 养生', NULL, 10,  1, 0, '养生馆 / SPA 会所',        '养生人'),
 ('foot_bath',  '足浴',     NULL, 20,  1, 0, '按摩 / 泡脚门店',          '足浴人'),
 ('apparel',    '服装',     NULL, 30,  1, 0, '服装店 / 女装男装',        '服装人'),
 ('food',       '美食',     NULL, 40,  1, 0, '餐厅 / 小吃 / 饮品',       '餐饮人'),
 ('fitness',    '健身',     NULL, 50,  1, 0, '健身房 / 瑜伽 / 私教',     '健身人'),
 ('home_decor', '家居装修', NULL, 60,  1, 0, '家具 / 装修 / 软装',       '家居人'),
 ('other',      '其他',     NULL, 999, 1, 0, '暂时没有你的行业？看看这里', '商家')
ON DUPLICATE KEY UPDATE `code` = `code`;

-- 子级用 INSERT IGNORE（uk_template_industry_code 兜住重复）：INSERT..SELECT 自表时 ON DUPLICATE KEY 的列名会二义
INSERT IGNORE INTO `afv_template_industry` (`code`, `name`, `parent_id`, `sort_order`, `enabled`, `show_when_empty`, `hint`)
SELECT 'foot_bath_bluev', '蓝V号', `id`, 10, 1, 0, '蓝V认证账号（企业认证的商家号，也就是门店官方号）'
  FROM `afv_template_industry` WHERE `code` = 'foot_bath';

-- 子级用 INSERT IGNORE（uk_template_industry_code 兜住重复）：INSERT..SELECT 自表时 ON DUPLICATE KEY 的列名会二义
INSERT IGNORE INTO `afv_template_industry` (`code`, `name`, `parent_id`, `sort_order`, `enabled`, `show_when_empty`, `hint`)
SELECT 'foot_bath_creator', '达人号', `id`, 20, 1, 0, '达人 / 探店账号（个人探店、种草的达人号）'
  FROM `afv_template_industry` WHERE `code` = 'foot_bath';

-- 2) 渠道字典（像素值为默认，运营核对后在后台修订）
INSERT INTO `afv_plaza_channel` (`code`, `name`, `media_type`, `width`, `height`, `ratio`, `note`, `sort_order`, `enabled`) VALUES
 ('meituan_head',  '美团头图',   'image', 1200, 800,  '3:2',  '默认值，待运营按平台规范核对', 10, 1),
 ('dianping',      '大众点评',   'image', 1080, 1080, '1:1',  '默认值，待运营按平台规范核对', 20, 1),
 ('douyin_9_16',   '抖音 9:16',  'image', 1080, 1920, '9:16', '默认值，待运营按平台规范核对', 30, 1),
 ('xhs_3_4',       '小红书 3:4', 'image', 1080, 1440, '3:4',  '默认值，待运营按平台规范核对', 40, 1),
 ('taobao_1_1',    '淘宝主图',   'image', 800,  800,  '1:1',  '默认值，待运营按平台规范核对', 50, 1)
ON DUPLICATE KEY UPDATE `code` = `code`;

-- 3) media_type：封面（data_json.result[0]，缺则 image_urls[0]）后缀是视频 → video，其余保持默认 image
UPDATE `afv_compat_plaza_template`
   SET `media_type` = 'video'
 WHERE `deleted` = 0
   AND `media_type` = 'image'
   AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(`data_json`, '$.result[0]')),
                JSON_UNQUOTE(JSON_EXTRACT(`data_json`, '$.image_urls[0]')), '')
       REGEXP '\\.(mp4|webm|mov|m4v)(\\?|#|$)';

UPDATE `afv_compat_plaza_template`
   SET `video_url` = COALESCE(JSON_UNQUOTE(JSON_EXTRACT(`data_json`, '$.result[0]')),
                              JSON_UNQUOTE(JSON_EXTRACT(`data_json`, '$.image_urls[0]')))
 WHERE `media_type` = 'video' AND `video_url` IS NULL AND `deleted` = 0;

-- 4) 行业：只做「标签与一级行业名完全相等」的确定性回填；二级（蓝V号/达人号）不回填
INSERT IGNORE INTO `afv_plaza_template_industry` (`template_id`, `industry_id`)
SELECT t.`id`, i.`id`
  FROM `afv_compat_plaza_template` t
  JOIN `afv_template_industry` i ON i.`parent_id` IS NULL AND i.`code` <> 'other' AND i.`deleted` = 0
 WHERE t.`deleted` = 0
   AND JSON_CONTAINS(t.`tags_json`, JSON_QUOTE(i.`name`), '$.tags');
