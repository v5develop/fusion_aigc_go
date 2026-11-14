-- ============================================================
-- 广场模板域后台（Go，/api/v1/plaza/admin/*）的细粒度权限，与 Java 后台共用 RBAC 表。
-- 其余读写复用 Java 已有的 creaft_catalog:view / creaft_catalog:edit（角色已授权），本文件只加三个更高风险的码：
--   plaza:publish  发布 / 下线 / 定时上下线（super_admin、operator）
--   plaza:delete   删除 / 恢复 / 彻底删除（super_admin）
--   plaza:audit    查看操作记录（super_admin）
-- 写法与 Go_V1.0.26 一致：权限 / 资源 / 授权都用 NOT EXISTS 守卫，重复执行无副作用。
-- 版本号 35 是头条迁移（Go_V1.0.35__plaza_headline.sql）。
-- ============================================================

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'plaza:publish' AS permission_code, '{"zh-CN": "发布广场模板", "en-US": "Publish Plaza Template", "ja-JP": "テンプレート公開", "ko-KR": "템플릿 게시"}' AS permission_name, 'plaza' AS resource, 'publish' AS action, '发布 / 下线 / 定时上下线广场模板' AS description
  UNION ALL SELECT 'plaza:delete', '{"zh-CN": "删除广场模板", "en-US": "Delete Plaza Template", "ja-JP": "テンプレート削除", "ko-KR": "템플릿 삭제"}', 'plaza', 'delete', '删除 / 恢复 / 彻底删除广场模板'
  UNION ALL SELECT 'plaza:audit', '{"zh-CN": "查看广场操作记录", "en-US": "View Plaza Audit", "ja-JP": "操作履歴表示", "ko-KR": "작업 기록 조회"}', 'plaza', 'audit', '查看广场模板、行业、头条的操作记录（含提示词全文）'
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_resource` (`resource_code`, `resource_name`, `description`, `sort_order`, `status`)
SELECT 'plaza', '{"zh-CN": "广场模板与行业", "en-US": "Plaza Templates & Industries", "ja-JP": "テンプレートと業種", "ko-KR": "템플릿과 업종"}',
       '广场模板、行业目录与头条（Go plaza 域）的细粒度后台权限', 0, 1
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_resource` r WHERE r.`resource_code` = 'plaza');

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code IN ('plaza:publish', 'plaza:delete', 'plaza:audit')
WHERE r.role_code = 'super_admin'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'plaza:publish'
WHERE r.role_code = 'operator'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
