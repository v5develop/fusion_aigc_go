-- ============================================================
-- 资产库增强后台（Go，/api/v1/asset-library/admin/*）的权限，与 Java 后台共用 RBAC 表。
--
-- asset_library:view    查看：标签字典、分类统计（按 kind / 来源计数）
-- asset_library:update  修改：预留给后续的官方分类 / 标签维护（当前后台接口都是只读）
-- super_admin 两个都给；operator 只给 view。写法与 Go_V1.0.22 studio 权限一致。
-- 只增不改：权限 / 资源 / 授权都用 NOT EXISTS 守卫，重复执行无副作用。
-- admin-api 的 AdminAuth 中间件按路由前缀 /api/v1/asset-library/admin 取这两个码（GET 要 view，其它要 update）。
-- ============================================================

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'asset_library:view' AS permission_code, '{"zh-CN": "查看资产库后台", "en-US": "View Asset Library Admin", "ja-JP": "アセットライブラリ管理表示", "ko-KR": "에셋 라이브러리 관리 조회"}' AS permission_name, 'asset_library' AS resource, 'view' AS action, '查看资产库的标签字典与分类统计' AS description
  UNION ALL SELECT 'asset_library:update', '{"zh-CN": "修改资产库后台", "en-US": "Update Asset Library Admin", "ja-JP": "アセットライブラリ管理更新", "ko-KR": "에셋 라이브러리 관리 수정"}', 'asset_library', 'update', '维护资产库分类与标签'
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_resource` (`resource_code`, `resource_name`, `description`, `sort_order`, `status`)
SELECT 'asset_library', '{"zh-CN": "资产库", "en-US": "Asset Library", "ja-JP": "アセットライブラリ", "ko-KR": "에셋 라이브러리"}',
       '资产库增强（Go assetlib 服务）的后台：标签字典、分类统计', 0, 1
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_resource` r WHERE r.`resource_code` = 'asset_library');

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code IN ('asset_library:view', 'asset_library:update')
WHERE r.role_code = 'super_admin'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'asset_library:view'
WHERE r.role_code = 'operator'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
