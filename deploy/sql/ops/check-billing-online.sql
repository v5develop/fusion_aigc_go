-- 线上库计费配置核对（只读）。给 Java 在**线上库**执行，把四段结果贴回来。
-- 对照测试库 2026-10-16 查出的问题：动作迁移档位名仍是 standard、cloud 档无扣费步骤；
-- 爆款复刻 lite 档无步骤、cloud 档多挂了 lite 步骤（会扣双份）；图文笔记/海报/长图没有场景。

-- ① 本地生活 9 个入口有没有场景（应有 9 行；缺的就是没建）
SELECT e.entry_code AS `入口码`, o.offer_code AS `场景`, o.feature_code AS `能力码`, o.status AS `状态`
FROM (SELECT 'imageTextNote' entry_code UNION SELECT 'poster' UNION SELECT 'longImage'
      UNION SELECT 'digitalHuman' UNION SELECT 'voiceover' UNION SELECT 'actionTransfer'
      UNION SELECT 'exploreShop' UNION SELECT 'shortDrama' UNION SELECT 'videoRemake') e
LEFT JOIN afv_billing_offer o ON o.entry_code = e.entry_code AND o.deleted = 0
ORDER BY e.entry_code;

-- ② 每个档位的计费步骤与单价（视频入口应各有 <offer>_cloud / <offer>_lite 两档，每档只有 1 个收费步骤）
SELECT o.entry_code AS `入口码`, v.variant_code AS `档位`, v.default_model_id AS `默认模型`,
       c.component_code AS `步骤`, c.cost_type AS `收费类型`, c.action_type AS `动作`,
       c.model_id AS `步骤模型`, p.price_points AS `单价`, p.price_unit AS `单位`, p.resolution AS `分辨率`
FROM afv_billing_offer o
JOIN afv_billing_offer_variant v ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
LEFT JOIN afv_billing_charge_component c ON c.variant_code = v.variant_code AND c.deleted = 0
LEFT JOIN afv_model_pricing p ON p.model_id = COALESCE(c.model_id, v.default_model_id)
       AND p.action_type = c.action_type AND p.status = 1 AND p.deleted = 0
WHERE o.deleted = 0 AND o.entry_code IN ('imageTextNote','poster','longImage','digitalHuman','voiceover',
      'actionTransfer','exploreShop','shortDrama','videoRemake')
ORDER BY o.entry_code, v.variant_code, c.sort, c.id;

-- ③ 有问题的档位：没有收费步骤 / 收费步骤多于 1 个 / 云端档里有 lite 步骤 / lite 档里有云端步骤
SELECT o.entry_code AS `入口码`, v.variant_code AS `档位`,
       SUM(c.cost_type = 'BILLED') AS `收费步骤数`,
       GROUP_CONCAT(c.component_code SEPARATOR ' + ') AS `步骤`,
       CASE WHEN SUM(c.cost_type = 'BILLED') IS NULL OR SUM(c.cost_type = 'BILLED') = 0 THEN '没有收费步骤（选这档不扣钱）'
            WHEN SUM(c.cost_type = 'BILLED') > 1 THEN '多个收费步骤（会叠加扣费，确认是否有意）'
            WHEN v.variant_code LIKE '%\_cloud' AND GROUP_CONCAT(c.component_code) LIKE '%lite%' THEN '云端档挂了 lite 步骤'
            WHEN v.variant_code LIKE '%\_lite' AND GROUP_CONCAT(c.component_code) LIKE '%cloud%' THEN 'lite 档挂了云端步骤'
            ELSE NULL END AS `问题`
FROM afv_billing_offer o
JOIN afv_billing_offer_variant v ON v.offer_code = o.offer_code AND v.deleted = 0 AND v.status = 'ENABLED'
LEFT JOIN afv_billing_charge_component c ON c.variant_code = v.variant_code AND c.deleted = 0
WHERE o.deleted = 0 AND o.entry_code IN ('imageTextNote','poster','longImage','digitalHuman','voiceover',
      'actionTransfer','exploreShop','shortDrama','videoRemake')
GROUP BY o.entry_code, v.variant_code
HAVING `问题` IS NOT NULL
ORDER BY o.entry_code, v.variant_code;

-- ④ 图片模型 creaft-image-2 及其单价（按 code 查；各库 id 不一样，测试库是 14，Java 说线上是 181。要有 CREAFT_V1 行，单价 > 0）
SELECT m.id, m.code, m.name, p.action_type, p.price_points, p.price_unit, p.resolution, p.status
FROM afv_ai_model m LEFT JOIN afv_model_pricing p ON p.model_id = m.id AND p.deleted = 0
WHERE m.id = 181 OR m.code = 'creaft-image-2';
