-- 逐档位列出计费步骤和单价行数，用来分辨"一个步骤配了多条单价"还是"配了多个步骤"。
-- 后者会叠加扣钱（两个步骤 11+11 = 22/秒）。
SELECT
  o.entry_code                                AS `入口码`,
  v.variant_code                              AS `档位`,
  COUNT(DISTINCT c.id)                        AS `计费步骤数`,
  GROUP_CONCAT(DISTINCT CONCAT(
      c.component_code, '[', COALESCE(c.component_type,'?'),
      '/', COALESCE(c.action_type,'无动作'),
      '/base=', COALESCE(c.base_points,0),
      '/x', COALESCE(c.price_multiplier,1),
      ']') ORDER BY c.sort, c.id SEPARATOR '  +  ') AS `步骤明细`,
  (SELECT COUNT(*) FROM afv_model_pricing p
     WHERE p.model_id = v.default_model_id AND p.status = 1 AND p.deleted = 0)
                                              AS `该模型单价行数`
FROM afv_billing_offer o
JOIN afv_billing_offer_variant v
  ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
LEFT JOIN afv_billing_charge_component c
  ON c.variant_code = v.variant_code AND c.deleted = 0 AND c.cost_type = 'BILLED'
WHERE o.deleted = 0 AND o.entry_code IN
  ('imageTextNote','poster','longImage','exploreShop','shortDrama',
   'voiceover','digitalHuman','videoRemake','actionTransfer')
GROUP BY o.entry_code, v.variant_code
ORDER BY o.entry_code, v.variant_code;
