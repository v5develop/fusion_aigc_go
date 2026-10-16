-- 爆款视频复刻（70 解析 → 71 复刻）：复用 afv_video_creation（同样是两步：先出剧情，确认后出片）
--
-- 两种入口：上传本地视频（upload）、粘贴热门视频链接（link，需链接解析能力，待定）。
SET NAMES utf8mb4;

ALTER TABLE `afv_video_creation`
  ADD COLUMN `source`     varchar(16)   DEFAULT NULL COMMENT '视频复刻的来源：upload-上传本地视频 link-粘贴链接' AFTER `kind`,
  ADD COLUMN `source_url` varchar(1024) DEFAULT NULL COMMENT '用户粘贴的原始分享链接（link 来源）' AFTER `source`,
  ADD COLUMN `source_video_url` varchar(1024) DEFAULT NULL COMMENT '待解析视频的公网地址（交给执行面 70 下载）；video_url 是成片地址' AFTER `source_url`,
  ADD COLUMN `title`      varchar(255)  DEFAULT NULL COMMENT '原视频标题（链接解析返回，展示用）' AFTER `source_video_url`,
  ADD COLUMN `cover_url`  varchar(1024) DEFAULT NULL COMMENT '原视频封面（链接解析返回，展示用）' AFTER `title`;

ALTER TABLE `afv_video_creation`
  MODIFY COLUMN `kind` varchar(16) NOT NULL COMMENT '类型：exploreShop-探店视频(72/73) shortDrama-带货短剧(87/88) videoRemake-爆款视频复刻(70/71)',
  MODIFY COLUMN `plan_script` json DEFAULT NULL COMMENT '第一步的产物：探店/短剧是分镜剧本 JSON；视频复刻是 70 返回的剧情文本（JSON 字符串）',
  MODIFY COLUMN `duration` int NOT NULL DEFAULT '15' COMMENT '视频时长秒数：探店/短剧 15/30；视频复刻按 70 返回的原片时长回填';

-- 素材库：复刻成品归到现有的「视频 → 视频复刻」分类下，新增本项目的入口编码
INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, 'videoRemake', 20 FROM `afv_asset_category` c
WHERE c.`category_code` = 'videoReplication' AND c.`deleted` = 0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id` = c.`id` AND e.`entry_code` = 'videoRemake');
