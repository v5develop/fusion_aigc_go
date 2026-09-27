-- 开权益之前必须跑这条：确认每个入口码挂的能力码**真的有人被发放过**。
-- 挂了一个没人有的码就等于把那个功能对所有人关掉。
SELECT
  o.entry_code                                   AS `入口码`,
  o.feature_code                                 AS `能力码`,
  COALESCE(g.union_cnt, 0)                       AS `联盟发放`,
  COALESCE(g.manual_cnt, 0)                      AS `手工发放`,
  COALESCE(g.users, 0)                           AS `有效用户数`,
  IF(COALESCE(g.users, 0) = 0, '✗ 没人有，开权益会全员被挡', '✓') AS `结论`
FROM afv_billing_offer o
LEFT JOIN (
  SELECT feature_code,
         SUM(source = 'UNION'  AND status = 'ACTIVE') AS union_cnt,
         SUM(source = 'MANUAL' AND status = 'ACTIVE') AS manual_cnt,
         COUNT(DISTINCT IF(status = 'ACTIVE', user_id, NULL)) AS users
    FROM afv_app_user_feature WHERE deleted = 0 GROUP BY feature_code
) g ON g.feature_code = o.feature_code
WHERE o.deleted = 0 AND o.entry_code IN
  ('imageTextNote','poster','longImage','exploreShop','shortDrama',
   'voiceover','digitalHuman','videoRemake','actionTransfer','aiCopywriting')
ORDER BY COALESCE(g.users, 0), o.entry_code;
