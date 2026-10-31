-- Java 主项目 creaft v1 兼容面：小岐 AI 期 0 配置（2026-10-31）
--
-- 所属库：Java 主项目库（fusion_aigc，表 afv_system_config / afv_compat_workflow_template /
--   afv_compat_core_capability_binding）。不是 Go 自己的库。
-- 依据：docs/design/xiaoqi-2026-10-30/00-需求分流与设计方案.md §2.1/§2.2/§8 期 0；08-Java主项目能力盘点.md「校验位置」。
-- 代码侧配套：Java 提交 494818fd（CreaftV2Contract 提示词上限 2000 -> 3000：场景生成 content、单图复制 / SKU 替换
--   prompt，即 wf:13 visual-style-replace、wf:14 visual-sku-replace），随 Java 发版生效；与本脚本无依赖，
--   前端放开 9 张前两边都要到位。
--
-- 本脚本分节：
--   0. 写：备份当前行（只增不覆盖）
--   1. 写：compat.v1.max_image_count 设为 9（可重放）
--   2. 读：复核 max_image_count 生效值
--   3. 读：wf:13 / wf:14 模板是否启用、payload 里 max_num 现值
--   4. 读：15 个工具 + 2 个工作流的能力绑定启用状态与 model_roles
--   2~4 只读，可随时重复执行。文末「回退」默认注释。
--
-- 执行方式：先用生产账号在测试库跑一遍（确认 CREATE TABLE … LIKE / INSERT … SELECT 权限）；生产遇错即停：
--   客户端支持时 `mysql --abort-source-on-error` 再 `source <文件>`（MySQL 8.0 客户端没有该选项），
--   通用做法是 `mysql … < 本文件`，不加 --force（批处理遇第一个错误即退出）。
--
-- ============================================================================
-- ⚠️ 第 1 节是「全局值」
-- ============================================================================
-- compat.v1.max_image_count = creaft v1 兼容面「单次最多携带参考图张数」，全站一个值（没有按等级区分）。
-- 同时影响：智能体对话 / 工具 / 工作流 / 帮写 / 批量托管每个任务 / 品牌档案每个图组。
-- 也就是说改成 9 之后，Agent、帮写、档案图组的上限也同步变成 9，不只是小岐 v1.0 的图片生成。
-- 各模型自己的参考图上限（model-presets maxReferenceImages）仍按网关能力另判，是第二道，不受此值影响。
-- 回退：见文末「回退」——执行前已有该行则从备份表还原原值与 remark；原本没有该行则 DELETE（恢复配置文件值，
--   app.compat.v1.max-image-count 默认 4）。没有原行时也可后台点「恢复默认」。
--
-- 等价的后台操作（推荐，会审计 + 广播让各副本即时失效缓存）：
--   管理台 → 系统 →「运行时设置」（权限 runtime_settings:view / runtime_settings:update）
--     → 分组「creaft 兼容」→「单次最多上传图片数」（key compat.v1.max_image_count）→ 改 9 → 保存。
--   等价接口：PUT /api/v1/admin/system/runtime-settings/compat.v1.max_image_count  body {"value":"9"}
--             DELETE 同路径 = 恢复配置文件值。
-- 直接跑本脚本不会经过「失效广播」：各副本本地缓存 TTL 5 秒（RuntimeSettings.LOCAL_TTL），5 秒内自然生效；
--   但不会写后台审计日志。要审计就走后台。
-- 校验：RuntimeSettingValidator 要求整数且 >= 1。
--
-- ============================================================================
-- 关于工作流模板 max_num（本脚本不改模板数据）
-- ============================================================================
-- max_num 是模板 payload.image_urls 里每个输入槽位的「可传张数」，运营配置在
--   afv_compat_workflow_template.payload（JSON）。v1.0 范围只复核 wf:13 / wf:14。
--   种子（Java V1.0.170）里两者都只有 image_urls 两个槽位：wf:13 主体图 1 + 参考图 3，wf:14 主体图 1 + 参考图 20。
-- 服装类 wf:5 fashion-model-tryon、wf:8 fashion-model-cloth-fit 的 max_num 1 -> 4 不在 v1.0；将来要改时：
--   后台 AdminCreaftWorkflowTemplateController（管理台「Creaft 工作流模板」）编辑该模板 payload，
--   把对应槽位的 "max_num":1 改成 4 并保存；或者先备份再 UPDATE ... SET payload = JSON_SET(payload, '$.payload.image_urls[i].max_num', 4)
--   （下标 i 先用第 3 节的查询确认，不要整体 REPLACE 字符串，payload 里 "max_num":1 出现很多次）。
--   注意槽位数受上面的 max_image_count 约束：1 + N 不得超过账号上限。
--
-- ============================================================================
-- 能力绑定部署前核对
-- ============================================================================
-- 启用准入由 CompatBindingAdmission 判：计费可装配、model_roles 非空、工作流必填角色齐全、
--   每个模型组在 AI 网关就绪（后者要问网关，SQL 判不了）。
-- 部署前请做两件事：
--   a) 管理台「Creaft 能力绑定」页逐行看 enabled 与每个角色的模型组就绪度；
--   b) 看 Java 的健康检查（CompatBindingAdmissionHealthIndicator，actuator health 里 compat binding 一项）无告警。
-- 第 4 节只给「库里现状」，通过与否以准入检查为准。启用请走后台页逐条点（同事务跑完整准入），
--   不要用 UPDATE 批量打开。
-- 工作流 wf:13 / wf:14 共用路由 /workflows/visual（workflow_code = workflows.visual），按 title 区分模板。
-- ============================================================================

SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- 0. 备份当前行（结构同原表；config_key 唯一键 + INSERT IGNORE：重放不覆盖第一次的原值；
--    执行前没有该行时备份表为空，回退据此判断走 DELETE）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS afv_system_config_bak_20261031_maximg LIKE afv_system_config;
INSERT IGNORE INTO afv_system_config_bak_20261031_maximg
SELECT * FROM afv_system_config WHERE config_key = 'compat.v1.max_image_count';

-- ---------------------------------------------------------------------------
-- 1. 写：max_image_count = 9（没有则插入，有则更新；包含被软删除的行，config_key 有唯一键）
-- ---------------------------------------------------------------------------
INSERT INTO afv_system_config (config_key, config_value, remark, deleted)
VALUES ('compat.v1.max_image_count', '9',
        '小岐AI期0：单次最多上传图片数 4->9（全局：同时影响 Agent/帮写/档案图组）', 0)
ON DUPLICATE KEY UPDATE config_value = VALUES(config_value),
                        remark       = VALUES(remark),
                        deleted      = 0;

-- ---------------------------------------------------------------------------
-- 2. 读：复核。期望 config_value = '9'、deleted = 0
--    （没有 DB 行时生效值是配置文件 app.compat.v1.max-image-count，默认 4；在后台「运行时设置」页看「生效值 / 来源」更直观）
-- ---------------------------------------------------------------------------
SELECT config_key, config_value, deleted, remark, update_time
  FROM afv_system_config
 WHERE config_key = 'compat.v1.max_image_count';

-- ---------------------------------------------------------------------------
-- 3. 读：wf:13 / wf:14 模板。期望 enabled = 1、payload 非空；
--    max_num_values 列出 payload 里全部 "max_num" 取值（顺序即槽位顺序，用来对照前端表单）。
--    wf:13 各槽位 max_num 之和应 <= 账号上限(9)（种子 1 + 3）；wf:14 产品 <= 20、num <= 4 在代码里（VISUAL_SKU_MAX_GROUPS）。
-- ---------------------------------------------------------------------------
SELECT t.id AS workflow_id, t.title, t.title_zh, t.enabled, t.deleted,
       (t.payload IS NOT NULL) AS has_payload,
       JSON_EXTRACT(t.payload, '$.payload.image_urls[*].max_num') AS image_urls_max_num,
       t.update_time
  FROM afv_compat_workflow_template t
 WHERE t.id IN (13, 14)
    OR t.title IN ('visual-style-replace', 'visual-sku-replace');

-- 兜底：payload 结构与上面路径不一致时，直接看原文里 max_num 出现的位置
SELECT t.id AS workflow_id, t.title,
       JSON_EXTRACT(t.payload, '$**.max_num') AS all_max_num
  FROM afv_compat_workflow_template t
 WHERE t.deleted = 0
   AND t.title IN ('visual-style-replace', 'visual-sku-replace');

-- 参考：服装类 wf:5 / wf:8 现值（v1.0 之后要改 1->4 时对照；本脚本不改）
SELECT t.id AS workflow_id, t.title, t.enabled,
       JSON_EXTRACT(t.payload, '$**.max_num') AS all_max_num
  FROM afv_compat_workflow_template t
 WHERE t.deleted = 0
   AND t.title IN ('fashion-model-tryon', 'fashion-model-cloth-fit');

