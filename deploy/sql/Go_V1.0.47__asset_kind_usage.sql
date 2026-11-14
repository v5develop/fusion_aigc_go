-- ============================================================
-- 资产库「一个分类」：afv_asset_ext.kind 扩展取值 + 系统标签列兜底
--
-- 1. afv_asset_ext.kind（Go 独写旁表）ENUM 追加：product / storefront / dish / logo / poster /
--    video_final / video_segment / reference / other（person 并入已有 model，scene 沿用）。
--    取值与 pkg/asset.Usages 一一对应；新值追加在末尾（MySQL 8 只改元数据，不重建表）。
--    kind 只存「用户改过的分类」；为空时资产库读接口按 sys_tags 用途标签 → entry_code → type 推算默认分类返回（不回填）。
-- 2. afv_asset.sys_tags（Java 表，属主 Java，fusion-aigc V1.0.236 建）：Go 读写这一列，为了不形成
--    「Java 必须先上线」的部署顺序约束，这里按同一定义判存在补列——谁先跑都行，另一边判存在后跳过。
--
-- 可重复执行：MODIFY 同一定义重复执行无副作用；加列按 information_schema 判存在。
-- ============================================================
SET NAMES utf8mb4;

ALTER TABLE `afv_asset_ext`
  MODIFY COLUMN `kind` enum('model','scene','makeup','hair','accessory',
                            'product','storefront','dish','logo','poster','video_final','video_segment','reference','other')
  DEFAULT NULL COMMENT '用户改过的分类：model-模特 scene-场景 makeup-妆容 hair-发型 accessory-配饰 product-商品 storefront-门头门店 dish-菜品 logo-Logo poster-海报 video_final-成片 video_segment-视频片段 reference-参考图 other-其他；NULL=没改过（读时按系统标签/入口/类型推算）';

DROP PROCEDURE IF EXISTS `go_v1047_asset_sys_tags`;
DELIMITER $$
CREATE PROCEDURE `go_v1047_asset_sys_tags`()
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE()
                   AND TABLE_NAME = 'afv_asset' AND COLUMN_NAME = 'sys_tags') THEN
        ALTER TABLE `afv_asset`
          ADD COLUMN `sys_tags` json NULL COMMENT '系统标签（JSON 字符串数组：来源功能、用途:xxx、行业:xxx、模板:xxx、清晰度、第N段等），进库时由系统写入，用户不可改';
    END IF;
END$$
DELIMITER ;
CALL `go_v1047_asset_sys_tags`();
DROP PROCEDURE IF EXISTS `go_v1047_asset_sys_tags`;
