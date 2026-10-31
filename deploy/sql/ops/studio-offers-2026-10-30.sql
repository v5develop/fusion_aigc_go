-- studio 计费场景配置（2026-10-30）：商拍换装 / 姿态复刻 / 图片生成 三个入口
--
-- 做什么：在 Java 后台库建 3 个 offer（场景）+ 各 1 个 variant（档位）+ 各 1 个 component（计费步骤）。
--   入口码（entry_code）   场景名称            offer_code      能力码
--   studio_tryon          商拍换装·网页版     studio_tryon    ecom_image_gen
--   studio_pose           姿态复刻·网页版     studio_pose     ecom_image_gen
--   studio_imagegen       图片生成·网页版     studio_imagegen ecom_image_gen
-- 入口码与 pkg/billing/audit.go 的 Targets、Go_V1.0.21__studio.sql 里的素材库分类 category_code 同名。
-- 这三个入口是 snake_case，与本地生活其它入口的驼峰（imageTextNote）不同，别「顺手改成驼峰」：代码按原样查。
-- offer_code 与 entry_code 取同一个值（offer.offer_code 是唯一键，经它关联档位；两者相同不影响关联）。
--
-- 所属库：Java 主项目库（fusion_aigc，表 afv_billing_* / afv_model_pricing / afv_ai_model）。
--   不是 Go 自己的库——Go 侧只读这些表算价（pkg/billing/price.go），配置权在 Java 后台。
--   运营也可以不跑脚本，直接在后台「计费管理 / 计费场景」手填，口径见 docs/billing/studio-入口码配置指引.md。
--
-- 执行顺序（必须按序）：
--   1. Go_V1.0.21__studio.sql          （Go 库：建 afv_studio_* 三表 + 素材库分类，与本脚本不同库）
--   2. 本脚本                          （Java 库：offer / variant / component）
--   3. 填价：产品定价后在后台「计价规则」（afv_model_pricing）补行，或按文末 ⑤ 说明处理
--   4. bash install.sh check-billing   （或 go run ./tools/ops/billing-check -dsn '...'）全部通过
--   5. 再把 BILLING_MODE 切 on
--
-- 与 install.sh 的关系（不配会卡部署）：
--   audit.go 的 Targets 已登记这三个入口，billing-check 会对每个入口真的算一遍价。
--   · 没有 offer / 没有档位 / 没有步骤 / 单价为 0 → billing-check 报 ERROR
--   · install.sh 的「4/6 安装程序」只警告（BILLING_MODE=off）或中止（BILLING_MODE=on）；
--     「6/6 计费配置检查」（cmd_prepare）有 ERROR 一律中止部署
--   · 线上提交时缺配置的入口会被拒（ErrNoOffer），不会静默放行成免费
--
-- 价格：本脚本不新建价格行。步骤指向图片模型 creaft-image-2 的 CREAFT_V1 单价行
--   （1k / 2k / 4k 分别一行，pricing-update-2026-10-20 之后为 2 / 3 / 4 点，不限分辨率一行），
--   这和图文笔记 / 海报 / 长图的做法一致，所以脚本一跑完 billing-check 就能算出价。
--   ⚠️ 这是占位：待产品定价。creaft-image-2 的单价行被 Java 17 条图片路由与三个本地生活图片场景共用，
--      调它会连带改别人的价；studio 要单独定价（含四档模型各自不同价）时，按文末 ⑤ 另建模型 / 价格行，
--      并在 pkg/billing/audit.go Targets 补 Tiers（按分辨率 / 模型档分档时需要，档位码与本脚本的 variant 保持一致）。
--
-- 可重复执行：offer / variant / component 都是「没有才建」（NOT EXISTS 判重，同现有 ops 脚本），已有的不覆盖。
--   component_code 有唯一键，软删除的同名行也算存在，所以判重不加 deleted 过滤。
--
-- ⚠️ 改的是现有系统的配置表，先备份（下面三条已写，重复执行不会覆盖已有备份）。

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS afv_billing_offer_bak_1030             AS SELECT * FROM afv_billing_offer;
CREATE TABLE IF NOT EXISTS afv_billing_offer_variant_bak_1030     AS SELECT * FROM afv_billing_offer_variant;
CREATE TABLE IF NOT EXISTS afv_billing_charge_component_bak_1030  AS SELECT * FROM afv_billing_charge_component;

