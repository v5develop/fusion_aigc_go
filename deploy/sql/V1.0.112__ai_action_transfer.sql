-- 动作迁移（执行面 9 / 86）：复用 afv_image_creation，新增两列存输入素材；
-- 用户导入的动作视频单独一张小表；预置动作视频复用模板素材表（localLife/actionTransfer/actionVideo）。
SET NAMES utf8mb4;

ALTER TABLE `afv_image_creation`
  ADD COLUMN `input_image_url` varchar(1024) DEFAULT NULL COMMENT '输入人物图（动作迁移：上传照片/公共数字人/AI定制形象解析后的地址）' AFTER `template_url`,
  ADD COLUMN `input_video_url` varchar(1024) DEFAULT NULL COMMENT '输入动作视频（动作迁移：预置模板或用户导入）' AFTER `input_image_url`;

ALTER TABLE `afv_image_creation`
  MODIFY COLUMN `kind` varchar(16) NOT NULL COMMENT '类型：poster-海报(78) longImage-长图(76) actionTransfer-动作迁移(9/86)',
  MODIFY COLUMN `content` text COMMENT '海报信息 / 长图内容（动作迁移不用）',
  MODIFY COLUMN `model` varchar(64) NOT NULL DEFAULT '' COMMENT '模型档位：海报 max；长图不传；动作迁移 doubao-seedance-2-0-260128 或 lite';

CREATE TABLE IF NOT EXISTS `afv_action_video` (
  `id`          bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`     bigint        NOT NULL COMMENT '导入用户ID',
  `name`        varchar(64)   NOT NULL COMMENT '展示名，如「我的动作1」',
  `url`         varchar(1024) NOT NULL COMMENT '视频地址（客户端直传后回传）',
  `cover_url`   varchar(1024) DEFAULT NULL COMMENT '封面图（首帧），前端可不传',
  `deleted`     tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time` datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time` datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  KEY `idx_user_time` (`user_id`,`deleted`,`id` DESC) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='用户导入的动作视频（动作迁移的输入素材，不是成品）';

-- 素材库聚合分类：动作迁移（成品是视频）
INSERT INTO `afv_asset_category` (`category_code`,`category_name`,`parent_id`,`level`,`media_type`,`sort_order`,`status`)
SELECT 'actionTransfer','动作迁移',2,2,'',50,1
WHERE NOT EXISTS (SELECT 1 FROM `afv_asset_category` c WHERE c.`category_code`='actionTransfer' AND c.`deleted`=0);

INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, 'actionTransfer', 10 FROM `afv_asset_category` c
WHERE c.`category_code`='actionTransfer' AND c.`deleted`=0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id`=c.`id` AND e.`entry_code`='actionTransfer');
