-- assetlib：资产库增强（设计方案 §5，前缀 /api/v1/asset-library）
--
-- Java 的 afv_asset / afv_asset_item 一律不 ALTER；Go 建 5 张旁表，按 asset_id 挂接：
--   afv_asset_ext       资产扩展：kind 分类（NULL=未分类）、资产级收藏、授权、metadata、provenance、当前版本
--   afv_asset_version   资产版本（追加，version_no 每资产自增）
--   afv_asset_relation  资产关系（reference / generated_from / version_of）
--   afv_asset_tag       用户标签字典
--   afv_asset_tag_link  资产 ↔ 标签
--
-- 约定：
--   * 全部可重复执行：建表用 IF NOT EXISTS，不改任何现有表。
--   * 排序规则统一 utf8mb4_unicode_ci（混用 0900_ai_ci 与 afv_asset JOIN 会报 1267，见 Go_V1.0.20）。
--   * 时间列 DATETIME(3)（bun 模型，接口返回 unix 毫秒）。
--   * 旁表只在用户第一次改分类 / 收藏 / 版本时才有行；读接口 afv_asset LEFT JOIN afv_asset_ext，没行 = 未分类、未收藏。
--   * 不建外键：afv_asset 由 Java 维护（逻辑删除），旁表随之失效，读时按 afv_asset.deleted = 0 过滤即可。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_asset_ext` (
  `asset_id`           bigint       NOT NULL COMMENT '资产ID（afv_asset.id）',
  `user_id`            bigint       NOT NULL COMMENT '资产所属用户ID（冗余 afv_asset.user_id，分类计数 / 收藏筛选走本表索引）',
  `kind`               enum('model','scene','makeup','hair','accessory') DEFAULT NULL COMMENT '分类：model-模特 scene-场景 makeup-妆容 hair-发型 accessory-配饰；NULL=未分类',
  `favorite`           tinyint      NOT NULL DEFAULT '0' COMMENT '资产级收藏：1-已收藏',
  `license`            varchar(16)  NOT NULL DEFAULT '' COMMENT '授权状态：cleared-已确权 internal-仅内部 unknown-未知；空=未设置',
  `metadata`           json         DEFAULT NULL COMMENT '筛选元数据 {industry,style,shot,color}',
  `provenance`         json         DEFAULT NULL COMMENT '生成溯源 {run_id,workflow_id,node_id,operator,model,params,created_at}',
  `current_version_id` bigint       DEFAULT NULL COMMENT '当前版本（afv_asset_version.id）；NULL=原图',
  `created_at`         datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`         datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`asset_id`) USING BTREE,
  KEY `idx_user_kind` (`user_id`,`kind`) USING BTREE,
  KEY `idx_user_fav` (`user_id`,`favorite`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='资产扩展（afv_asset 旁表：分类、收藏、授权、元数据、溯源、当前版本）';

CREATE TABLE IF NOT EXISTS `afv_asset_version` (
  `id`         bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID（版本ID）',
  `asset_id`   bigint        NOT NULL COMMENT '资产ID（afv_asset.id）',
  `version_no` int           NOT NULL COMMENT '版本号：同资产从 1 起，追加时取 MAX+1',
  `image_rid`  varchar(128)  DEFAULT NULL COMMENT '图片资源号（媒体服务 rid / ai-gateway mr://），可空',
  `image_url`  varchar(1024) NOT NULL COMMENT '版本图片地址',
  `prompt`     text          COMMENT '生成该版本的提示词',
  `model`      varchar(64)   NOT NULL DEFAULT '' COMMENT '生成该版本的模型',
  `params`     json          DEFAULT NULL COMMENT '生成参数（前端原样）',
  `created_at` datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_asset_version` (`asset_id`,`version_no`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='资产版本';

CREATE TABLE IF NOT EXISTS `afv_asset_relation` (
  `id`               bigint      NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `asset_id`         bigint      NOT NULL COMMENT '资产ID（afv_asset.id）',
  `related_asset_id` bigint      NOT NULL COMMENT '关联资产ID（afv_asset.id）',
  `relation_type`    enum('reference','generated_from','version_of') NOT NULL COMMENT '关系：reference-参考（双向展示） generated_from-asset 由 related 生成 version_of-asset 是 related 的另一版本',
  `created_at`       datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_asset_related_type` (`asset_id`,`related_asset_id`,`relation_type`) USING BTREE,
  KEY `idx_related` (`related_asset_id`,`relation_type`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='资产关系';

CREATE TABLE IF NOT EXISTS `afv_asset_tag` (
  `id`         bigint      NOT NULL AUTO_INCREMENT COMMENT '主键ID（标签ID）',
  `user_id`    bigint      NOT NULL COMMENT '所属用户ID',
  `name`       varchar(32) NOT NULL COMMENT '标签名（≤20 字，同用户唯一）',
  `created_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_name` (`user_id`,`name`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='资产标签字典（每用户 ≤200 个）';

CREATE TABLE IF NOT EXISTS `afv_asset_tag_link` (
  `asset_id`   bigint      NOT NULL COMMENT '资产ID（afv_asset.id）',
  `tag_id`     bigint      NOT NULL COMMENT '标签ID（afv_asset_tag.id）',
  `created_at` datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  PRIMARY KEY (`asset_id`,`tag_id`) USING BTREE,
  KEY `idx_tag` (`tag_id`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='资产 ↔ 标签（每资产 ≤10 个）';