-- ── 0. 前置检查：图片模型 creaft-image-2 要存在，且有 CREAFT_V1 单价行 ───────────────────
-- 模型按 code 找，各库 id 不一样（测试库 14，Java 说线上 181）。找不到，下面的 variant / component 不会建。
SET @img_model := (SELECT id FROM afv_ai_model WHERE code = 'creaft-image-2' AND deleted = 0 ORDER BY id LIMIT 1);
SELECT @img_model AS `creaft-image-2 的模型id（NULL=不存在，先处理再跑）`;
-- 模型缺失时下面的场景 / 档位 / 步骤一律不建（不留 ENABLED 的空壳 offer；入口无 offer 则准入拒绝 = 关闭），并出告警行。
SELECT '⚠️ creaft-image-2 模型缺失：studio 三个入口均未建，先补模型再重跑本脚本' AS `告警` WHERE @img_model IS NULL;

-- 占位：步骤上的 base_points 只在「查不到单价行」时兜底，默认 0 = 不兜底（查不到就让检查报错，不让用户白嫖）。
-- ⚠️ 待产品定价；不要为了让检查变绿随手填个数。
SET @studio_base_points := 0;

-- ── 1. offer / variant / component ───────────────────────────────────────────
-- 场景（offer_code, 名称, 入口码, 执行面任务号, 排序）。
-- mode 是 NOT NULL 的整型（AIGC 模式码），studio 走 ai-gateway，没有对应模式码，先填 0
-- （与 pricing-update-2026-10-20 的 gen_ai_cutout 一致）；price() 不用它算价，只是存档。待 Java 确认。
DROP TEMPORARY TABLE IF EXISTS tmp_studio;
CREATE TEMPORARY TABLE tmp_studio (offer_code VARCHAR(64), name VARCHAR(64), entry_code VARCHAR(64), mode INT, sort INT) DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;
INSERT INTO tmp_studio VALUES
  ('studio_tryon',    '商拍换装·网页版', 'studio_tryon',    0, 0),
  ('studio_pose',     '姿态复刻·网页版', 'studio_pose',     0, 0),
  ('studio_imagegen', '图片生成·网页版', 'studio_imagegen', 0, 0);

-- 1.1 场景。能力码复用 ecom_image_gen（同图文笔记 / 海报 / 长图）。
INSERT INTO afv_billing_offer (offer_code, name, category, entry_code, feature_code, billing_scene, status, sort, deleted)
SELECT t.offer_code, t.name, 'IMAGE', t.entry_code, 'ecom_image_gen', 'IMAGE_GEN', 'ENABLED', t.sort, 0
FROM tmp_studio t
WHERE @img_model IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer o
                   WHERE o.offer_code = t.offer_code COLLATE utf8mb4_unicode_ci
                      OR (o.entry_code = t.entry_code COLLATE utf8mb4_unicode_ci AND o.deleted = 0));

-- 1.2 档位：每个场景一档（standard，默认模型 creaft-image-2）。
-- 每张图一张预留单（Count=1），所以 settlement_type 用 PARTIAL 与 ALL_OR_NOTHING 结果相同；
-- 沿用图片三场景的 PARTIAL。「部分失败只退失败张」靠的是一张图一单，不靠这个字段。
-- default_resolution 不填：分辨率由提交带的 resolution（1k/2k/4k）决定，查不到精确行时退回「不限分辨率」行。
INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, included_outputs, settlement_type, status, deleted)
SELECT CONCAT(t.offer_code, '_standard'), t.offer_code, '标准版', 'standard', @img_model, 1, 'PARTIAL', 'ENABLED', 0
FROM tmp_studio t
WHERE @img_model IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_standard') COLLATE utf8mb4_unicode_ci);

-- 1.3 计费步骤：按张（OUTPUT / PER_IMAGE），action_type 填 CREAFT_V1 才能命中 creaft-image-2 现成的单价行
-- （它没有 IMAGE_GEN 行；见 docs/billing/计费入口码-后台填写指引.md 卡点 2）。倍率 1.0。
INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT CONCAT('gen_', t.offer_code), CONCAT(t.offer_code, '_standard'), t.offer_code, 'OUTPUT', t.mode, 'BILLED', 'CREAFT_V1', @img_model, @studio_base_points, 1.000, 1, 0
FROM tmp_studio t
WHERE @img_model IS NOT NULL
  AND EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = CONCAT(t.offer_code, '_standard') COLLATE utf8mb4_unicode_ci AND v.deleted = 0)
  AND NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = CONCAT('gen_', t.offer_code) COLLATE utf8mb4_unicode_ci);

