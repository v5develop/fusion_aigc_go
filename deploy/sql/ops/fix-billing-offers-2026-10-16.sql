-- 计费配置补齐（2026-10-16）。可重复执行；跑完用 `bash install.sh check-billing` 自查。
--
-- 做的事：
--   A. 建图文笔记 / 海报 / 长图 三个计费场景（GPT Image 2 = creaft-image-2，CREAFT_V1 单价沿用现成的 10 点）
--   B. 动作迁移：lite 档改名（standard → lite）；cloud 档补步骤；去掉叠加的旧步骤
--   C. 爆款复刻：lite 步骤从 cloud 档搬回 lite 档（cloud 档不再叠加）；删掉挂在拼错的档位 video_remake_litee 上的孤儿步骤
--   D. 探店 / 带货短剧 / 数字人口播 / AI数字人：建场景 + lite 档 + 云端档
--   E. 线上没有 Seedance 2.5：新建模型 doubao-seedance-2-5，按分辨率定价（2.0 价上浮 50% 取整，运营可在后台改，见 E 段注释）
--   F. 爆款视频复刻：新建入口 videoRemake 的场景与 lite / cloud 两档（Go 生图服务按这个入口码计费）
--   G. 动作迁移云端档：用 Seedance 2.5（默认 720p，价格取 2.5 的分辨率定价），云端步骤挂到这个档位上
--
-- 价格不在这里写：图片取模型 creaft-image-2 现有的 CREAFT_V1 行，视频取各档默认模型的单价行。
-- 产品要调价，只改 afv_model_pricing（后台「模型定价」）里对应行，不用动这份配置。
--
-- ⚠️ 改的是现有系统的配置表，先备份（下面三条已写，重复执行不会覆盖已有备份）。

CREATE TABLE IF NOT EXISTS afv_billing_offer_bak_1016             AS SELECT * FROM afv_billing_offer;
CREATE TABLE IF NOT EXISTS afv_billing_offer_variant_bak_1016     AS SELECT * FROM afv_billing_offer_variant;
CREATE TABLE IF NOT EXISTS afv_billing_charge_component_bak_1016  AS SELECT * FROM afv_billing_charge_component;
CREATE TABLE IF NOT EXISTS afv_ai_model_bak_1016                  AS SELECT * FROM afv_ai_model;
CREATE TABLE IF NOT EXISTS afv_model_pricing_bak_1016            AS SELECT * FROM afv_model_pricing;

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
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_offer o WHERE o.offer_code = t.offer_code COLLATE utf8mb4_unicode_ci OR (o.entry_code = t.entry_code COLLATE utf8mb4_unicode_ci AND o.deleted = 0));

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, included_outputs, settlement_type, status, deleted)
SELECT CONCAT(t.offer_code, '_standard'), t.offer_code, '标准版', 'standard', @img_model, 1, 'PARTIAL', 'ENABLED', 0
FROM tmp_img t
WHERE @img_model IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_standard') COLLATE utf8mb4_unicode_ci);

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT CONCAT('gen_', t.offer_code), CONCAT(t.offer_code, '_standard'), t.offer_code, 'OUTPUT', t.mode, 'BILLED', 'CREAFT_V1', @img_model, 0, 1.000, 1, 0
FROM tmp_img t
WHERE @img_model IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = CONCAT('gen_', t.offer_code) COLLATE utf8mb4_unicode_ci);
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
 WHERE component_code = 'gen_action_transfer_cloud' AND deleted = 0
   AND EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = 'action_transfer_cloud' AND deleted = 0);
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

-- ── E. 新建模型 doubao-seedance-2-5（线上没有）────────────────────────────────
-- 建模型记录，并按分辨率给默认单价：2.0 的价 ×1.5 取整（480p 7→11，720p 14→21，1080p 33→50）。
-- 默认价只在该分辨率还没有 VIDEO_GEN 定价行时插入；运营在后台「模型定价」（afv_model_pricing）调过的价，重跑脚本不会被覆盖。
-- ⚠️ 这三个数是占位值（2.0 价上浮 50%）。上线前请确认，不对就在这里改，或直接在后台改。
-- 配价前还要确认执行面（网关模型组 / provider）能真的生成视频，否则云端档会计费却生成失败。
-- 模型配置与 Seedance 2.0 同结构（config.aigcOptions：分辨率 / 画幅 / 时长），默认分辨率 720p。已有的模型不改。
INSERT INTO afv_ai_model (code, name, model_type, status, config, deleted)
SELECT 'doubao-seedance-2-5', 'Seedance 2.5', 3, 1,
       '{"aigcOptions":{"resolutions":["480p","720p","1080p"],"defaultResolution":"720p","aspectRatios":["9:16","16:9"],"defaultAspectRatio":"16:9","durations":[4,5,10,15],"defaultDuration":5}}', 0
FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM afv_ai_model WHERE code = 'doubao-seedance-2-5' AND deleted = 0);

INSERT INTO afv_model_pricing (model_id, action_type, price_points, price_unit, resolution, status, deleted)
SELECT m.id, 'VIDEO_GEN', v.price, 'PER_SECOND', v.res, 1, 0
FROM afv_ai_model m
JOIN (SELECT '480p' AS res, 11 AS price UNION ALL SELECT '720p', 21 UNION ALL SELECT '1080p', 50) v
WHERE m.code = 'doubao-seedance-2-5' AND m.deleted = 0
  AND NOT EXISTS (SELECT 1 FROM afv_model_pricing p WHERE p.model_id = m.id AND p.action_type = 'VIDEO_GEN' AND p.resolution = v.res AND p.deleted = 0);

-- ── D. 探店 / 带货短剧 / 数字人口播 / AI数字人（视频，lite + 云端两档）──────────
-- lite 档用 xiaoqi-video-1.0-lite；云端档用 Seedance 2.5。文档说 Seedance 2.5 线上还没建，
-- 云端档用 doubao-seedance-2-5，由 E 段先建好；E 段之后模型查不到的话，只建 lite 档。
-- 步骤不写 model_id，单价取档位默认模型在 afv_model_pricing 里的按秒行（同 action_transfer）。
-- 模式号取文档里列的第一个；price() 不用它算价，只是存档。
SET @seedance25 := (SELECT id FROM afv_ai_model WHERE code = 'doubao-seedance-2-5' AND deleted = 0 ORDER BY id LIMIT 1);
SET @xq_lite := (SELECT id FROM afv_ai_model WHERE code = 'xiaoqi-video-1.0-lite' AND deleted = 0 ORDER BY id LIMIT 1);

DROP TEMPORARY TABLE IF EXISTS tmp_vid;
CREATE TEMPORARY TABLE tmp_vid (offer_code VARCHAR(64), name VARCHAR(64), entry_code VARCHAR(64), mode INT, sort INT) DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
INSERT INTO tmp_vid VALUES
  ('explore_shop',  '探店视频',   'exploreShop', 72, 0),
  ('short_drama',   '带货短剧',   'shortDrama',  87, 0),
  ('voiceover',     '数字人口播', 'voiceover',   92, 0),
  ('digital_human', 'AI数字人',   'digitalHuman', 85, 0);

INSERT INTO afv_billing_offer (offer_code, name, category, entry_code, feature_code, billing_scene, status, sort, deleted)
SELECT t.offer_code, t.name, 'VIDEO', t.entry_code, 'ecom_video_gen', 'VIDEO_GEN', 'ENABLED', t.sort, 0
FROM tmp_vid t
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_offer o WHERE o.offer_code = t.offer_code COLLATE utf8mb4_unicode_ci OR (o.entry_code = t.entry_code COLLATE utf8mb4_unicode_ci AND o.deleted = 0));

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, included_outputs, settlement_type, status, deleted)
SELECT CONCAT(t.offer_code, '_lite'), t.offer_code, 'lite', 'lite', @xq_lite, 1, 'ALL_OR_NOTHING', 'ENABLED', 0
FROM tmp_vid t
WHERE @xq_lite IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_lite') COLLATE utf8mb4_unicode_ci);

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, default_resolution, included_outputs, settlement_type, status, deleted)
SELECT CONCAT(t.offer_code, '_cloud'), t.offer_code, 'cloud', 'cloud', @seedance25, '720p', 1, 'ALL_OR_NOTHING', 'ENABLED', 0
FROM tmp_vid t
WHERE @seedance25 IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_cloud') COLLATE utf8mb4_unicode_ci);

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT CONCAT('gen_', t.offer_code, '_lite'), CONCAT(t.offer_code, '_lite'), t.offer_code, 'DURATION', t.mode, 'BILLED', 'VIDEO_GEN', NULL, 0, 1.000, 1, 0
FROM tmp_vid t
WHERE @xq_lite IS NOT NULL
  AND EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = CONCAT(t.offer_code, '_lite') COLLATE utf8mb4_unicode_ci AND deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = CONCAT('gen_', t.offer_code, '_lite') COLLATE utf8mb4_unicode_ci);

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT CONCAT('gen_', t.offer_code, '_cloud'), CONCAT(t.offer_code, '_cloud'), t.offer_code, 'DURATION', t.mode, 'BILLED', 'VIDEO_GEN', NULL, 0, 1.000, 1, 0
FROM tmp_vid t
WHERE @seedance25 IS NOT NULL
  AND EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = CONCAT(t.offer_code, '_cloud') COLLATE utf8mb4_unicode_ci AND deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = CONCAT('gen_', t.offer_code, '_cloud') COLLATE utf8mb4_unicode_ci);

