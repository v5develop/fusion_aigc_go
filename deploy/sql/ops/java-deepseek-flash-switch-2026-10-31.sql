-- Java 主项目：DeepSeek Flash 模型组改名的数据切换（2026-10-31）
--   llm.deepseek-v4-flash  ->  llm.deepseek-flash
--
-- 所属库：Java 主项目库（fusion_aigc；表 afv_compat_core_capability_binding / afv_ai_model / afv_system_config /
--   afv_agent_turn，-1 节另读 afv_creation_task）。不是 Go 自己的库，也不是网关库。只改数据，不改 Java 代码。
--
-- ============================================================================
-- 背景与时机
-- ============================================================================
-- ai-gateway 导入 deploy/seed/ai-gateway-seed-diff-deepseek-chat.sql 并「校验 → 发布」后，已发布配置里有
--   llm.deepseek-flash（deepseek-chat.deepseek-flash tier 0 + bailian-chat.deepseek-flash tier 1），同一次发布删除
--   llm.deepseek-v4-flash（不保留别名）。Java 侧本脚本把库里引用旧组名的地方改到新组。
-- 系统尚未正式上线，没有在途任务与历史数据：gateway 发布后接着执行第 1 节即可，不需要低峰窗口。
--   上线后若再次改模型组名，必须先做 -1 节的在途预检（工作流任务受理时把模型组冻结进 workflow_input，
--   智能体轮次冻结进 afv_agent_turn.model_group，网关删旧组后这些已受理的任务会失败，与脚本执行快慢无关）。
-- gateway 发布之前不要执行第 1 节：那时 llm.deepseek-flash 还不存在，切过去同样失败。
--
-- ============================================================================
-- 脚本结构（每节标注「何时执行」）
-- ============================================================================
--   -1  可选核对：在途任务（只读）                     gateway 发布前
--    0  只读预览：受影响行                             任何时候，可重复
--    A  备份（只增不覆盖）                             gateway 发布前（推荐）；也可在第 1 节前紧挨着执行
--   [ gateway 管理端「配置版本」校验 → 发布 ]
--    1  切换（一个事务）                               gateway 发布后
--    2  只读复核                                       第 1 节后
--   回退（注释）                                       需要时
--
-- 执行方式：
--   - 先用生产账号（同一套权限）在测试库把整份脚本跑一遍，确认权限够（CREATE TABLE … LIKE、INSERT … SELECT、
--     UPDATE）且无报错。
--   - 遇错即停：客户端支持时用 `mysql --abort-source-on-error` 再 `source <文件>`（MySQL 8.0 客户端没有该选项，
--     本机 8.0.46 实测报 unknown option）；通用做法是重定向 `mysql … < 文件`，不加 --force（批处理遇第一个错误即退出，
--     实测 rc=1、后续语句不执行）。中途出错退出时，第 1 节未提交的事务随连接关闭回滚。按节执行时把该节存成单独文件。
--   - 也可以在 gateway 发布后整份执行一次：A 在 1 之前，备份到的仍是原值；-1 / 0 / 2 只读。
--     不能在 gateway 发布前整份执行（会跑到第 1 节）。
--   - 全部可重放：第 1 节只命中仍是旧值 / 仍缺键的行，第二次影响 0 行；A 用 INSERT IGNORE，已备份的行不覆盖
--     （保留第一次的原值）。
--
-- ============================================================================
-- 改什么
-- ============================================================================
--   1a. afv_compat_core_capability_binding.model_roles / video_model_aliases：值恰为 llm.deepseek-v4-flash 的键（迁移
--       V1.0.165 写的 rewrite / translator、V1.0.173 写的 text，或运营另配的任何键）按值改为 llm.deepseek-flash。
--       JSON_SEARCH 精确匹配整串（不会误伤 llm.deepseek-v4-pro 之类），找到路径后 JSON_REPLACE；一行多个同值键时
--       同一语句重复执行（写了 3 遍，第 2 节核对残留）。
--   1b. 缺键补齐（JSON_SET 只补缺的键，已有键不动）。绑定缺某个角色时，Java 回落到代码里的出厂默认，而出厂默认
--       仍是旧组名（Java 后续版本才改常量）：
--         rewrite    BindingColumnModelRoleResolver 先取 DefaultModelRoleResolver.resolve（:38 LLM_DEEPSEEK_V4_FLASH，
--                    :60 GPT_WITH_WRITERS）再叠绑定列。该默认对 GPT_IMAGE_ROUTES 的 18 条路由都注入 rewrite，
--                    结果整份冻结进 workflow_input.input.model_groups。真正调用的只有 ImageToolWorkflow:226 的
--                    /tools/ai-image-generator、/workflows/ecommerce、/workflows/fashion（ImageToolWorkflows:118
--                    把它列为必填角色 → SubmitReadinessGate 提交前查就绪，旧组不存在即拒收；运行期 rewrite 是
--                    ctx.optional，失败退回原提示词）。其余 15 条只是带着不调用。补齐范围取全部 18 条
--                    （与 V1.0.165 一致），新任务冻结的输入里就不再出现旧组名。
--         translator 视频翻译 /tools/ai-video-translate：VideoInputBuilders:394 缺键时用 ModelGroups.LLM_DEEPSEEK_FLASH
--                    （:31，旧组）；translate_model_group 是 primaryModelGroups 必经组（提交前查就绪）。
--         text       /agent/stream：AgentModelGroupResolver.resolve（:50-60）依次取 model_roles.text → 计费模型
--                    gateway_model_group → "llm." + code。只在 text 缺失/为空、且计费模型 gateway_model_group 为空、
--                    code 推导恰为 llm.deepseek-v4-flash 时补 text（gateway_model_group 为旧组的由 1c 处理；
--                    指向别的模型的绑定不动，不覆盖运营选的模型）。
--       绑定行 model_roles 为 SQL NULL / JSON null 时按空对象补；不是 JSON 对象的（Java 整列忽略、全走默认）不自动改，
--       在第 0 节列出人工处理。只补 deleted = 0 的行。
--       注意：若 Java 配置了 app.compat.workflow.model-roles.<能力编码>.rewrite / .translator（env 覆盖出厂默认），
--       补齐后绑定列优先于该配置；先看第 0 节预览与「配置文件」一节再执行。
--   1c. afv_ai_model：
--       - gateway_model_group = llm.deepseek-v4-flash 的行改为新组（V1.0.173 的全局模型 code=deepseek-v4-flash 即此行）。
--         包括用户私有模型（user_id 非 0 / -1），第 0 节单独列出，执行前人工过目。code 不改：会话、用户选择、计费
--         口径可能按 code 存；gateway_model_group 非空时路由不看 code。
--       - gateway_model_group 为空、code = deepseek-v4-flash 的文本模型（"llm." + code 会落到旧组）：
--         ai_provider_id 为空的（本来就无法直连）补显式 gateway_model_group = llm.deepseek-flash；
--         ai_provider_id 非空的不改——补上会让 ChatModelFactory（:54 gateway_model_group 非空即走网关）的直连调用方
--         改走网关，超出本次改名范围；它们在 /agent/stream 上的推导由 1b 的 text 补齐覆盖，第 0 节列出供人工决定。
--   1d. afv_system_config（运行时设置）：config_value 恰为旧组名的行（例如后台把 agent.helper_model_group 设成了它）。
--       出厂没有这样的行，通常 0 行。
--   1e. afv_agent_turn.model_group：只改 status = 'submitted'（已受理、worker 未认领）的轮次。未上线环境通常 0 行。
--   不改：afv_creation_task.workflow_input、afv_workflow_step.model_group（冻结输入与步骤带请求摘要，改组名会破坏
--     幂等键对应关系）、afv_gateway_cost_log.model_group（成本历史）。
--
-- ============================================================================
-- 缓存（读 Java 代码确认）
-- ============================================================================
--   1. 绑定 model_roles：提交时每次读库，无缓存，执行后下一次提交即生效。CompatBindingAdmission 健康报告缓存 10 秒，
--      只影响健康检查展示。
--   2. 运行时设置：RuntimeSettings 每副本本地缓存 5 秒（LOCAL_TTL），直接改库不经失效广播，5 秒内自然生效。
--   3. afv_ai_model.gateway_model_group：ChatModelFactory / AgentScopeModelFactory 按模型 id 缓存已建好的模型对象
--      （进程内 ConcurrentHashMap，无 TTL），直接改库不会失效。/agent/stream 每轮受理时 selectById 读库，不受影响；
--      AI 助手、提示词优化、AgentModelRouter 等按模型 id 取模型的调用方会继续用旧组直到缓存清掉：
--        - 后台「模型管理」编辑该模型并保存（PUT /api/v1/admin/models/{id}）会 evict，但只清处理这次请求的那个副本；
--        - 后台保存 AI 服务商配置会 evictAll，同样只清本副本；
--        - 多副本时滚动重启 Java（无需停机）；单副本时用上面的后台保存即可。
--
-- ============================================================================
-- 配置文件（只列出，不改 Java 代码；值为 llm.deepseek-v4-flash 时改为 llm.deepseek-flash 并重启 Java）
-- ============================================================================
--   app.agent-model.gateway-model-group          env AGENT_MODEL_GATEWAY_MODEL_GROUP（application.yaml app.agent-model，
--                                                默认空；AgentModelFallbackProperties，智能体文本兜底模型）
--   app.agent-vision-model.gateway-model-group   env APP_AGENT_VISION_MODEL_GATEWAY_MODEL_GROUP（yaml 里没有这一段，
--                                                只能经 env / 外部配置设置；AgentVisionModelFallbackProperties）
--   app.agent.helper-model-group                 env APP_AGENT_HELPER_MODEL_GROUP（运行时设置 agent.helper_model_group 的
--                                                配置文件值；DB 行优先，DB 行由 1d 处理）
--   app.compat.workflow.model-roles.<能力编码>.<角色>   env APP_COMPAT_WORKFLOW_MODEL_ROLES_...（DefaultModelRoleResolver 的
--                                                出厂默认覆盖，只在绑定 model_roles 没配该角色时生效）
--   这两个 Properties 类注释里的 {@code llm.deepseek-v4-flash} 只是示例，不用管。
--   代码常量 DefaultModelRoleResolver.LLM_DEEPSEEK_V4_FLASH、ModelGroups.LLM_DEEPSEEK_FLASH 仍是旧组名，只在绑定缺该
--   角色时才用（1b 已把现存绑定补齐）。之后新建 / 恢复的绑定、或运营删掉这些键，会重新回落到旧组；
--   新建库时 Flyway V1.0.165 / V1.0.173 也仍写旧组名。Java 后续版本改常量与迁移之前，这些情况要再跑一次本脚本。
-- ============================================================================

SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci;

-- ===========================================================================
-- -1. 可选核对：在途任务（只读）                                【何时执行】gateway 发布前
--     未上线环境两条计数应为 0；不为 0 说明是测试数据，可在后台取消或直接清理测试任务后再继续。
--     上线后再改组名时这是硬门槛：两条都为 0 才能发布 gateway；不为 0 就等任务自然完成，或运营在后台取消 / 重提。
--     不要直接改 workflow_input（与提交幂等键 / 请求摘要的对应关系会被破坏）。
--     说明：rewrite 的出厂默认会被冻结进 18 条出图路由的 workflow_input（见文件头 1b），其中只有
--     ai-image-generator / ecommerce / fashion 真正调用；第一条按整份 workflow_input 搜索，偏保守，按 route 细分看第二条。
-- ===========================================================================
SELECT 'creation_task 非终态且冻结旧组' AS item, COUNT(*) AS cnt
  FROM afv_creation_task
 WHERE status IN (0, 1)            -- CreationTaskStatus: 0 QUEUED / 1 PROCESSING（终态 2/3/4/5）
   AND JSON_SEARCH(workflow_input, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
UNION ALL
SELECT 'agent_turn 未结束且冻结旧组', COUNT(*)
  FROM afv_agent_turn
 WHERE status IN ('submitted', 'running', 'cancelling')   -- 其余 completed / failed / cancelled 为终态
   AND model_group = 'llm.deepseek-v4-flash';

SELECT JSON_UNQUOTE(JSON_EXTRACT(workflow_input, '$.input.route')) AS route, status,
       JSON_SEARCH(workflow_input, 'all', 'llm.deepseek-v4-flash') AS hit_paths, COUNT(*) AS cnt
  FROM afv_creation_task
 WHERE status IN (0, 1)
   AND JSON_SEARCH(workflow_input, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
 GROUP BY route, status, hit_paths;

-- ===========================================================================
-- 0. 只读预览：受影响的行                                       【何时执行】任何时候，可重复
-- ===========================================================================
-- 0a. 绑定：值为旧组名的键（1a 按值替换）
SELECT b.id, b.creaft_v1_route AS route, b.enabled, b.deleted,
       JSON_SEARCH(b.model_roles, 'all', 'llm.deepseek-v4-flash')         AS model_roles_paths,
       JSON_SEARCH(b.video_model_aliases, 'all', 'llm.deepseek-v4-flash') AS video_alias_paths,
       b.model_roles, b.video_model_aliases
  FROM afv_compat_core_capability_binding b
 WHERE JSON_SEARCH(b.model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
    OR JSON_SEARCH(b.video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
 ORDER BY b.deleted, b.creaft_v1_route;

-- 0b. 绑定：缺角色、会回落到旧组出厂默认的行（1b 补齐）；bad_json = 1 的不会自动改，需人工处理
SELECT b.id, b.creaft_v1_route AS route, b.enabled, 'rewrite' AS missing_role, b.model_roles,
       (b.model_roles IS NOT NULL AND JSON_TYPE(b.model_roles) NOT IN ('OBJECT', 'NULL')) AS bad_json
  FROM afv_compat_core_capability_binding b
 WHERE b.deleted = 0
   AND b.creaft_v1_route IN (
        '/tools/ai-erase', '/tools/ai-retouch', '/tools/ai-clothing-dewrinkle', '/tools/ai-image-upscaler',
        '/tools/ai-image-translate', '/tools/ai-inpaint', '/tools/ai-product-replace',
        '/tools/ai-background-generator', '/tools/ai-image-generator', '/tools/ai-first-screen-main-image',
        '/tools/ai-ratio-convert', '/tools/ai-posture-fission', '/tools/ai-model-tryon',
        '/workflows/ecommerce', '/workflows/fashion', '/workflows/visual',
        '/agent/canvas/image-edict', '/agent/canvas/multiview')
   AND (b.model_roles IS NULL OR JSON_TYPE(b.model_roles) <> 'OBJECT'
        OR NOT JSON_CONTAINS_PATH(b.model_roles, 'one', '$.rewrite'))
UNION ALL
SELECT b.id, b.creaft_v1_route, b.enabled, 'translator', b.model_roles,
       (b.model_roles IS NOT NULL AND JSON_TYPE(b.model_roles) NOT IN ('OBJECT', 'NULL'))
  FROM afv_compat_core_capability_binding b
 WHERE b.deleted = 0
   AND b.creaft_v1_route = '/tools/ai-video-translate'
   AND (b.model_roles IS NULL OR JSON_TYPE(b.model_roles) <> 'OBJECT'
        OR NOT JSON_CONTAINS_PATH(b.model_roles, 'one', '$.translator'))
UNION ALL
SELECT b.id, b.creaft_v1_route, b.enabled, 'text', b.model_roles,
       (b.model_roles IS NOT NULL AND JSON_TYPE(b.model_roles) NOT IN ('OBJECT', 'NULL'))
  FROM afv_compat_core_capability_binding b
  JOIN afv_ai_model m ON m.id = b.model_id
 WHERE b.deleted = 0
   AND b.creaft_v1_route = '/agent/stream'
   AND TRIM(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(b.model_roles, '$.text')), '')) IN ('', 'null')
   AND TRIM(COALESCE(m.gateway_model_group, '')) = ''
   AND CONCAT('llm.', TRIM(m.code)) = 'llm.deepseek-v4-flash'
 ORDER BY route;

-- 0c. 计费 / 对话模型：gateway_model_group 为旧组（1c 改）。scope = 用户私有 的行请人工过目
SELECT id, user_id, code, name, model_type, status, deleted, gateway_model_group,
       CASE WHEN user_id IN (0, -1) THEN '全局' ELSE '用户私有：人工过目' END AS scope
  FROM afv_ai_model
 WHERE TRIM(gateway_model_group) = 'llm.deepseek-v4-flash'
 ORDER BY scope, id;

-- 0d. 计费 / 对话模型：gateway_model_group 为空、"llm." + code 会落到旧组
--     action = 补显式组 的由 1c 改；= 不改 的（有直连服务商）只由 1b 补绑定 text，是否改走网关人工决定
SELECT id, user_id, code, name, model_type, status, ai_provider_id, gateway_model_group,
       CASE WHEN ai_provider_id IS NULL THEN '补显式组' ELSE '不改（有直连服务商）' END AS action
  FROM afv_ai_model
 WHERE deleted = 0 AND model_type = 1
   AND TRIM(COALESCE(gateway_model_group, '')) = ''
   AND TRIM(code) = 'deepseek-v4-flash';

-- 0e. 运行时设置
SELECT id, config_key, config_value, deleted
  FROM afv_system_config
 WHERE config_value = 'llm.deepseek-v4-flash';

-- 0f. 排队中的智能体轮次（走索引 idx_afv_agent_turn_status_created）
SELECT status, COUNT(*) AS turns
  FROM afv_agent_turn
 WHERE status = 'submitted'
   AND model_group = 'llm.deepseek-v4-flash'
 GROUP BY status;

-- ===========================================================================
-- A. 备份（只增不覆盖）                                         【何时执行】gateway 发布前（推荐）
--    表结构与原表相同（LIKE），按主键 INSERT IGNORE：重放不覆盖第一次的原值。只备份第 1 节会改的行。
--    afv_agent_turn 不备份（只改排队中的轮次，回退按值改回）。确认无误后可 DROP 这三张表。
-- ===========================================================================
CREATE TABLE IF NOT EXISTS afv_compat_core_capability_binding_bak_1031_dsflash
    LIKE afv_compat_core_capability_binding;
INSERT IGNORE INTO afv_compat_core_capability_binding_bak_1031_dsflash
SELECT b.*
  FROM afv_compat_core_capability_binding b
 WHERE JSON_SEARCH(b.model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
    OR JSON_SEARCH(b.video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
    OR (b.deleted = 0
        AND (b.model_roles IS NULL OR JSON_TYPE(b.model_roles) IN ('OBJECT', 'NULL'))
        AND ((b.creaft_v1_route IN (
                '/tools/ai-erase', '/tools/ai-retouch', '/tools/ai-clothing-dewrinkle', '/tools/ai-image-upscaler',
                '/tools/ai-image-translate', '/tools/ai-inpaint', '/tools/ai-product-replace',
                '/tools/ai-background-generator', '/tools/ai-image-generator', '/tools/ai-first-screen-main-image',
                '/tools/ai-ratio-convert', '/tools/ai-posture-fission', '/tools/ai-model-tryon',
                '/workflows/ecommerce', '/workflows/fashion', '/workflows/visual',
                '/agent/canvas/image-edict', '/agent/canvas/multiview')
              AND (JSON_TYPE(b.model_roles) <> 'OBJECT' OR b.model_roles IS NULL
                   OR NOT JSON_CONTAINS_PATH(b.model_roles, 'one', '$.rewrite')))
          OR (b.creaft_v1_route = '/tools/ai-video-translate'
              AND (JSON_TYPE(b.model_roles) <> 'OBJECT' OR b.model_roles IS NULL
                   OR NOT JSON_CONTAINS_PATH(b.model_roles, 'one', '$.translator')))
          OR (b.creaft_v1_route = '/agent/stream'
              AND TRIM(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(b.model_roles, '$.text')), '')) IN ('', 'null')
              AND EXISTS (SELECT 1 FROM afv_ai_model m
                           WHERE m.id = b.model_id
                             AND TRIM(COALESCE(m.gateway_model_group, '')) = ''
                             AND CONCAT('llm.', TRIM(m.code)) = 'llm.deepseek-v4-flash'))));

CREATE TABLE IF NOT EXISTS afv_ai_model_bak_1031_dsflash LIKE afv_ai_model;
INSERT IGNORE INTO afv_ai_model_bak_1031_dsflash
SELECT *
  FROM afv_ai_model
 WHERE TRIM(gateway_model_group) = 'llm.deepseek-v4-flash'
    OR (deleted = 0 AND model_type = 1 AND ai_provider_id IS NULL
        AND TRIM(COALESCE(gateway_model_group, '')) = '' AND TRIM(code) = 'deepseek-v4-flash');

CREATE TABLE IF NOT EXISTS afv_system_config_bak_1031_dsflash LIKE afv_system_config;
INSERT IGNORE INTO afv_system_config_bak_1031_dsflash
SELECT * FROM afv_system_config WHERE config_value = 'llm.deepseek-v4-flash';

-- ---------------------------------------------------------------------------
-- [ 此处：gateway 管理端「配置版本」校验 → 发布，确认 llm.deepseek-flash 已在已发布配置里 ]
-- ---------------------------------------------------------------------------

-- ===========================================================================
-- 1. 切换（一个事务）                                           【何时执行】gateway 发布后
-- ===========================================================================
START TRANSACTION;

-- 1a. 绑定：按值替换（每遍替换每行的一个同值键；3 遍覆盖一行多键）
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

-- 1b. 绑定：缺键补齐（只补缺的键；NULL / JSON null 按空对象；非对象 JSON 不动）
--     text 必须在 1c 之前：1c 会给 code 推导的模型补显式组，之后这里的判据就不再命中
UPDATE afv_compat_core_capability_binding b
  JOIN afv_ai_model m ON m.id = b.model_id
   SET b.model_roles = JSON_SET(IF(b.model_roles IS NULL OR JSON_TYPE(b.model_roles) = 'NULL',
                                   JSON_OBJECT(), b.model_roles),
                                '$.text', 'llm.deepseek-flash')
 WHERE b.deleted = 0
   AND b.creaft_v1_route = '/agent/stream'
   AND (b.model_roles IS NULL OR JSON_TYPE(b.model_roles) IN ('OBJECT', 'NULL'))
   AND TRIM(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(b.model_roles, '$.text')), '')) IN ('', 'null')
   AND TRIM(COALESCE(m.gateway_model_group, '')) = ''
   AND CONCAT('llm.', TRIM(m.code)) = 'llm.deepseek-v4-flash';

