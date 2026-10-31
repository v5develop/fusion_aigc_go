-- ============================================================
-- 网页创作 studio 后台（Go，/api/v1/studio/admin/*）的权限，与 Java 后台共用 RBAC 表。
--
-- studio:view    查看：商拍换装 / 姿态复刻 / 图片生成 的批次、任务、成本汇总、依赖（反推 sidecar / ai-gateway / media）状态
-- studio:update  修改：重做、强制失败子任务
-- super_admin 两个都给；operator 只给 view。写法与 Go_V1.0.14 本地生活权限一致。
-- 只增不改：权限 / 资源 / 授权都用 NOT EXISTS 守卫，重复执行无副作用。
-- 对应管理前端 fusion-aigc-admin 的 src/utils/studio-permission.ts。
-- ============================================================

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'studio:view' AS permission_code, '{"zh-CN": "查看网页创作后台", "en-US": "View Studio Admin", "ja-JP": "ウェブ制作管理表示", "ko-KR": "웹 제작 관리 조회"}' AS permission_name, 'studio' AS resource, 'view' AS action, '查看网页创作（商拍换装 / 姿态复刻 / 图片生成）的批次、任务、成本与依赖状态' AS description
  UNION ALL SELECT 'studio:update', '{"zh-CN": "修改网页创作后台", "en-US": "Update Studio Admin", "ja-JP": "ウェブ制作管理更新", "ko-KR": "웹 제작 관리 수정"}', 'studio', 'update', '重做 / 强制失败网页创作子任务'
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_resource` (`resource_code`, `resource_name`, `description`, `sort_order`, `status`)
SELECT 'studio', '{"zh-CN": "网页创作", "en-US": "Web Studio", "ja-JP": "ウェブ制作", "ko-KR": "웹 제작"}',
       '网页创作（Go studio 服务）的后台：商拍换装、姿态复刻、图片生成', 0, 1
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_resource` r WHERE r.`resource_code` = 'studio');

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code IN ('studio:view', 'studio:update')
WHERE r.role_code = 'super_admin'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'studio:view'
WHERE r.role_code = 'operator'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
