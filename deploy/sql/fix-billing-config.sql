-- 修计费配置：把挂错档位的计费步骤搬回自己的档位，删掉旧的/坏的。
--
-- 问题：lite 的步骤被挂到了云端档上，导致云端档两个步骤相加（翻倍扣钱），
--       而 lite 档一个步骤都没有（算不出价）。6 个档位都是同一个错。
-- 步骤名里的数字是执行面任务号，据此判断该归哪档：
--       云端 85/81/89/92/71/86    lite 45/73/88/91/83/9
--
-- ⚠️ 改的是配置表，跑之前先备份这张表：
--    CREATE TABLE afv_billing_charge_component_bak_0927 AS SELECT * FROM afv_billing_charge_component;

-- 1) lite 的步骤搬回 lite 档
UPDATE afv_billing_charge_component SET variant_code = 'digital_human_lite'
 WHERE component_code = 'gen_digital_human_lite_45' AND deleted = 0;
UPDATE afv_billing_charge_component SET variant_code = 'short_drama_lite'
 WHERE component_code = 'gen_short_drama_lite_88' AND deleted = 0;
UPDATE afv_billing_charge_component SET variant_code = 'video_remake_lite'
 WHERE component_code = 'gen_video_remake_lite_83' AND deleted = 0;
UPDATE afv_billing_charge_component SET variant_code = 'voiceover_lite'
 WHERE component_code = 'gen_voiceover_lite_91' AND deleted = 0;
UPDATE afv_billing_charge_component SET variant_code = 'explore_shop_lite'
 WHERE component_code = 'gen_explore_shop_lite_73' AND deleted = 0;

-- 2) 云端的步骤搬回云端档
UPDATE afv_billing_charge_component SET variant_code = 'explore_shop_cloud'
 WHERE component_code = 'gen_explore_shop_81' AND deleted = 0;
UPDATE afv_billing_charge_component SET variant_code = 'action_transfer_cloud'
 WHERE component_code = 'gen_action_transfer_86' AND deleted = 0;

-- 3) 删掉旧的/坏的步骤
--    gen_action_transfer  旧的，OUTPUT 计量、单价 1~2，和新的 _86 会相加
--    gen_explore_shop     旧的，没带任务号
--    gen_explore_shop_lite 旧的，而且是 BASE/无动作，没单价，算价会直接报错
UPDATE afv_billing_charge_component SET deleted = 1
 WHERE component_code IN ('gen_action_transfer', 'gen_explore_shop', 'gen_explore_shop_lite')
   AND deleted = 0;

-- 4) 还缺一个：动作迁移的 lite 步骤（执行面任务号 9）没建过。
--    档位 action_transfer_standard 就是 lite 档（默认模型 xiaoqi-video-1.0-lite-b6），
--    照 gen_explore_shop_lite_73 的样子补一个 DURATION / VIDEO_GEN 的步骤。
--    在后台页面加，或者把下面这条的注释去掉：
-- INSERT INTO afv_billing_charge_component
--   (component_code, variant_code, step_code, component_type, mode, cost_type,
--    action_type, model_id, base_points, price_multiplier, sort, deleted)
-- VALUES ('gen_action_transfer_lite_9', 'action_transfer_standard', 'video_gen',
--    'DURATION', 9, 'BILLED', 'VIDEO_GEN', NULL, 0, 1.000, 0, 0);

-- 5) 改完自查：每个档位的「计费步骤数」都应该是 1
--    再跑一遍 deploy/sql/check-billing-detail.sql
