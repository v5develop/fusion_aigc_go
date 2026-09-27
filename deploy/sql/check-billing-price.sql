-- 按算价代码的同一套逻辑核单价：单价 × 秒数 × 倍率，和产品扣点表对。
-- 「结论」列直接给答案，全是 ✓ 就说明配置和扣点表一致了。
SELECT
  o.entry_code                       AS `入口码`,
  v.variant_code                     AS `档位`,
  m.code                             AS `模型`,
  p.price_points                     AS `单价`,
  p.price_unit                       AS `单位`,
  t.secs                             AS `按几秒算`,
  CEIL(p.price_points * t.secs * COALESCE(c.price_multiplier, 1)) AS `算出来`,
  t.expect                           AS `扣点表应为`,
  IF(CEIL(p.price_points * t.secs * COALESCE(c.price_multiplier, 1)) = t.expect,
     '✓', '✗ 对不上')                AS `结论`
FROM afv_billing_offer o
JOIN afv_billing_offer_variant v
  ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
JOIN afv_billing_charge_component c
  ON c.variant_code = v.variant_code AND c.deleted = 0 AND c.cost_type = 'BILLED'
JOIN afv_model_pricing p
  ON p.model_id = COALESCE(c.model_id, v.default_model_id)
 AND p.action_type = c.action_type AND p.status = 1 AND p.deleted = 0
JOIN afv_ai_model m
  ON m.id = COALESCE(c.model_id, v.default_model_id) AND m.deleted = 0
-- 各模块拿来核对的时长与产品扣点表上的应扣点数
JOIN (
  SELECT 'explore_shop_cloud'       AS vc, 15  AS secs, 165  AS expect UNION ALL
  SELECT 'explore_shop_lite',            15,       90            UNION ALL
  SELECT 'short_drama_cloud',            15,       165           UNION ALL
  SELECT 'short_drama_lite',             15,       90            UNION ALL
  SELECT 'voiceover_cloud',              15,       165           UNION ALL
  SELECT 'voiceover_lite',               15,       90            UNION ALL
  SELECT 'digital_human_cloud',          15,       165           UNION ALL
  SELECT 'digital_human_lite',           15,       90            UNION ALL
  SELECT 'video_remake_cloud',           180,      2160          UNION ALL
  SELECT 'video_remake_lite',            180,      1080          UNION ALL
  SELECT 'action_transfer_cloud',        15,       120           UNION ALL
  SELECT 'action_transfer_standard',     15,       90            UNION ALL
  SELECT 'action_transfer_lite',          15,       90
) t ON t.vc = v.variant_code
ORDER BY o.entry_code, v.variant_code;
