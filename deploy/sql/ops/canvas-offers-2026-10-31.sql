-- canvas 计费场景配置（2026-10-31）：画布里由 Go 计费的三个入口（写法同 studio-offers-2026-10-30.sql）
--
--   入口码（entry_code）   场景名称          能力码           步骤 action / 计价模型（占位，待产品定价）
--   canvas_image          画布生图          ecom_image_gen   CREAFT_V1 / creaft-image-2（1k/2k/4k 分辨率行，按张）
--   canvas_tool_i2p       画布反推提示词    ecom_image_gen   CREAFT_V1 / creaft-image-2（按次，占位）
--   canvas_video          画布视频          ecom_video_gen   CREAFT_V1 / creaft-video-2.0（按秒，480p/720p/1080p/4k 行）
-- 入口码与 pkg/billing/audit.go Targets、Go_V1.0.25__canvas.sql 的素材库分类同名（snake_case，别改驼峰）。
-- 不在这里：canvas_tool_crop（裁剪不计费）、放大 / 扩图 / 重绘（Java 兼容工具按 Java 的口径扣）、试穿（studio_tryon）。
--
-- 所属库：Java 主项目库（afv_billing_* / afv_model_pricing / afv_ai_model），Go 只读算价。运营也可在后台手填。
-- 执行顺序：Go_V1.0.25__canvas.sql → 本脚本 → 产品定价后改价格模型 → bash install.sh check-billing 全过 → BILLING_MODE=on。
-- 为什么必须配：canvas-rpc 提交 image / video / i2p 时先做准入（billing.CheckAccess，与计费开关无关），
--   入口没有 offer 会被拒（「暂未开通该功能」2005）——缺配置不静默放行。
-- 模型缺失（creaft-image-2 / creaft-video-2.0 查不到）：对应入口整套都不建（不会留下 ENABLED 的空壳 offer），
--   脚本会 SELECT 出「⚠️ 模型缺失」告警行；先在后台补好模型再重跑本脚本即可补建（NOT EXISTS 判重）。
--   入口没有 offer 时 canvas-rpc 准入拒绝（2005），即「缺配置 = 关闭」，不会半套放行或免费放行。
-- 可重复执行：NOT EXISTS 判重，已有的不覆盖。先备份（已有备份不覆盖）。

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS afv_billing_offer_bak_1031             AS SELECT * FROM afv_billing_offer;
CREATE TABLE IF NOT EXISTS afv_billing_offer_variant_bak_1031     AS SELECT * FROM afv_billing_offer_variant;
CREATE TABLE IF NOT EXISTS afv_billing_charge_component_bak_1031  AS SELECT * FROM afv_billing_charge_component;

SET @img_model := (SELECT id FROM afv_ai_model WHERE code = 'creaft-image-2' AND deleted = 0 ORDER BY id LIMIT 1);
SET @vid_model := (SELECT id FROM afv_ai_model WHERE code = 'creaft-video-2.0' AND deleted = 0 ORDER BY id LIMIT 1);
SELECT @img_model AS `creaft-image-2（NULL=不存在）`, @vid_model AS `creaft-video-2.0（NULL=不存在）`;
-- 告警：模型缺失的入口（有行 = 该入口未建，别往下走）
SELECT t.offer_code AS `⚠️ 模型缺失，未建场景（先补模型再重跑）`
FROM (SELECT 'canvas_image' AS offer_code, @img_model AS model_id UNION ALL SELECT 'canvas_tool_i2p', @img_model UNION ALL SELECT 'canvas_video', @vid_model) t
WHERE t.model_id IS NULL;
SET @canvas_base_points := 0; -- 查不到单价行时的兜底；0 = 不兜底（让检查报错），待产品定价

DROP TEMPORARY TABLE IF EXISTS tmp_canvas;
CREATE TEMPORARY TABLE tmp_canvas (offer_code VARCHAR(64), name VARCHAR(64), category VARCHAR(16), feature VARCHAR(64), scene VARCHAR(32),
  ctype VARCHAR(16), model_id BIGINT, sort INT) DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
INSERT INTO tmp_canvas VALUES
  ('canvas_image',    '画布生图',       'IMAGE', 'ecom_image_gen', 'IMAGE_GEN', 'OUTPUT',   @img_model, 0),
  ('canvas_tool_i2p', '画布反推提示词', 'IMAGE', 'ecom_image_gen', 'IMAGE_GEN', 'OUTPUT',   @img_model, 0),
  ('canvas_video',    '画布视频',       'VIDEO', 'ecom_video_gen', 'VIDEO_GEN', 'DURATION', @vid_model, 0);

