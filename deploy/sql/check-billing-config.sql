SELECT
  x.entry_code                                   AS `入口码`,
  COALESCE(o.offer_code, '✗ 没建场景')            AS `场景编码`,
  COALESCE(o.feature_code, '-')                  AS `能力码`,
  COALESCE(v.variant_code, '✗ 没建档位')          AS `档位`,
  COALESCE(m.code, '-')                          AS `模型`,
  COALESCE(v.settlement_type, '-')               AS `结算方式`,
  COALESCE(c.component_type, '✗ 没建步骤')        AS `计量`,
  COALESCE(c.action_type, '-')                   AS `动作`,
  COALESCE(p.price_points, -1)                   AS `单价`,
  COALESCE(p.price_unit, '✗ 没有单价')            AS `单位`,
  COALESCE(p.resolution, 'ANY')                  AS `分辨率`
FROM (
  SELECT 'imageTextNote' AS entry_code UNION ALL SELECT 'poster'        UNION ALL
  SELECT 'longImage'                   UNION ALL SELECT 'exploreShop'   UNION ALL
  SELECT 'shortDrama'                  UNION ALL SELECT 'voiceover'     UNION ALL
  SELECT 'digitalHuman'                UNION ALL SELECT 'videoRemake'   UNION ALL
  SELECT 'actionTransfer'
) x
LEFT JOIN afv_billing_offer o
       ON o.entry_code = x.entry_code AND o.deleted = 0 AND o.status = 'ENABLED'
LEFT JOIN afv_billing_offer_variant v
       ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
LEFT JOIN afv_billing_charge_component c
       ON c.variant_code = v.variant_code AND c.deleted = 0 AND c.cost_type = 'BILLED'
LEFT JOIN afv_ai_model m
       ON m.id = COALESCE(c.model_id, v.default_model_id) AND m.deleted = 0
LEFT JOIN afv_model_pricing p
       ON p.model_id = COALESCE(c.model_id, v.default_model_id)
      AND p.action_type = c.action_type AND p.status = 1 AND p.deleted = 0
ORDER BY x.entry_code, v.variant_code, p.resolution;
