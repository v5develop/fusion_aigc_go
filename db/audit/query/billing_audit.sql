-- 计费配置检查（pkg/billing/audit.go，上线前自检；只读）。

-- name: BillingAuditModelGroups :many
-- 业务含义：某入口下所有已启用档位的默认模型的网关模型组（去重、排序）。分模型计价的入口按这些组逐个试算。
-- 读者：pkg/billing.Audit 的 auditGroups。
SELECT DISTINCT m.gateway_model_group
FROM afv_billing_offer o
JOIN afv_billing_offer_variant v ON v.offer_code = o.offer_code AND v.status = 'ENABLED' AND v.deleted = 0
JOIN afv_ai_model m ON m.id = v.default_model_id AND m.deleted = 0
WHERE o.entry_code = sqlc.arg(entry_code) AND o.status = 'ENABLED' AND o.deleted = 0
  AND m.gateway_model_group IS NOT NULL AND m.gateway_model_group <> ''
ORDER BY 1;

-- name: BillingAuditActiveFeatureCount :one
-- 业务含义：某创作能力码当前有效（ACTIVE 且未过期）的发放数；0 表示没人被发放过，开了准入等于对所有人关闭。
-- 读者：pkg/billing.Audit 的 auditOne。
SELECT COUNT(*) FROM afv_app_user_feature
WHERE feature_code = sqlc.arg(feature_code) AND status = 'ACTIVE' AND deleted = 0
  AND (expire_time IS NULL OR expire_time > NOW());