UPDATE afv_compat_core_capability_binding
   SET model_roles = JSON_SET(IF(model_roles IS NULL OR JSON_TYPE(model_roles) = 'NULL', JSON_OBJECT(), model_roles),
                              '$.rewrite', 'llm.deepseek-flash')
 WHERE deleted = 0
   AND creaft_v1_route IN (
        '/tools/ai-erase', '/tools/ai-retouch', '/tools/ai-clothing-dewrinkle', '/tools/ai-image-upscaler',
        '/tools/ai-image-translate', '/tools/ai-inpaint', '/tools/ai-product-replace',
        '/tools/ai-background-generator', '/tools/ai-image-generator', '/tools/ai-first-screen-main-image',
        '/tools/ai-ratio-convert', '/tools/ai-posture-fission', '/tools/ai-model-tryon',
        '/workflows/ecommerce', '/workflows/fashion', '/workflows/visual',
        '/agent/canvas/image-edict', '/agent/canvas/multiview')
   AND (model_roles IS NULL OR JSON_TYPE(model_roles) = 'NULL'
        OR (JSON_TYPE(model_roles) = 'OBJECT' AND NOT JSON_CONTAINS_PATH(model_roles, 'one', '$.rewrite')));

UPDATE afv_compat_core_capability_binding
   SET model_roles = JSON_SET(IF(model_roles IS NULL OR JSON_TYPE(model_roles) = 'NULL', JSON_OBJECT(), model_roles),
                              '$.translator', 'llm.deepseek-flash')
 WHERE deleted = 0
   AND creaft_v1_route = '/tools/ai-video-translate'
   AND (model_roles IS NULL OR JSON_TYPE(model_roles) = 'NULL'
        OR (JSON_TYPE(model_roles) = 'OBJECT' AND NOT JSON_CONTAINS_PATH(model_roles, 'one', '$.translator')));