INSERT INTO afv_billing_offer (offer_code, name, category, entry_code, feature_code, billing_scene, status, sort, deleted)
SELECT t.offer_code, t.name, t.category, t.offer_code, t.feature, t.scene, 'ENABLED', t.sort, 0
FROM tmp_canvas t
WHERE t.model_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer o
                   WHERE o.offer_code = t.offer_code COLLATE utf8mb4_unicode_ci
                      OR (o.entry_code = t.offer_code COLLATE utf8mb4_unicode_ci AND o.deleted = 0));

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, included_outputs, settlement_type, status, deleted)
SELECT CONCAT(t.offer_code, '_standard'), t.offer_code, '标准版', 'standard', t.model_id, 1, 'PARTIAL', 'ENABLED', 0
FROM tmp_canvas t
WHERE t.model_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_standard') COLLATE utf8mb4_unicode_ci);

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT CONCAT('gen_', t.offer_code), CONCAT(t.offer_code, '_standard'), t.offer_code, t.ctype, 0, 'BILLED', 'CREAFT_V1', t.model_id, @canvas_base_points, 1.000, 1, 0
FROM tmp_canvas t
WHERE t.model_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_standard') COLLATE utf8mb4_unicode_ci AND v.deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = CONCAT('gen_', t.offer_code) COLLATE utf8mb4_unicode_ci);

-- 画布生图按模型分档（2026-11-09；载体与单价由 Java V1.0.215__atlas_image_pricing_and_studio_tiers.sql 建）：本段只在 offer 比那次迁移晚建时补档位，
-- 载体不存在的行自动跳过。canvas-rpc 提交带 Charge.ModelGroup，按「档位默认模型的 gateway_model_group」选档；没有对应档位退回 canvas_image_standard。
INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, included_outputs, settlement_type, status, deleted)
SELECT s.variant_code, o.offer_code, s.name, s.tier, m.id, 1, 'PARTIAL', 'ENABLED', 0
FROM (
            SELECT 'canvas_image_gpt_image_2_6' AS variant_code, 'GPT Image 2.6 Sunburst' AS name, 'gpt' AS tier, 'canvas-gpt-image-2.6-sunburst' AS model_code
  UNION ALL SELECT 'canvas_image_nano_banana_2_1_dev',        'Nano Banana 2.1 Dev',              'banana',   'creaft-nano-banana-2.1-dev'
  UNION ALL SELECT 'canvas_image_seedream_5_5',               'Seedream 5.5',                     'seedream', 'creaft-seedream-5.5'
  UNION ALL SELECT 'canvas_image_gpt_image_2_5_sunburst_dev', 'GPT Image 2.5 Sunburst Developer', 'gpt25dev', 'creaft-gpt-image-2.5-sunburst-developer'
) s
JOIN afv_billing_offer o ON o.entry_code = 'canvas_image' AND o.deleted = 0
JOIN afv_ai_model m ON m.code = s.model_code COLLATE utf8mb4_unicode_ci AND m.deleted = 0
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = s.variant_code COLLATE utf8mb4_unicode_ci);

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT CONCAT('gen_', v.variant_code), v.variant_code, v.offer_code, 'OUTPUT', 0, 'BILLED', 'CREAFT_V1', v.default_model_id, 0, 1.000, 1, 0
FROM afv_billing_offer_variant v
WHERE v.variant_code IN ('canvas_image_gpt_image_2_6', 'canvas_image_nano_banana_2_1_dev', 'canvas_image_seedream_5_5',
                         'canvas_image_gpt_image_2_5_sunburst_dev')
  AND v.deleted = 0
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = CONCAT('gen_', v.variant_code) COLLATE utf8mb4_unicode_ci);

-- 复核（只读）：每个入口应至少有一行单价
SELECT x.entry_code AS `入口码`, COALESCE(o.offer_code, '✗ 没建场景') AS `场景`, COALESCE(v.variant_code, '✗ 没建档位') AS `档位`,
       COALESCE(c.component_code, '✗ 没建步骤') AS `步骤`, COALESCE(m.code, '-') AS `模型`,
       COALESCE(p.price_points, -1) AS `单价（占位）`, COALESCE(p.price_unit, '✗ 没有单价') AS `单位`, COALESCE(p.resolution, 'ANY') AS `分辨率`
FROM (SELECT 'canvas_image' AS entry_code UNION ALL SELECT 'canvas_tool_i2p' UNION ALL SELECT 'canvas_video') x
LEFT JOIN afv_billing_offer o ON o.entry_code = x.entry_code AND o.deleted = 0 AND o.status = 'ENABLED'
LEFT JOIN afv_billing_offer_variant v ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
LEFT JOIN afv_billing_charge_component c ON c.variant_code = v.variant_code AND c.deleted = 0 AND c.cost_type = 'BILLED'
LEFT JOIN afv_ai_model m ON m.id = COALESCE(c.model_id, v.default_model_id) AND m.deleted = 0
LEFT JOIN afv_model_pricing p ON p.model_id = COALESCE(c.model_id, v.default_model_id)
      AND p.action_type = c.action_type AND p.status = 1 AND p.deleted = 0
ORDER BY x.entry_code, p.resolution;
