-- ============================================================
-- 本地生活生成配置：海报 / 长图 / 动作迁移 / 数字人口播 / 视频的模型、比例、时长可在运营后台调整。
--
-- 只能改「启用 / 名称 / 排序 / 默认」，不能新增编码：编码直接决定执行面路由（如 lite → 本地 GPU），
-- 新增编码要改代码并核对路由。库里某组没有启用项时，回落代码里原有的默认值，行为不变。
-- 只增不改：表与种子都用 IF NOT EXISTS / NOT EXISTS 守卫，重复执行无副作用。
-- ============================================================

CREATE TABLE IF NOT EXISTS `afv_local_life_gen_option` (
  `id`           bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `option_group` varchar(32)  NOT NULL COMMENT '选项组：poster_models / poster_sizes / action_models / voiceover_models / voiceover_sizes / voiceover_durations / remake_models / video_models / video_sizes',
  `code`         varchar(64)  NOT NULL COMMENT '选项编码（执行面取值，运营不可新增）',
  `label`        varchar(64)  NOT NULL COMMENT '展示名',
  `sort_order`   int          NOT NULL DEFAULT '0' COMMENT '排序，小的在前',
  `enabled`      tinyint      NOT NULL DEFAULT '1' COMMENT '是否启用：0-停用 1-启用',
  `is_default`   tinyint      NOT NULL DEFAULT '0' COMMENT '是否默认：组内最多一个，且必须启用',
  `create_time`  datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`  datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_group_code` (`option_group`, `code`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='本地生活生成选项（模型 / 比例 / 时长）';

-- 种子：与代码里原有的列表和顺序一致；默认值与原口径一致（海报 max、口播 seedance2.5 / 15 秒）
INSERT INTO `afv_local_life_gen_option` (`option_group`, `code`, `label`, `sort_order`, `enabled`, `is_default`)
SELECT t.g, t.c, t.l, t.s, 1, t.d FROM (
  SELECT 'poster_models' AS g, 'max' AS c, 'max' AS l, 10 AS s, 1 AS d
  UNION ALL SELECT 'poster_sizes', '9:16', '9:16', 10, 0
  UNION ALL SELECT 'poster_sizes', '16:9', '16:9', 20, 0
  UNION ALL SELECT 'poster_sizes', '3:4', '3:4', 30, 0
  UNION ALL SELECT 'poster_sizes', '4:3', '4:3', 40, 0
  UNION ALL SELECT 'poster_sizes', '2:3', '2:3', 50, 0
  UNION ALL SELECT 'poster_sizes', '1:1', '1:1', 60, 0
  UNION ALL SELECT 'action_models', 'doubao-seedance-2-0-260128', 'seedance 2.0', 10, 0
  UNION ALL SELECT 'action_models', 'lite', 'xiaoqi-video-1.0-lite', 20, 0
  UNION ALL SELECT 'voiceover_models', 'seedance2.5', 'seedance 2.5', 10, 1
  UNION ALL SELECT 'voiceover_models', 'lite', 'XQ Video 1.0 Lite', 20, 0
  UNION ALL SELECT 'voiceover_sizes', '9:16', '9:16', 10, 0
  UNION ALL SELECT 'voiceover_sizes', '16:9', '16:9', 20, 0
  UNION ALL SELECT 'voiceover_durations', '15', '15 秒', 10, 1
  UNION ALL SELECT 'voiceover_durations', '30', '30 秒', 20, 0
  UNION ALL SELECT 'remake_models', 'doubao-seedance-2-0-260128', 'seedance 2.0', 10, 0
  UNION ALL SELECT 'remake_models', 'lite', 'XQ Video 1.0 Lite', 20, 0
  UNION ALL SELECT 'video_models', 'seedance2.5', 'seedance 2.5', 10, 0
  UNION ALL SELECT 'video_models', 'lite', 'XQ Video 1.0 Lite', 20, 0
  UNION ALL SELECT 'video_sizes', '9:16', '9:16', 10, 0
  UNION ALL SELECT 'video_sizes', '16:9', '16:9', 20, 0
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_local_life_gen_option` o WHERE o.option_group = t.g AND o.code = t.c);
