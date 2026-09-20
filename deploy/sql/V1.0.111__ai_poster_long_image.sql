-- AI海报 / AI长图：新增 1 张表，模板复用现有模板素材表
--
-- 只新建 afv_image_creation，其余为配置数据（不改表结构）：
--   模板：afv_public_template_assets + afv_public_template_asset_placement
--         海报 biz_type=localLife / module_code=poster    / slot_code=template
--         长图 biz_type=localLife / module_code=longImage / slot_code=template
--   海报类型标签：afv_public_template_filter(posterType) + 选项 + slot 绑定，运营在现有后台维护
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_image_creation` (
  `id`                bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`           bigint        NOT NULL COMMENT '创建用户ID',
  `kind`              varchar(16)   NOT NULL COMMENT '类型：poster-海报（执行面78） longImage-长图（执行面76）',
  `content`           text          NOT NULL COMMENT '海报信息 / 长图内容（用户输入）',
  `material_urls`     json          DEFAULT NULL COMMENT '上传素材 URL 数组（最多 3 张）',
  `template_asset_id` bigint        DEFAULT NULL COMMENT '所选模板（afv_public_template_assets.id）；空=AI智能匹配',
  `template_url`      varchar(1024) DEFAULT NULL COMMENT '模板图地址（下单时快照，模板下架不影响已有记录）',
  `poster_type`       varchar(32)   DEFAULT NULL COMMENT '海报类型 code（模板筛选维度 posterType）',
  `poster_style`      varchar(32)   DEFAULT NULL COMMENT '海报风格 code；空=AI自动匹配',
  `size`              varchar(16)   NOT NULL DEFAULT '' COMMENT '图片比例：海报 9:16 等；长图默认 1:4',
  `model`             varchar(32)   NOT NULL DEFAULT '' COMMENT '出图模型档位：本版海报固定 max；长图不传，用执行面默认',
  `image_count`       int           NOT NULL DEFAULT '1' COMMENT '生成数量，一张图一个执行面任务',
  `tasks`             json          DEFAULT NULL COMMENT '各张图的任务与结果：[{"task_id","status","url","error"}]',
  `image_urls`        json          DEFAULT NULL COMMENT '生成成功的图片地址数组',
  `request_id`        varchar(64)   DEFAULT NULL COMMENT '客户端幂等键（X-Request-Id）',
  `status`            int           NOT NULL DEFAULT '0' COMMENT '状态：0-生成中 1-已完成（含部分成功） 2-失败',
  `fail_reason`       varchar(512)  DEFAULT NULL COMMENT '失败原因',
  `deleted`           tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`       datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`       datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_request` (`user_id`,`request_id`) USING BTREE,
  KEY `idx_user_kind_time` (`user_id`,`kind`,`deleted`,`id` DESC) USING BTREE,
  KEY `idx_status_time` (`status`,`update_time`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='AI海报/长图';

-- 海报类型（模板筛选维度）。长图的模板设计稿没有分类，不加维度。
INSERT INTO `afv_public_template_filter` (`biz_type`,`filter_code`,`filter_name`,`ui_type`,`multi_select`,`sort_order`,`status`)
SELECT 'localLife','posterType','海报类型','tab',0,10,1
WHERE NOT EXISTS (SELECT 1 FROM `afv_public_template_filter` WHERE `biz_type`='localLife' AND `filter_code`='posterType' AND `deleted`=0);

INSERT INTO `afv_public_template_filter_option` (`filter_id`,`option_code`,`option_name`,`sort_order`,`status`)
SELECT f.id, t.code, t.name, t.sort, 1
FROM `afv_public_template_filter` f
JOIN (
  SELECT 'social' code, '社媒' name, 1 sort
  UNION ALL SELECT 'marketing','营销',2
  UNION ALL SELECT 'festival','节日',3
  UNION ALL SELECT 'celebration','喜庆',4
  UNION ALL SELECT 'invitation','邀请函',5
  UNION ALL SELECT 'recruitment','招聘',6
) t
WHERE f.biz_type='localLife' AND f.filter_code='posterType' AND f.deleted=0
  AND NOT EXISTS (SELECT 1 FROM `afv_public_template_filter_option` o WHERE o.filter_id=f.id AND o.option_code=t.code AND o.deleted=0);

-- 海报模板槽位显示「海报类型」这一维度（便签：模板跟随海报类型联动）
INSERT INTO `afv_public_template_slot_filter` (`biz_type`,`module_code`,`slot_code`,`filter_id`,`option_filter_type`,`sort_order`,`status`)
SELECT 'localLife','poster','template', f.id, 'all', 10, 1
FROM `afv_public_template_filter` f
WHERE f.biz_type='localLife' AND f.filter_code='posterType' AND f.deleted=0
  AND NOT EXISTS (SELECT 1 FROM `afv_public_template_slot_filter` s
                  WHERE s.biz_type='localLife' AND s.module_code='poster' AND s.slot_code='template' AND s.filter_id=f.id AND s.deleted=0);

-- 素材库聚合分类：海报、长图
INSERT INTO `afv_asset_category` (`category_code`,`category_name`,`parent_id`,`level`,`media_type`,`sort_order`,`status`)
SELECT * FROM (
  SELECT 'poster' AS category_code, '海报' AS category_name, 1 AS parent_id, 2 AS level, '' AS media_type, 60 AS sort_order, 1 AS status
  UNION ALL SELECT 'longImage','长图',1,2,'',70,1
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_asset_category` c WHERE c.`category_code` = t.category_code AND c.`deleted` = 0);

INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, c.`category_code`, 10 FROM `afv_asset_category` c
WHERE c.`category_code` IN ('poster','longImage') AND c.`deleted` = 0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id` = c.`id` AND e.`entry_code` = c.`category_code`);