-- ---------------------------------------------------------------------------
-- 4. 读：能力绑定。路由 -> 小岐工具（见 08-Java主项目能力盘点.md 映射表）
--    upscale            /tools/ai-image-upscaler
--    eraser             /tools/ai-erase
--    cutout             （同步 /tools/cutout 或 v2 异步；若无绑定行见第二段查询）
--    image-translate    /tools/ai-image-translate
--    text-replace       /tools/ai-text-replace（+ /tools/ocr 同步）
--    ratio-convert      /tools/ai-ratio-convert
--    image-generate     /tools/ai-image-generator
--    first-screen       /tools/ai-first-screen-main-image
--    background-gen     /tools/ai-background-generator
--    posture-fission    /tools/ai-posture-fission
--    product-retouch    /tools/ai-retouch
--    product-replace    /tools/ai-product-replace
--    cloth-wrinkle      /tools/ai-clothing-dewrinkle
--    digital-model      /tools/ai-model-tryon
--    video-enhance      /tools/ai-video-enhance
--    video-translate    /tools/ai-video-translate
--    video-generator    /tools/ai-video-generator
--    工作流 wf:13/14    /workflows/visual
--    （08 文件写「17 工具」，v1.0 取其中 15 个 + 2 个工作流；上面是全部候选路由，多出来的按 v1.0 范围忽略）
--    期望：enabled = 1、deleted = 0、model_code 非空、model_roles 非空（视频类另看 video_model_aliases）。
-- ---------------------------------------------------------------------------
SELECT b.creaft_v1_route AS route, b.display_name, b.workflow_code, b.entry_code, b.model_code,
       b.task_type, b.enabled, b.deleted, b.billing_policy_kind,
       b.model_roles, b.video_model_aliases, b.update_time
  FROM afv_compat_core_capability_binding b
 WHERE b.deleted = 0
   AND b.creaft_v1_route IN (
        '/tools/ai-image-upscaler', '/tools/ai-erase', '/tools/ai-image-translate',
        '/tools/ai-text-replace', '/tools/ocr', '/tools/ai-ratio-convert',
        '/tools/ai-image-generator', '/tools/ai-first-screen-main-image',
        '/tools/ai-background-generator', '/tools/ai-posture-fission',
        '/tools/ai-retouch', '/tools/ai-product-replace', '/tools/ai-clothing-dewrinkle',
        '/tools/ai-model-tryon', '/tools/ai-video-enhance', '/tools/ai-video-translate',
        '/tools/ai-video-generator', '/workflows/visual')
    OR (b.deleted = 0 AND b.creaft_v1_route LIKE '%cutout%')
 ORDER BY b.enabled, b.creaft_v1_route;

-- 4b. 缺口：上面候选路由里「库里根本没有绑定行」的（有行但 enabled=0 的在 4 里看）
SELECT r.route AS missing_route
  FROM (SELECT '/tools/ai-image-upscaler' AS route UNION ALL SELECT '/tools/ai-erase'
        UNION ALL SELECT '/tools/ai-image-translate' UNION ALL SELECT '/tools/ai-text-replace'
        UNION ALL SELECT '/tools/ai-ratio-convert' UNION ALL SELECT '/tools/ai-image-generator'
        UNION ALL SELECT '/tools/ai-first-screen-main-image' UNION ALL SELECT '/tools/ai-background-generator'
        UNION ALL SELECT '/tools/ai-posture-fission' UNION ALL SELECT '/tools/ai-retouch'
        UNION ALL SELECT '/tools/ai-product-replace' UNION ALL SELECT '/tools/ai-clothing-dewrinkle'
        UNION ALL SELECT '/tools/ai-model-tryon' UNION ALL SELECT '/tools/ai-video-enhance'
        UNION ALL SELECT '/tools/ai-video-translate' UNION ALL SELECT '/tools/ai-video-generator'
        UNION ALL SELECT '/workflows/visual') r
  LEFT JOIN afv_compat_core_capability_binding b
         ON b.creaft_v1_route = r.route COLLATE utf8mb4_unicode_ci AND b.deleted = 0
 WHERE b.id IS NULL;

-- ---------------------------------------------------------------------------
-- 回退（默认注释）：执行前已有该行 → 还原原值、remark、deleted；执行前没有该行 → 删除本脚本插入的行
-- ---------------------------------------------------------------------------
-- START TRANSACTION;
-- UPDATE afv_system_config c
--   JOIN afv_system_config_bak_20261031_maximg k ON k.config_key = c.config_key
--    SET c.config_value = k.config_value, c.remark = k.remark, c.deleted = k.deleted;
-- DELETE FROM afv_system_config
--  WHERE config_key = 'compat.v1.max_image_count'
--    AND NOT EXISTS (SELECT 1 FROM afv_system_config_bak_20261031_maximg k
--                     WHERE k.config_key = 'compat.v1.max_image_count');
-- COMMIT;
-- 直接改库不经失效广播，各副本 5 秒内（RuntimeSettings.LOCAL_TTL）生效。