-- ── 2. 复核（只读）────────────────────────────────────────────────────────────
-- ② 三个入口 → 场景 → 档位 → 步骤 → 单价。每个入口应至少有一行带单价；
--    「✗ 没建场景 / 没建档位 / 没建步骤 / 没有单价」就是没配好。1k / 2k / 4k 各一行，是分辨率分档的价。
SELECT x.entry_code                                   AS `入口码`,
       COALESCE(o.offer_code, '✗ 没建场景')            AS `场景编码`,
       COALESCE(o.name, '-')                          AS `场景名称`,
       COALESCE(o.feature_code, '-')                  AS `能力码`,
       COALESCE(v.variant_code, '✗ 没建档位')          AS `档位`,
       COALESCE(v.settlement_type, '-')               AS `结算方式`,
       COALESCE(c.component_code, '✗ 没建步骤')        AS `步骤`,
       COALESCE(c.component_type, '-')                AS `计量`,
       COALESCE(c.action_type, '-')                   AS `动作`,
       COALESCE(m.code, '-')                          AS `模型`,
       COALESCE(p.price_points, -1)                   AS `单价（占位，待产品定价）`,
       COALESCE(p.price_unit, '✗ 没有单价')            AS `单位`,
       COALESCE(p.resolution, 'ANY')                  AS `分辨率`
FROM (SELECT 'studio_tryon' AS entry_code UNION ALL SELECT 'studio_pose' UNION ALL SELECT 'studio_imagegen') x
LEFT JOIN afv_billing_offer o ON o.entry_code = x.entry_code AND o.deleted = 0 AND o.status = 'ENABLED'
LEFT JOIN afv_billing_offer_variant v ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
LEFT JOIN afv_billing_charge_component c ON c.variant_code = v.variant_code AND c.deleted = 0 AND c.cost_type = 'BILLED'
LEFT JOIN afv_ai_model m ON m.id = COALESCE(c.model_id, v.default_model_id) AND m.deleted = 0
LEFT JOIN afv_model_pricing p ON p.model_id = COALESCE(c.model_id, v.default_model_id)
      AND p.action_type = c.action_type AND p.status = 1 AND p.deleted = 0
      AND (p.effective_time IS NULL OR p.effective_time <= NOW()) AND (p.expire_time IS NULL OR p.expire_time > NOW())
ORDER BY x.entry_code, v.variant_code, p.resolution;

-- ③ 能力码 ecom_image_gen 有没有人被发放过（同 check-billing-entitlement.sql；0 人 = 开权益会全员被挡）
SELECT o.entry_code AS `入口码`, o.feature_code AS `能力码`,
       COUNT(DISTINCT IF(f.status = 'ACTIVE', f.user_id, NULL)) AS `有效用户数`
FROM afv_billing_offer o
LEFT JOIN afv_app_user_feature f ON f.feature_code = o.feature_code AND f.deleted = 0
WHERE o.deleted = 0 AND o.entry_code IN ('studio_tryon', 'studio_pose', 'studio_imagegen')
GROUP BY o.entry_code, o.feature_code;

-- ④ 每档收费步骤数应恰好为 1（多于 1 会叠加扣费）
SELECT o.entry_code AS `入口码`, v.variant_code AS `档位`, SUM(c.cost_type = 'BILLED') AS `收费步骤数`
FROM afv_billing_offer o
JOIN afv_billing_offer_variant v ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
LEFT JOIN afv_billing_charge_component c ON c.variant_code = v.variant_code AND c.deleted = 0
WHERE o.deleted = 0 AND o.entry_code IN ('studio_tryon', 'studio_pose', 'studio_imagegen')
GROUP BY o.entry_code, v.variant_code
HAVING `收费步骤数` IS NULL OR `收费步骤数` <> 1;

-- ⑤ 产品定价后的做法（不在本脚本执行，按需选一）：
--   a) 沿用 creaft-image-2 现价：什么都不用做；之后想调，在后台「计价规则」改 creaft-image-2 的 CREAFT_V1 行
--      （注意连带影响图文笔记 / 海报 / 长图）。
--   b) studio 单独定价：在后台「模型管理」另建一个计价用模型（如 studio-image，仅作价格载体），
--      按 pricing-update-2026-10-20.sql 第 3、4 段的写法给它写 CREAFT_V1 的 1k / 2k / 4k（resolution 小写）行，
--      再把本脚本建的三个 variant 的 default_model_id 与 component 的 model_id 改成它：
--        UPDATE afv_billing_offer_variant SET default_model_id = <新模型id> WHERE variant_code IN
--          ('studio_tryon_standard', 'studio_pose_standard', 'studio_imagegen_standard');
--        UPDATE afv_billing_charge_component SET model_id = <新模型id> WHERE component_code IN
--          ('gen_studio_tryon', 'gen_studio_pose', 'gen_studio_imagegen');
--   c) 四档模型（通用 / GPT / Banana / Seedream）要各自定价：每档一个 variant（studio_*_gpt 之类，
--      default_model_id 各指各的价格模型），并在 pkg/billing/audit.go 的 Targets 给对应入口补 Tiers，
--      代码侧提交时带 Charge.Tier 选档（pickVariant：先找 <offer_code>_<tier> 再找 quality_tier）。
--      只按分辨率分价（同一模型 1k/2k/4k 不同价）不需要多个 variant，写在 afv_model_pricing 的 resolution 行即可。