-- ── F. 爆款视频复刻（入口 videoRemake）─────────────────────────────────────────
-- Go 生图服务计费时入口码就是 videoRemake；线上没有这个入口，只有 hitVideoReplication / hitReplication（另一个产品，不复用）。
-- lite 档不指定分辨率，同 action_transfer_lite：按 720p 1 点/秒（线上现状，保持不变）。
-- 步骤的 mode：lite 用 83，cloud 用 71（和 C 段里的步骤命名一致）。
INSERT INTO afv_billing_offer (offer_code, name, category, entry_code, feature_code, billing_scene, status, sort, deleted)
SELECT 'video_remake', '爆款视频复刻', 'VIDEO', 'videoRemake', 'ecom_video_gen', 'VIDEO_GEN', 'ENABLED', 0, 0 FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_offer o WHERE o.offer_code = 'video_remake' OR (o.entry_code = 'videoRemake' AND o.deleted = 0));

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, default_resolution, included_outputs, settlement_type, status, deleted)
SELECT 'video_remake_lite', 'video_remake', 'lite', 'lite', @xq_lite, NULL, 1, 'ALL_OR_NOTHING', 'ENABLED', 0 FROM DUAL
WHERE @xq_lite IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = 'video_remake_lite');

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, default_resolution, included_outputs, settlement_type, status, deleted)
SELECT 'video_remake_cloud', 'video_remake', 'cloud', 'cloud', @seedance25, '720p', 1, 'ALL_OR_NOTHING', 'ENABLED', 0 FROM DUAL
WHERE @seedance25 IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = 'video_remake_cloud');

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT 'gen_video_remake_lite', 'video_remake_lite', 'video_remake', 'DURATION', 83, 'BILLED', 'VIDEO_GEN', NULL, 0, 1.000, 1, 0 FROM DUAL
WHERE EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = 'video_remake_lite' AND deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = 'gen_video_remake_lite');

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT 'gen_video_remake_cloud', 'video_remake_cloud', 'video_remake', 'DURATION', 71, 'BILLED', 'VIDEO_GEN', NULL, 0, 1.000, 1, 0 FROM DUAL
WHERE EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = 'video_remake_cloud' AND deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = 'gen_video_remake_cloud');

-- ── G. 动作迁移云端档（Seedance 2.5，默认 720p）────────────────────────────────
-- 云端档要等 E 段建好模型，所以放在最后。档位和步骤都是「没有才建」，已有的不覆盖。
INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, default_resolution, included_outputs, settlement_type, status, deleted)
SELECT 'action_transfer_cloud', 'action_transfer', 'cloud', 'cloud', @seedance25, '720p', 1, 'ALL_OR_NOTHING', 'ENABLED', 0 FROM DUAL
WHERE @seedance25 IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = 'action_transfer_cloud');

-- 云端步骤：已有的（之前挂在 standard 档上）挪到 cloud 档；没有就新建（执行面任务号 86）
UPDATE afv_billing_charge_component SET variant_code = 'action_transfer_cloud'
 WHERE component_code = 'gen_action_transfer_cloud' AND deleted = 0
   AND EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = 'action_transfer_cloud' AND deleted = 0);

INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT 'gen_action_transfer_cloud', 'action_transfer_cloud', 'action_transfer', 'DURATION', 86, 'BILLED', 'VIDEO_GEN', NULL, 0, 1.000, 1, 0 FROM DUAL
WHERE EXISTS (SELECT 1 FROM afv_billing_offer_variant WHERE variant_code = 'action_transfer_cloud' AND deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = 'gen_action_transfer_cloud' AND c.deleted = 0);
