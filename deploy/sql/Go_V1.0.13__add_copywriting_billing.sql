-- 文案计费方案：入口码 aiCopywriting，按次 1 点（计费方案-扣点与入口码.md：AI文案 1 点/次）。
-- 由 install.sh 的 migrate 步骤执行（先整库备份）。可重复执行（每段都有 NOT EXISTS）。
--
-- ⚠️ 与历史文档冲突：docs/billing 里 2026-10-11 的记录写的是「AI 文案不计费、不建 offer」。
--    2026-10-17 的新决定是接入计费，以此为准；如产品要撤回，删掉这三段 INSERT 并把 BILLING_MODE 相关的文案计费关掉。
--
-- 只记 offer / 档位 / 计费步骤，不改任何已有入口的配置；category、billing_scene 留空，
-- 不让 Java 的分类与场景逻辑把它当成图片或视频。

INSERT INTO afv_billing_offer (offer_code, name, category, entry_code, feature_code, billing_scene, status, sort, deleted)
SELECT 'copywriting', 'AI文案', NULL, 'aiCopywriting', 'locallife_text_gen', NULL, 'ENABLED', 90, 0
FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_offer o
                  WHERE o.offer_code = 'copywriting' OR (o.entry_code = 'aiCopywriting' AND o.deleted = 0));

INSERT INTO afv_billing_offer_variant (variant_code, offer_code, name, quality_tier, default_model_id, included_outputs, settlement_type, status, deleted)
SELECT 'ai_copywriting_std', 'copywriting', '标准版', 'standard', NULL, 1, 'ALL_OR_NOTHING', 'ENABLED', 0
FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_offer_variant v WHERE v.variant_code = 'ai_copywriting_std');

-- mode = 0：非 AIGC 执行面任务（文案走 Go 自己的模型调用，不提交执行面）。固定 1 点，不挂模型单价。
INSERT INTO afv_billing_charge_component (component_code, variant_code, step_code, component_type, mode, cost_type, action_type, model_id, base_points, price_multiplier, sort, deleted)
SELECT 'gen_copywriting', 'ai_copywriting_std', 'copywriting', 'OUTPUT', 0, 'BILLED', '', NULL, 1, 1.000, 1, 0
FROM DUAL
WHERE NOT EXISTS (SELECT 1 FROM afv_billing_charge_component c WHERE c.component_code = 'gen_copywriting');