-- 1c. 计费 / 对话模型（code 不改）
UPDATE afv_ai_model
   SET gateway_model_group = 'llm.deepseek-flash'
 WHERE TRIM(gateway_model_group) = 'llm.deepseek-v4-flash';

UPDATE afv_ai_model
   SET gateway_model_group = 'llm.deepseek-flash'
 WHERE deleted = 0 AND model_type = 1 AND ai_provider_id IS NULL
   AND TRIM(COALESCE(gateway_model_group, '')) = ''
   AND TRIM(code) = 'deepseek-v4-flash';

-- 1d. 运行时设置：值恰为旧组名的行
UPDATE afv_system_config
   SET config_value = 'llm.deepseek-flash'
 WHERE config_value = 'llm.deepseek-v4-flash';

-- 1e. 排队中的智能体轮次（已受理、未开始）
UPDATE afv_agent_turn
   SET model_group = 'llm.deepseek-flash'
 WHERE status = 'submitted'
   AND model_group = 'llm.deepseek-v4-flash';

COMMIT;

-- ===========================================================================
-- 2. 只读复核                                                   【何时执行】第 1 节后
--    remaining 期望全为 0。例外：「缺 rewrite / translator」两项包含 0b 里 bad_json = 1 的行（model_roles 不是 JSON
--    对象，不自动改，到后台「Creaft 能力绑定」重存该行）；ai_model_code_derived 只计无直连服务商的行（有的见 0d）。
-- ===========================================================================
SELECT 'binding.model_roles / video_model_aliases' AS item, COUNT(*) AS remaining
  FROM afv_compat_core_capability_binding
 WHERE JSON_SEARCH(model_roles, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
    OR JSON_SEARCH(video_model_aliases, 'one', 'llm.deepseek-v4-flash') IS NOT NULL
UNION ALL
SELECT 'binding 缺 rewrite（出图 18 路由）', COUNT(*)
  FROM afv_compat_core_capability_binding
 WHERE deleted = 0
   AND creaft_v1_route IN (
        '/tools/ai-erase', '/tools/ai-retouch', '/tools/ai-clothing-dewrinkle', '/tools/ai-image-upscaler',
        '/tools/ai-image-translate', '/tools/ai-inpaint', '/tools/ai-product-replace',
        '/tools/ai-background-generator', '/tools/ai-image-generator', '/tools/ai-first-screen-main-image',
        '/tools/ai-ratio-convert', '/tools/ai-posture-fission', '/tools/ai-model-tryon',
        '/workflows/ecommerce', '/workflows/fashion', '/workflows/visual',
        '/agent/canvas/image-edict', '/agent/canvas/multiview')
   AND (model_roles IS NULL OR JSON_TYPE(model_roles) <> 'OBJECT'
        OR NOT JSON_CONTAINS_PATH(model_roles, 'one', '$.rewrite'))
UNION ALL
SELECT 'binding 缺 translator（视频翻译）', COUNT(*)
  FROM afv_compat_core_capability_binding
 WHERE deleted = 0 AND creaft_v1_route = '/tools/ai-video-translate'
   AND (model_roles IS NULL OR JSON_TYPE(model_roles) <> 'OBJECT'
        OR NOT JSON_CONTAINS_PATH(model_roles, 'one', '$.translator'))
UNION ALL
SELECT 'ai_model.gateway_model_group', COUNT(*)
  FROM afv_ai_model WHERE TRIM(gateway_model_group) = 'llm.deepseek-v4-flash'
UNION ALL
SELECT 'ai_model_code_derived', COUNT(*)
  FROM afv_ai_model
 WHERE deleted = 0 AND model_type = 1 AND ai_provider_id IS NULL
   AND TRIM(COALESCE(gateway_model_group, '')) = '' AND TRIM(code) = 'deepseek-v4-flash'
UNION ALL
SELECT 'system_config.config_value', COUNT(*) FROM afv_system_config WHERE config_value = 'llm.deepseek-v4-flash'
UNION ALL
SELECT 'agent_turn(submitted)', COUNT(*) FROM afv_agent_turn
 WHERE status = 'submitted' AND model_group = 'llm.deepseek-v4-flash';

-- 切换后引用新组的绑定一览
SELECT b.id, b.creaft_v1_route AS route, b.enabled, b.deleted,
       JSON_SEARCH(b.model_roles, 'all', 'llm.deepseek-flash') AS new_group_paths, b.model_roles
  FROM afv_compat_core_capability_binding b
 WHERE JSON_SEARCH(b.model_roles, 'one', 'llm.deepseek-flash') IS NOT NULL
    OR JSON_SEARCH(b.video_model_aliases, 'one', 'llm.deepseek-flash') IS NOT NULL
 ORDER BY b.deleted, b.creaft_v1_route;

SELECT id, user_id, code, status, gateway_model_group
  FROM afv_ai_model
 WHERE gateway_model_group = 'llm.deepseek-flash';

-- ===========================================================================
-- 回退（默认注释）                                              【何时执行】需要时
-- 先在 gateway 管理端 config:rollback 回到含 llm.deepseek-v4-flash 的版本，再取消注释执行。
-- 从备份表恢复第 1 节改过的列（含 1b 补的键：model_roles 整列回到备份值）；会覆盖这些列在切换之后的人工改动。
-- ===========================================================================
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
--  WHERE status = 'submitted' AND model_group = 'llm.deepseek-flash';
-- COMMIT;
-- 回退后同样要处理「缓存」第 3 条的模型缓存（后台保存该模型或滚动重启）。
