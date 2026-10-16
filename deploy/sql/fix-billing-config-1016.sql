-- 计费配置补齐（2026-10-16）。可重复执行；跑完用 `bash install.sh check-billing` 自查。
--
-- 做三件事：
--   A. 建图文笔记 / 海报 / 长图 三个计费场景（GPT Image 2 = creaft-image-2，CREAFT_V1 单价沿用现成的 10 点）
--   B. 动作迁移：lite 档改名（standard → lite）；cloud 档补步骤；去掉叠加的旧步骤
--   C. 爆款复刻：lite 步骤从 cloud 档搬回 lite 档（cloud 档不再叠加）；删掉挂在拼错的档位 video_remake_litee 上的孤儿步骤
--
-- 价格不在这里写：图片取模型 creaft-image-2 现有的 CREAFT_V1 行，视频取各档默认模型的单价行。
-- 产品要调价，只改 afv_model_pricing（后台「模型定价」）里对应行，不用动这份配置。
--
-- ⚠️ 改的是现有系统的配置表，先备份（下面三条已写，重复执行不会覆盖已有备份）。

CREATE TABLE IF NOT EXISTS afv_billing_offer_bak_1016             AS SELECT * FROM afv_billing_offer;
CREATE TABLE IF NOT EXISTS afv_billing_offer_variant_bak_1016     AS SELECT * FROM afv_billing_offer_variant;
CREATE TABLE IF NOT EXISTS afv_billing_charge_component_bak_1016  AS SELECT * FROM afv_billing_charge_component;

-- ── A. 图片三个场景 ───────────────────────────────────────────────────────────
-- 模型按 code 找，各库 id 不一样（测试库 14，Java 说线上 181）。
SET @img_model := (SELECT id FROM afv_ai_model WHERE code = 'creaft-image-2' AND deleted = 0 ORDER BY id LIMIT 1);

-- 场景（offer_code, 名称, 入口码, 执行面任务号, 排序）
DROP TEMPORARY TABLE IF EXISTS tmp_img;
CREATE TEMPORARY TABLE tmp_img (offer_code VARCHAR(64), name VARCHAR(64), entry_code VARCHAR(64), mode INT, sort INT) DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
INSERT INTO tmp_img VALUES
  ('image_text_note', 'AI图文笔记', 'imageTextNote', 75, 0),
  ('poster_gen',      'AI海报',     'poster',        78, 0),
  ('long_image_gen',  'AI长图',     'longImage',     76, 0);

INSERT INTO afv_billing_offer (offer_code, name, category, entry_code, feature_code, billing_scene, status, sort, deleted)
SELECT t.offer_code, t.name, 'IMAGE', t.entry_code, 'ecom_image_gen', 'IMAGE_GEN', 'ENABLED', t.sort, 0
FROM tmp_img t
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_offer o WHERE o.offer_code = t.offer_code OR (o.entry_code = t.entry_code AND o.deleted = 0));

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, included_outputs, settlement_type, status, deleted)
SELECT CONCAT(t.offer_code, '_standard'), t.offer_code, '标准版', 'standard', @img_model, 1, 'PARTIAL', 'ENABLED', 0
FROM tmp_img t
WHERE @img_model IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_standard'));

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT CONCAT('gen_', t.offer_code), CONCAT(t.offer_code, '_standard'), t.offer_code, 'OUTPUT', t.mode, 'BILLED', 'CREAFT_V1', @img_model, 0, 1.000, 1, 0
FROM tmp_img t
WHERE @img_model IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = CONCAT('gen_', t.offer_code));
-- settlement_type 用 PARTIAL：海报 / 长图 / 笔记一次出多张，部分成功时按实际成功张数结算（和产品「部分成功按完成处理」一致）。

-- ── B. 动作迁移 ───────────────────────────────────────────────────────────────
-- B1 lite 档改名：档位和挂在上面的步骤一起改
UPDATE afv_billing_offer_variant SET variant_code = 'action_transfer_lite', name = 'lite'
 WHERE variant_code = 'action_transfer_standard' AND deleted = 0
   AND NOT EXISTS (SELECT 1 FROM (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = 'action_transfer_lite') x);
UPDATE afv_billing_charge_component SET variant_code = 'action_transfer_lite'
 WHERE variant_code = 'action_transfer_standard' AND deleted = 0;
-- B2 云端的步骤（执行面任务号 86）搬回 cloud 档
UPDATE afv_billing_charge_component SET variant_code = 'action_transfer_cloud'
 WHERE component_code = 'gen_action_transfer_cloud' AND deleted = 0;
-- B3 旧的 OUTPUT 步骤（单价 1~2，和 lite 的按秒步骤会叠加）作废，换成按秒的 lite 步骤
UPDATE afv_billing_charge_component SET deleted = 1
 WHERE component_code = 'gen_action_transfer' AND deleted = 0;
INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT 'gen_action_transfer_lite_9', 'action_transfer_lite', 'action_transfer', 'DURATION', 9, 'BILLED', 'VIDEO_GEN', NULL, 0, 1.000, 1, 0
FROM DUAL WHERE EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = 'action_transfer_lite' AND deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component WHERE component_code = 'gen_action_transfer_lite_9');

-- ── C. 爆款复刻 ───────────────────────────────────────────────────────────────
-- C1 lite 步骤（任务号 83）搬回 lite 档
UPDATE afv_billing_charge_component SET variant_code = 'video_remake_lite'
 WHERE component_code = 'gen_video_remake_lite_83' AND variant_code = 'video_remake_cloud' AND deleted = 0;
-- C2 孤儿步骤：挂在拼错的档位 video_remake_litee 上，永远命中不了
UPDATE afv_billing_charge_component SET deleted = 1
 WHERE component_code = 'gen_video_remake_lite' AND variant_code = 'video_remake_litee' AND deleted = 0;
