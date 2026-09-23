-- 探店视频（72 → 73/81）与带货短剧（87 → 88/89）：两步生成，先出剧本再出片
--
-- 不复用 afv_image_creation：那张表是「一次提交一个任务」，这两个模块要存中间的剧本和两个任务号。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_video_creation` (
  `id`              bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`         bigint        NOT NULL COMMENT '创建用户ID',
  `kind`            varchar(16)   NOT NULL COMMENT '类型：exploreShop-探店视频(72/73) shortDrama-带货短剧(87/88)',
  `user_text`       text          COMMENT '探店：视频文案；短剧：短剧主题',
  `selling_points`  varchar(768)  DEFAULT NULL COMMENT '商品卖点（短剧，≤500 字）',
  `shop_profile_id` bigint        DEFAULT NULL COMMENT '关联店铺档案ID（afv_shop_profile.id），选填',
  `char_url`        varchar(1024) DEFAULT NULL COMMENT '出镜人物图：探店达人 / 短剧角色（进件 id CHAR_01）',
  `env1_url`        varchar(1024) DEFAULT NULL COMMENT '探店：门店图；短剧：环境图（ENV_01）',
  `env2_url`        varchar(1024) DEFAULT NULL COMMENT '探店：店内图（ENV_02）；短剧不用',
  `prop_url`        varchar(1024) DEFAULT NULL COMMENT '探店：道具图；短剧：商品图（PROP_01，短剧必填）',
  `duration`        int           NOT NULL DEFAULT '15' COMMENT '视频时长秒数：15 / 30',
  `size`            varchar(16)   NOT NULL DEFAULT '9:16' COMMENT '视频比例：9:16 / 16:9',
  `model`           varchar(32)   NOT NULL DEFAULT '' COMMENT '模型：seedance2.5(云端 81/89) 或 lite(本地 GPU 73/88)',
  `plan_task_id`    varchar(64)   DEFAULT NULL COMMENT '出剧本的执行面任务号（72 / 87）',
  `plan_script`     json          DEFAULT NULL COMMENT '执行面返回的剧本原文，出片时原样回传',
  `gen_task_id`     varchar(64)   DEFAULT NULL COMMENT '出成片的执行面任务号（73 / 88）',
  `video_url`       varchar(1024) DEFAULT NULL COMMENT '成片地址（转存到公有桶后）',
  `request_id`      varchar(64)   DEFAULT NULL COMMENT '客户端幂等键（X-Request-Id）',
  `status`          int           NOT NULL DEFAULT '0' COMMENT '状态：0-构思中 1-待确认(剧本已出) 2-生成中 3-已完成 4-失败',
  `fail_reason`     varchar(512)  DEFAULT NULL COMMENT '失败原因',
  `deleted`         tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`     datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`     datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_request` (`user_id`,`request_id`) USING BTREE,
  KEY `idx_user_kind_time` (`user_id`,`kind`,`deleted`,`id` DESC) USING BTREE,
  KEY `idx_status_time` (`status`,`update_time`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='探店视频/带货短剧';

-- 素材库聚合分类：探店视频、带货短剧（成品是视频，挂在一级分类「视频」id=2 下）
INSERT INTO `afv_asset_category` (`category_code`,`category_name`,`parent_id`,`level`,`media_type`,`sort_order`,`status`)
SELECT * FROM (
  SELECT 'exploreShop' AS category_code, '探店视频' AS category_name, 2 AS parent_id, 2 AS level, '' AS media_type, 46 AS sort_order, 1 AS status
  UNION ALL SELECT 'shortDrama','带货短剧',2,2,'',47,1
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_asset_category` c WHERE c.`category_code` = t.category_code AND c.`deleted` = 0);

INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, c.`category_code`, 10 FROM `afv_asset_category` c
WHERE c.`category_code` IN ('exploreShop','shortDrama') AND c.`deleted` = 0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id` = c.`id` AND e.`entry_code` = c.`category_code`);
