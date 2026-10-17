-- ============================================================
-- 本地生活后台（Go，/api/v1/admin/local-life/*）的权限，与 Java 后台共用 RBAC 表。
--
-- local_life:view    查看：文案 / 数字人 / 笔记 / 出图出片的列表、选项、模型配置
-- local_life:update  修改：增删改选项、音色、数字人、删除记录等所有写操作
-- super_admin 两个都给；operator 只给 view。与 Java 的 runtime_settings 权限同一写法。
-- 只增不改：权限 / 资源 / 授权都用 NOT EXISTS 守卫，重复执行无副作用。
-- ============================================================

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'local_life:view' AS permission_code, '{"zh-CN": "查看本地生活后台", "en-US": "View Local Life Admin", "ja-JP": "ローカルライフ管理表示", "ko-KR": "로컬 라이프 관리 조회"}' AS permission_name, 'local_life' AS resource, 'view' AS action, '查看本地生活（文案、数字人、笔记、出图出片）的后台数据与配置' AS description
  UNION ALL SELECT 'local_life:update', '{"zh-CN": "修改本地生活后台", "en-US": "Update Local Life Admin", "ja-JP": "ローカルライフ管理更新", "ko-KR": "로컬 라이프 관리 수정"}', 'local_life', 'update', '增删改本地生活的选项、音色、数字人，以及删除生成记录'
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_resource` (`resource_code`, `resource_name`, `description`, `sort_order`, `status`)
SELECT 'local_life', '{"zh-CN": "本地生活", "en-US": "Local Life", "ja-JP": "ローカルライフ", "ko-KR": "로컬 라이프"}',
       '本地生活（Go 服务）的后台：文案、数字人、图文笔记、海报 / 长图 / 视频', 0, 1
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_resource` r WHERE r.`resource_code` = 'local_life');

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code IN ('local_life:view', 'local_life:update')
WHERE r.role_code = 'super_admin'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'local_life:view'
WHERE r.role_code = 'operator'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
