-- 区域蒙版（局部校色）：子任务的服装蒙版 + 通用蒙版缓存。
--
-- 蒙版 = 白色 + 软 alpha 的 PNG（区域内不透明、区域外透明、边缘半透明），前端校色时用 destination-in 只改蒙版内的像素。
-- 来源：ai-gateway image.segment（阿里云视觉 SegmentCloth / SegmentBody / SegmentCommodity / SegmentHDCommonImage），
-- studio-rpc 把网关产物的 alpha 转成「白色 + alpha」（背景 = 主体 alpha 取反）后转存素材库桶（公开地址，与 result_url 同口径）。
--
-- 1) afv_studio_task.mask_url：商拍换装 / 姿态复刻成品置成功后异步补的服装蒙版；NULL = 没有（未生成 / 识别失败 / 没识别到服装）。
--    蒙版是可选后置步骤：失败只记日志，不影响任务状态与计费。
--    商拍换装按用户选的单品分割（上衣 / 下装 / 裙装 / 外套 / 鞋 / 包帽），mask_url 恰好是这些单品的合并蒙版；
--    afv_studio_task.mask_parts 存逐件蒙版 [{"part":"tops","mask_url":"…"}]（前端按件切换校色范围）；NULL = 没有逐件结果。
-- 2) afv_studio_mask_cache：POST /api/v1/studio/masks 与子任务蒙版共用的缓存，按（规范化地址, 区域, 部位）唯一。
--    parts：服装细分部位（tops / coat / pants / skirt / shoes / bag / hat，按固定顺序逗号拼接）；'' = 不细分（全部服装 / 其它区域）。
--    part_masks：多部位请求时的逐件蒙版 JSON [{"part":"tops","mask_url":"…"}]（单件或不细分时为 NULL）。
--    规范化地址 = 去掉查询串与锚点、scheme/host 小写；url_key = 其 sha256（hex）。同一张图同一区域只调一次网关。
--    status：1 成功（mask_url 有值）；2 没识别到对应区域（不再重复调用，24 小时后允许重试）。服务失败不落行。
--
-- 只增不改：加两个可空列 + 一张新表 + 一个前缀索引（见 3），旧数据不动。
SET NAMES utf8mb4;

ALTER TABLE `afv_studio_task`
  ADD COLUMN `mask_url` varchar(1024) DEFAULT NULL COMMENT '服装蒙版（白色+软 alpha PNG，素材库桶公开地址）；NULL=没有' AFTER `color_fix_params`,
  ADD COLUMN `mask_parts` json DEFAULT NULL COMMENT '逐件服装蒙版 [{part, mask_url}]（按所选单品分割时）；NULL=没有' AFTER `mask_url`;

CREATE TABLE IF NOT EXISTS `afv_studio_mask_cache` (
  `id`              bigint       NOT NULL AUTO_INCREMENT,
  `url_key`         char(64)     NOT NULL COMMENT 'sha256(规范化地址) hex',
  `target`          varchar(16)  NOT NULL COMMENT '区域：garment / product / person / background',
  `parts`           varchar(64)  NOT NULL DEFAULT '' COMMENT '服装细分部位（固定顺序逗号拼接）；空=不细分',
  `source_url`      varchar(2048) NOT NULL COMMENT '规范化地址（去查询串），排障用',
  `status`          tinyint      NOT NULL COMMENT '1 成功；2 没识别到对应区域',
  `mask_rid`        varchar(64)  DEFAULT NULL COMMENT '蒙版在媒体服务的 rid（素材库桶）',
  `mask_url`        varchar(1024) DEFAULT NULL COMMENT '蒙版公开地址（多部位时为合并蒙版）；status=2 时为 NULL',
  `part_masks`      json         DEFAULT NULL COMMENT '多部位请求的逐件蒙版 [{part, mask_url}]；其余为 NULL',
  `coverage`        decimal(7,6) NOT NULL DEFAULT 0 COMMENT '蒙版不透明像素占比 0–1',
  `width`           int          NOT NULL DEFAULT 0,
  `height`          int          NOT NULL DEFAULT 0,
  `model_group`     varchar(64)  NOT NULL DEFAULT '' COMMENT '网关模型组',
  `gateway_call_id` varchar(64)  DEFAULT NULL,
  `cost_cny`        decimal(12,6) NOT NULL DEFAULT 0 COMMENT '网关分割调用成本（平台承担，不向用户收费）',
  `user_id`         bigint       NOT NULL DEFAULT 0 COMMENT '首次请求的用户',
  `hits`            int          NOT NULL DEFAULT 0 COMMENT '命中缓存次数',
  `created_at`      datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`      datetime(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_mask_url_target_parts` (`url_key`, `target`, `parts`),
  KEY `idx_mask_created` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='studio 区域蒙版缓存（局部校色）';

-- 3) 免费档水印视图（mrw://{rid}.{ext}）：免费档用户的成品原件留私有桶，result_url 写 mrw://，读时签带水印参数的地址；
--    用户付过钱后 studio-rpc 的付费转存扫描按 result_url LIKE 'mrw://%' 找候选。前缀索引只取 6 个字符（正好覆盖 'mrw://'），
--    让这个扫描走范围而不是全表。只加索引，不改数据。
ALTER TABLE `afv_studio_task`
  ADD KEY `idx_result_url_scheme` (`result_url`(6));
