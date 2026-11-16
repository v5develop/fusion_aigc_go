-- ============================================================
-- 视频做同款总开关后台权限
--
-- 后台「做同款任务」页可切换 afv_system_config.replicate.enabled（数据库 > 文件配置；关着只拦新提交，历史 / 在途任务照常）。
--   GET    /api/v1/replicate/admin/switch  查看：沿用 replicate:view
--   PUT    /api/v1/replicate/admin/switch  写库：replicate:switch
--   DELETE /api/v1/replicate/admin/switch  恢复跟随部署配置：replicate:switch
-- 暂停 / 恢复影响全站新提交，与退点 / 强制收尾同级，只授 super_admin。
--
-- 编号：Go_V1.0.50（deploy/sql 最高 Go_V1.0.49）。幂等：WHERE NOT EXISTS，可重复执行。不建表、不改 afv_system_config 结构。
-- ============================================================
SET NAMES utf8mb4;

INSERT INTO `afv_admin_permission` (`permission_code`, `permission_name`, `resource`, `action`, `description`)
SELECT t.* FROM (
  SELECT 'replicate:switch' AS permission_code, '{"zh-CN": "视频做同款总开关", "en-US": "Toggle Video Replicate", "ja-JP": "動画リプリケート全体スイッチ", "ko-KR": "영상 따라 만들기 전체 스위치"}' AS permission_name, 'replicate' AS resource, 'switch' AS action, '暂停 / 恢复视频做同款新提交（写 afv_system_config.replicate.enabled，原因必填）' AS description
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_admin_permission` p WHERE p.`permission_code` = t.permission_code);

INSERT INTO `afv_admin_role_permission` (`role_id`, `permission_id`)
SELECT r.id, p.id
FROM `afv_admin_role` r
JOIN `afv_admin_permission` p ON p.permission_code = 'replicate:switch'
WHERE r.role_code = 'super_admin'
  AND NOT EXISTS (SELECT 1 FROM `afv_admin_role_permission` rp WHERE rp.role_id = r.id AND rp.permission_id = p.id);
