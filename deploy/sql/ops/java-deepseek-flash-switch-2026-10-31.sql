-- Java 主项目：DeepSeek Flash 模型组改名的数据切换（2026-10-31）
--   llm.deepseek-v4-flash  ->  llm.deepseek-flash
--
-- 所属库：Java 主项目库（fusion_aigc；表 afv_compat_core_capability_binding / afv_ai_model / afv_system_config /
--   afv_agent_turn）。不是 Go 自己的库，也不是网关库。只改数据，不改 Java 代码。
--
-- ============================================================================
-- ⚠️ 执行时机：必须在 gateway 发布后「立即」执行
-- ============================================================================
-- 前提：ai-gateway 已导入 deploy/seed/ai-gateway-seed-diff-deepseek-chat.sql 并「校验 → 发布」，已发布配置里有
--   llm.deepseek-flash（deepseek-chat.deepseek-flash tier 0 + bailian-chat.deepseek-flash tier 1）。
-- 同一次发布删除了 llm.deepseek-v4-flash（不保留过渡别名）。从发布到本脚本执行完之间是「窗口期」：
--   Java 的文生图润色（rewrite）/ 视频翻译（translator）/ 智能体对话（/agent/stream 的 text）按旧组名调用，
--   网关报模型组不存在，这些提交会失败。建议低峰期操作。
-- 反过来，gateway 发布之前不要执行：那时 llm.deepseek-flash 还不存在，切过去同样失败。
--
-- 窗口期最短的顺序：
--   a) 提前：执行第 0 节（只读）看清受影响行；把整份脚本准备在连好 Java 库的客户端里。
--   b) gateway 管理端「配置版本」校验 → 发布（各副本约 10 秒内加载新配置）。
--   c) 立即执行本脚本的备份与第 1～4 节（第 1～4 节在一个事务里）。
--   d) 处理 Java 进程内的模型缓存（见下方「缓存」第 3 条）；若 yaml / env 配过旧组名（见「配置文件」），一并改完后
--      滚动重启 Java。
--   e) 执行第 5 节复核（只读，期望残留为 0）；readiness 查 llm.deepseek-flash；抽查一次润色、视频翻译、智能体对话。
--
-- ============================================================================
-- 改什么（全部按「值」替换，不硬编码键名；第 0 节列出受影响行）
-- ============================================================================
--   1. afv_compat_core_capability_binding.model_roles：值为 llm.deepseek-v4-flash 的键（迁移 V1.0.165 写的 rewrite /
--      translator、V1.0.173 写的 text，或运营另配的任何键）改为 llm.deepseek-flash；video_model_aliases 同值一并处理。
--      JSON_SEARCH 找到路径后 JSON_REPLACE；一行里有多个同值键时同一语句重复执行（下面写了 3 遍，第 5 节核对残留）。
--   2. afv_ai_model.gateway_model_group = 'llm.deepseek-v4-flash' 的行改为 llm.deepseek-flash（V1.0.173 的全局模型
--      code=deepseek-v4-flash 即此行）。code 不改：会话、用户选择、计费口径可能按 code 存。
--      code 不改也不会回落到旧组：AgentModelGroupResolver.resolve 的判据顺序是 绑定 model_roles.text →
--      计费模型的 gateway_model_group（非空即用）→ 才是 "llm." + code 兜底；ChatModelFactory / AgentScopeModelFactory
--      也是 gateway_model_group 非空即经网关、用该组。所以只要 gateway_model_group 显式写了新组，code 留着
--      deepseek-v4-flash 不影响路由。
--   3. afv_system_config（运行时设置，RuntimeSettings）：config_value 恰为 llm.deepseek-v4-flash 的行（例如后台把
--      agent.helper_model_group 设成了它）改为新组。出厂没有这样的行，通常 0 行。
--   4. afv_agent_turn.model_group：受理时冻结的组。只改 status='submitted'（已受理、worker 还没开始）的轮次，
--      让排队中的轮次用新组跑；running / cancelling 的轮次已在内存里拿着旧组名，窗口期内可能失败，终态轮次是历史，不改。
--   不改：afv_workflow_step.model_group（已发往网关的步骤由网关按调用 id 继续；未发出的步骤带请求摘要，改组名会破坏
--     幂等键对应关系）、afv_gateway_cost_log.model_group（成本历史）。窗口期内新建的工作流步骤若仍是旧组名会失败，
--     重新提交即可。
--
-- ============================================================================
-- 缓存（读 Java 代码确认）
-- ============================================================================
--   1. 绑定 model_roles：提交时每次读库（BindingColumnModelRoleResolver / AgentModelGroupResolver 只读传入的绑定行），
--      无缓存，执行后下一次提交即生效。CompatBindingAdmission 的健康报告缓存 10 秒，只影响健康检查展示。
--   2. 运行时设置：RuntimeSettings 每副本本地缓存 5 秒（LOCAL_TTL），直接改库不经失效广播，5 秒内自然生效。
--   3. afv_ai_model.gateway_model_group：ChatModelFactory / AgentScopeModelFactory 按模型 id 缓存已建好的模型对象
--      （进程内 ConcurrentHashMap，无 TTL），直接改库**不会失效**。/agent/stream 不受影响（它走绑定 model_roles.text，
--      第 1 节已改），但 AI 助手、提示词优化、AgentModelRouter 等按模型 id 取模型的调用方会继续用旧组直到缓存清掉：
--        - 后台「模型管理」编辑该模型并保存（PUT /api/v1/admin/models/{id}，带 gatewayModelGroup=llm.deepseek-flash）
--          会调 evict，但只清处理这次请求的那个副本；
--        - 后台保存 AI 服务商配置会 evictAll，同样只清本副本；
--        - 多副本时最稳妥是滚动重启 Java（无需停机）。单副本时用上面的后台保存即可。
--      等价做法：不跑第 2 节，改在后台「模型管理」把该模型的网关模型组改成 llm.deepseek-flash（会审计并清本副本缓存）。
--
-- ============================================================================
-- 配置文件（只列出，不改 Java 代码；值为 llm.deepseek-v4-flash 时改为 llm.deepseek-flash 并重启 Java）
-- ============================================================================
--   app.agent-model.gateway-model-group          env AGENT_MODEL_GATEWAY_MODEL_GROUP（application.yaml app.agent-model，
--                                                默认空；AgentModelFallbackProperties，智能体文本兜底模型）
--   app.agent-vision-model.gateway-model-group   env APP_AGENT_VISION_MODEL_GATEWAY_MODEL_GROUP（yaml 里没有这一段，
--                                                只能经 env / 外部配置设置；AgentVisionModelFallbackProperties）
--   app.agent.helper-model-group                 env APP_AGENT_HELPER_MODEL_GROUP（运行时设置 agent.helper_model_group 的
--                                                配置文件值；DB 行优先，DB 行由第 3 节处理）
--   app.compat.workflow.model-roles.<能力编码>.<角色>   env APP_COMPAT_WORKFLOW_MODEL_ROLES_...（DefaultModelRoleResolver 的
--                                                出厂默认覆盖，只在绑定 model_roles 没配该角色时生效）
--   这两个 Properties 类注释里的 {@code llm.deepseek-v4-flash} 只是示例，不用管。
--   代码里写死的出厂默认 DefaultModelRoleResolver.LLM_DEEPSEEK_V4_FLASH（出图工具 rewrite）、
--   ModelGroups.LLM_DEEPSEEK_FLASH（视频翻译 translator）仍是旧组名：只在绑定 model_roles 缺该角色时才用；
--   第 1 节后各绑定都显式配了新组，平时用不到。新建库时 Flyway V1.0.165 / V1.0.173 仍会写旧组名，需在 Java 后续
--   版本改常量与迁移（新库在那之前要再跑一次本脚本）。
--
-- ============================================================================
-- 可重放 / 备份 / 回退
-- ============================================================================
-- 可重放：所有 UPDATE 只命中仍是旧值的行，第二次执行影响 0 行；备份表 CREATE TABLE IF NOT EXISTS，已存在不覆盖
--   （保留第一次执行前的原值）。
-- 备份表（建在 Java 库内，确认无误后可 DROP）：
--   afv_compat_core_capability_binding_bak_1031_dsflash（id、route、model_roles、video_model_aliases）
--   afv_ai_model_bak_1031_dsflash（id、code、gateway_model_group）
--   afv_system_config_bak_1031_dsflash（id、config_key、config_value）
--   afv_agent_turn 不备份（只改排队中的轮次，回退时按值改回即可）。
-- 回退（先在 gateway 管理端 config:rollback 回到含 llm.deepseek-v4-flash 的版本，再执行文末「回退」一节；
--   回退会用备份覆盖这几列在切换之后的人工改动）。
-- ============================================================================

SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- 0. 只读预览：受影响的行（执行前先看；可随时重复执行）
-- ---------------------------------------------------------------------------
SELECT b.id, b.creaft_v1_route AS route, b.enabled, b.deleted,
       JSON_SEARCH(b.model_roles, 'all', 'llm.deepseek-v4-flash')         AS model_roles_paths,
       JSON_SEARCH(b.video_model_aliases, 'all', 'llm.deepseek-v4-flash') AS video_alias_paths,
       b.model_roles, b.video_model_aliases
  FROM afv_compat_core_capability_binding b
 WHERE JSON_SEARCH(b.model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
    OR JSON_SEARCH(b.video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
 ORDER BY b.deleted, b.creaft_v1_route;

SELECT id, user_id, code, name, status, deleted, gateway_model_group
  FROM afv_ai_model
 WHERE gateway_model_group = 'llm.deepseek-v4-flash';

SELECT id, config_key, config_value, deleted
  FROM afv_system_config
 WHERE config_value = 'llm.deepseek-v4-flash';

SELECT status, COUNT(*) AS turns
  FROM afv_agent_turn
 WHERE model_group = 'llm.deepseek-v4-flash'
 GROUP BY status;

-- ---------------------------------------------------------------------------
-- 备份（已存在则不覆盖）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS afv_compat_core_capability_binding_bak_1031_dsflash AS
SELECT id, creaft_v1_route, model_roles, video_model_aliases, NOW() AS backed_up_at
  FROM afv_compat_core_capability_binding
 WHERE JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
    OR JSON_SEARCH(video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL;

CREATE TABLE IF NOT EXISTS afv_ai_model_bak_1031_dsflash AS
SELECT id, code, gateway_model_group, NOW() AS backed_up_at
  FROM afv_ai_model
 WHERE gateway_model_group = 'llm.deepseek-v4-flash';

CREATE TABLE IF NOT EXISTS afv_system_config_bak_1031_dsflash AS
SELECT id, config_key, config_value, NOW() AS backed_up_at
  FROM afv_system_config
 WHERE config_value = 'llm.deepseek-v4-flash';

START TRANSACTION;

-- ---------------------------------------------------------------------------
-- 1. 能力绑定：model_roles / video_model_aliases 按值替换（每遍替换每行的一个同值键；3 遍覆盖一行多键）
-- ---------------------------------------------------------------------------
UPDATE afv_compat_core_capability_binding
   SET model_roles = JSON_REPLACE(model_roles,
                                  JSON_UNQUOTE(JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash')),
                                  'llm.deepseek-flash')
 WHERE JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL;
UPDATE afv_compat_core_capability_binding
   SET model_roles = JSON_REPLACE(model_roles,
                                  JSON_UNQUOTE(JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash')),
                                  'llm.deepseek-flash')
 WHERE JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL;
UPDATE afv_compat_core_capability_binding
   SET model_roles = JSON_REPLACE(model_roles,
                                  JSON_UNQUOTE(JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash')),
                                  'llm.deepseek-flash')
 WHERE JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL;

UPDATE afv_compat_core_capability_binding
   SET video_model_aliases = JSON_REPLACE(video_model_aliases,
                                          JSON_UNQUOTE(JSON_SEARCH(video_model_aliases, 'one', 'llm.deepseek-v4-flash')),
                                          'llm.deepseek-flash')
 WHERE JSON_SEARCH(video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL;
UPDATE afv_compat_core_capability_binding
   SET video_model_aliases = JSON_REPLACE(video_model_aliases,
                                          JSON_UNQUOTE(JSON_SEARCH(video_model_aliases, 'one', 'llm.deepseek-v4-flash')),
                                          'llm.deepseek-flash')
 WHERE JSON_SEARCH(video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. 计费 / 对话模型：显式网关模型组（code 不改，理由见文件头）
-- ---------------------------------------------------------------------------
UPDATE afv_ai_model
   SET gateway_model_group = 'llm.deepseek-flash'
 WHERE gateway_model_group = 'llm.deepseek-v4-flash';

-- ---------------------------------------------------------------------------
-- 3. 运行时设置：值恰为旧组名的行
-- ---------------------------------------------------------------------------
UPDATE afv_system_config
   SET config_value = 'llm.deepseek-flash'
 WHERE config_value = 'llm.deepseek-v4-flash';

-- ---------------------------------------------------------------------------
-- 4. 排队中的智能体轮次（已受理、未开始）
-- ---------------------------------------------------------------------------
UPDATE afv_agent_turn
   SET model_group = 'llm.deepseek-flash'
 WHERE model_group = 'llm.deepseek-v4-flash'
   AND status = 'submitted';

COMMIT;

-- ---------------------------------------------------------------------------
-- 5. 只读复核：前三项期望 0；第四项只应剩终态 / running 的历史轮次
-- ---------------------------------------------------------------------------
SELECT 'binding.model_roles / video_model_aliases' AS item, COUNT(*) AS remaining
  FROM afv_compat_core_capability_binding
 WHERE JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
    OR JSON_SEARCH(video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
UNION ALL
SELECT 'ai_model.gateway_model_group', COUNT(*) FROM afv_ai_model WHERE gateway_model_group = 'llm.deepseek-v4-flash'
UNION ALL
SELECT 'system_config.config_value', COUNT(*) FROM afv_system_config WHERE config_value = 'llm.deepseek-v4-flash'
UNION ALL
SELECT 'agent_turn(submitted)', COUNT(*) FROM afv_agent_turn
 WHERE model_group = 'llm.deepseek-v4-flash' AND status = 'submitted';

-- 切换后的绑定一览（含新组的行）
SELECT b.id, b.creaft_v1_route AS route, b.enabled, b.deleted,
       JSON_SEARCH(b.model_roles, 'all', 'llm.deepseek-flash') AS new_group_paths, b.model_roles
  FROM afv_compat_core_capability_binding b
 WHERE JSON_SEARCH(b.model_roles, 'one', 'llm.deepseek-flash') IS NOT NULL
    OR JSON_SEARCH(b.video_model_aliases, 'one', 'llm.deepseek-flash') IS NOT NULL
 ORDER BY b.deleted, b.creaft_v1_route;

SELECT id, user_id, code, status, gateway_model_group
  FROM afv_ai_model
 WHERE gateway_model_group = 'llm.deepseek-flash';

-- ============================================================================
-- 回退（默认注释；先在 gateway 回滚到含 llm.deepseek-v4-flash 的配置版本，再取消注释执行）
-- ============================================================================
-- START TRANSACTION;
-- UPDATE afv_compat_core_capability_binding b
--   JOIN afv_compat_core_capability_binding_bak_1031_dsflash k ON k.id = b.id
--    SET b.model_roles = k.model_roles, b.video_model_aliases = k.video_model_aliases;
-- UPDATE afv_ai_model m
--   JOIN afv_ai_model_bak_1031_dsflash k ON k.id = m.id
--    SET m.gateway_model_group = k.gateway_model_group;
-- UPDATE afv_system_config c
--   JOIN afv_system_config_bak_1031_dsflash k ON k.id = c.id
--    SET c.config_value = k.config_value;
-- UPDATE afv_agent_turn SET model_group = 'llm.deepseek-v4-flash'
--  WHERE model_group = 'llm.deepseek-flash' AND status = 'submitted';
-- COMMIT;
-- 回退后同样要处理第「缓存」3 条的模型缓存（后台保存该模型或滚动重启）。
