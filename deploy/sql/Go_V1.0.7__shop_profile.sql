-- 店铺档案：内容生产的数据底座（AI文案、图文笔记、探店视频、带货短剧都可关联）
-- 一个账号可建多条（上限由服务配置，当前 20 条）。只新建一张表。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_shop_profile` (
  `id`              bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`         bigint       NOT NULL COMMENT '所属用户ID',
  `name`            varchar(64)  NOT NULL COMMENT '店铺名称（必填，上限 20 字符，汉字算 2）',
  `open_date`       varchar(32)  DEFAULT NULL COMMENT '开店时间：前端日期选择器选中后按文本存，如 2025-10-03',
  `address`         varchar(255) DEFAULT NULL COMMENT '店铺地址：省市区+街道门牌号（上限 60 字符）',
  `products`        varchar(512) DEFAULT NULL COMMENT '提供商品/服务（上限 180 字符）',
  `selling_points`  varchar(768) DEFAULT NULL COMMENT '核心卖点，3~5 条（上限 220 字符）',
  `self_title`      varchar(64)  DEFAULT NULL COMMENT '如何称呼自己，如「老板娘小林」（上限 15 字符），第一人称口播用',
  `target_audience` varchar(128) DEFAULT NULL COMMENT '主要目标客群（上限 40 字符）',
  `deleted`         tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  KEY `idx_user_time` (`user_id`,`deleted`,`id` DESC) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='店铺档案';
