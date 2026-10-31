-- ============================================================
-- 画布 / 提示词库后台（Go，/api/v1/canvas/admin/* 与 /api/v1/prompt-library/admin/*）的权限，与 Java 后台共用 RBAC 表。
--
-- canvas:view    查看：节点执行（run）列表、成本汇总、画布模板、提示词库
-- canvas:update  修改：强制失败 run、维护官方模板 / 官方提示词（新增、编辑、上下架）
-- super_admin 两个都给；operator 只给 view。写法与 Go_V1.0.22 studio 权限一致。
-- 只增不改：权限 / 资源 / 授权都用 NOT EXISTS 守卫，重复执行无副作用。
-- ============================================================

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'canvas:view' AS permission_code, '{"zh-CN": "查看画布后台", "en-US": "View Canvas Admin", "ja-JP": "キャンバス管理表示", "ko-KR": "캔버스 관리 조회"}' AS permission_name, 'canvas' AS resource, 'view' AS action, '查看画布节点执行、成本汇总、画布模板与提示词库' AS description
  UNION ALL SELECT 'canvas:update', '{"zh-CN": "修改画布后台", "en-US": "Update Canvas Admin", "ja-JP": "キャンバス管理更新", "ko-KR": "캔버스 관리 수정"}', 'canvas', 'update', '强制失败节点执行；维护官方画布模板与官方提示词'
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_resource` (`resource_code`, `resource_name`, `description`, `sort_order`, `status`)
SELECT 'canvas', '{"zh-CN": "画布与提示词库", "en-US": "Canvas & Prompt Library", "ja-JP": "キャンバスとプロンプト", "ko-KR": "캔버스와 프롬프트"}',
       '画布（Go canvas 服务）的后台：节点执行、模板、提示词库', 0, 1
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_resource` r WHERE r.`resource_code` = 'canvas');

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code IN ('canvas:view', 'canvas:update')
WHERE r.role_code = 'super_admin'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'canvas:view'
WHERE r.role_code = 'operator'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
