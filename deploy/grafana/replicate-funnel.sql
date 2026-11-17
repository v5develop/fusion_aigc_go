-- ============================================================
-- 视频做同款：业务漏斗按天查询（表 afv_replicate_funnel，Go_V1.0.53）
--
-- 部署里目前没有 MySQL 数据源，Grafana 看板（replicate-funnel.json）只放了 Prometheus 实时计数；
-- 按天历史、严格同批次转化率、两个决策指标（多人原片占比、定妆图 → 确认转化）在 DB 控制台跑下面的查询。
-- 以后接入 MySQL 数据源，把每条查询贴进 Table / Time series 面板即可（时间列已是 day / time）。
--
-- 口径：
--   · stage 取值见 migration 注释；channel = agent（智能体）/ web（REST）/ system（兜底，通常系统事件继承用户入口渠道）。
--   · 下面「事件口径」的比率 = 当天某阶段事件数 / 当天另一阶段事件数（跨天会有错位，量大时够用）；
--     「同批次口径」按 plan_id / job_id 关联，是严格的转化。
--   · 时间窗口默认最近 30 天，改 INTERVAL 即可。
--   · 批量（Go_V1.0.54）：批内每条任务也记一条 confirm_all（为了让后续系统事件继承渠道），但它们不是「看完方案后的确认」，
--     下面所有确认口径都用 afv_replicate_batch_job 排除批内任务；批量本身单列 batch_confirm。
--     Prometheus 看板的 confirm_all 计数含批内任务，看转化率以本文件为准。
-- ============================================================

-- 1. 按天漏斗（事件数，行转列）
SELECT DATE(created_at) AS day,
       SUM(stage = 'analysis_create')  AS analysis_create,
       SUM(stage = 'analysis_succeed') AS analysis_succeed,
       SUM(stage = 'plan_create')      AS plan_create,
       SUM(stage = 'preview')          AS preview,
       SUM(stage = 'confirm_first')    AS confirm_first,
       SUM(stage = 'confirm_all' AND job_id NOT IN (SELECT job_id FROM afv_replicate_batch_job))      AS confirm_all,
       SUM(stage = 'first_done')       AS first_done,
       SUM(stage = 'continue')         AS continue_rest,
       SUM(stage = 'delivered')        AS delivered,
       SUM(stage = 'segment_save')     AS segment_save,
       SUM(stage IN ('redo', 'refine')) AS redo_refine,
       SUM(stage = 'bad_output')       AS bad_output,
       SUM(stage IN ('stop', 'cancel_rest', 'decision_timeout')) AS stopped,
       SUM(stage = 'batch_confirm')    AS batch_confirm
FROM afv_replicate_funnel
WHERE created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY DATE(created_at)
ORDER BY day;

-- 2. 按天 × 渠道 × 阶段（渠道切分：agent 智能体入口 vs web REST）
SELECT DATE(created_at) AS day, channel, stage, COUNT(*) AS n
FROM afv_replicate_funnel
WHERE created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY DATE(created_at), channel, stage
ORDER BY day, channel, stage;

-- 3. 按天转化率（事件口径）
SELECT day,
       ROUND(analysis_succeed / NULLIF(analysis_create, 0), 4)           AS analysis_success_rate,
       ROUND(plan_create / NULLIF(analysis_succeed, 0), 4)               AS plan_rate,
       ROUND((confirm_first + confirm_all) / NULLIF(plan_create, 0), 4)  AS confirm_rate,
       ROUND(continue_rest / NULLIF(first_done, 0), 4)                   AS continue_rate,
       ROUND(delivered / NULLIF(confirm_first + confirm_all, 0), 4)      AS deliver_rate,
       ROUND(redo_refine / NULLIF(delivered, 0), 4)                      AS redo_rate,
       ROUND(bad_output / NULLIF(confirm_first + confirm_all, 0), 4)     AS bad_output_rate
FROM (
  SELECT DATE(created_at) AS day,
         SUM(stage = 'analysis_create') AS analysis_create, SUM(stage = 'analysis_succeed') AS analysis_succeed,
         SUM(stage = 'plan_create') AS plan_create, SUM(stage = 'confirm_first') AS confirm_first, SUM(stage = 'confirm_all' AND job_id NOT IN (SELECT job_id FROM afv_replicate_batch_job)) AS confirm_all,
         SUM(stage = 'first_done') AS first_done, SUM(stage = 'continue') AS continue_rest, SUM(stage = 'delivered') AS delivered,
         SUM(stage IN ('redo', 'refine')) AS redo_refine, SUM(stage = 'bad_output') AS bad_output
  FROM afv_replicate_funnel
  WHERE created_at >= CURDATE() - INTERVAL 30 DAY
  GROUP BY DATE(created_at)
) t
ORDER BY day;

-- 4. 第 1 段后继续率（同批次口径）：先做第 1 段并且第 1 段已做完的任务里，有多少继续了；按确认当天算，分渠道
SELECT DATE(cf.created_at) AS day, cf.channel,
       COUNT(*)                                   AS first_done_jobs,
       SUM(ct.job_id IS NOT NULL)                 AS continued_jobs,
       ROUND(SUM(ct.job_id IS NOT NULL) / COUNT(*), 4) AS continue_rate
FROM afv_replicate_funnel cf
JOIN afv_replicate_funnel fd ON fd.job_id = cf.job_id AND fd.stage = 'first_done'
LEFT JOIN afv_replicate_funnel ct ON ct.job_id = cf.job_id AND ct.stage = 'continue'
WHERE cf.stage = 'confirm_first' AND cf.created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY DATE(cf.created_at), cf.channel
ORDER BY day, cf.channel;

-- 5. 决策指标一：多人原片占比（按天；拆解成功的原片里主体 ≥ 2 的比例）
SELECT DATE(created_at) AS day,
       COUNT(*)                         AS analyses,
       SUM(multi_person = 1)            AS multi_person_analyses,
       ROUND(SUM(multi_person = 1) / COUNT(*), 4) AS multi_person_share
FROM afv_replicate_funnel
WHERE stage = 'analysis_succeed' AND created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY DATE(created_at)
ORDER BY day;

-- 5b. 多人 vs 单人原片的后续转化（方案出卡 → 确认；同一分析下任一方案被确认就算转化）
SELECT s.multi_person,
       COUNT(*)                                   AS analyses_with_plan,
       SUM(c.analysis_id IS NOT NULL)             AS confirmed,
       ROUND(SUM(c.analysis_id IS NOT NULL) / COUNT(*), 4) AS confirm_rate
FROM (SELECT DISTINCT analysis_id FROM afv_replicate_funnel
      WHERE stage IN ('plan_create', 'plan_edit') AND analysis_id > 0 AND created_at >= CURDATE() - INTERVAL 30 DAY) p
JOIN afv_replicate_funnel s ON s.analysis_id = p.analysis_id AND s.stage = 'analysis_succeed'
LEFT JOIN (SELECT DISTINCT analysis_id FROM afv_replicate_funnel
           WHERE (stage = 'confirm_first' OR (stage = 'confirm_all' AND job_id NOT IN (SELECT job_id FROM afv_replicate_batch_job))) AND analysis_id > 0) c ON c.analysis_id = p.analysis_id
GROUP BY s.multi_person;

-- 6. 决策指标二：定妆图 → 确认转化率（同一方案 plan_id：先出过定妆图，之后被确认），按定妆图当天
SELECT DATE(p.created_at) AS day,
       COUNT(*)                                   AS previewed_plans,
       SUM(c.plan_id IS NOT NULL)                 AS confirmed_plans,
       ROUND(SUM(c.plan_id IS NOT NULL) / COUNT(*), 4) AS preview_to_confirm
FROM afv_replicate_funnel p
LEFT JOIN (SELECT plan_id, MIN(created_at) AS at FROM afv_replicate_funnel
           WHERE (stage = 'confirm_first' OR (stage = 'confirm_all' AND job_id NOT IN (SELECT job_id FROM afv_replicate_batch_job))) AND plan_id > 0 GROUP BY plan_id) c
       ON c.plan_id = p.plan_id AND c.at >= p.created_at
WHERE p.stage = 'preview' AND p.created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY DATE(p.created_at)
ORDER BY day;

-- 6b. 有定妆图 vs 没有定妆图的方案，确认率对比（衡量定妆图是否真的促进确认）
SELECT (pv.plan_id IS NOT NULL) AS had_preview,
       COUNT(*)                                   AS plans,
       SUM(cf.plan_id IS NOT NULL)                AS confirmed,
       ROUND(SUM(cf.plan_id IS NOT NULL) / COUNT(*), 4) AS confirm_rate
FROM afv_replicate_funnel pc
LEFT JOIN (SELECT DISTINCT plan_id FROM afv_replicate_funnel WHERE stage = 'preview') pv ON pv.plan_id = pc.plan_id
LEFT JOIN (SELECT DISTINCT plan_id FROM afv_replicate_funnel WHERE (stage = 'confirm_first' OR (stage = 'confirm_all' AND job_id NOT IN (SELECT job_id FROM afv_replicate_batch_job)))) cf ON cf.plan_id = pc.plan_id
WHERE pc.stage IN ('plan_create', 'plan_edit') AND pc.created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY (pv.plan_id IS NOT NULL);

-- 7. 画面连贯度（CONTRACT §Y）：确认量按 continuity 对比（continuity 列 Go_V1.0.60；之前的历史行默认 fast）
SELECT DATE(created_at) AS day, continuity,
       COUNT(*) AS confirms
FROM afv_replicate_funnel
WHERE stage IN ('confirm_first', 'confirm_all') AND created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY DATE(created_at), continuity
ORDER BY day, continuity;

-- 8. 成片反馈（CONTRACT §Z）：按天、按连贯度的满意率（只算整条成片 seg_index = 0，段级反馈另看 8c）
SELECT DATE(created_at) AS day, continuity,
       COUNT(*)                                  AS feedbacks,
       SUM(rating = 'up')                        AS up_cnt,
       SUM(rating = 'down')                      AS down_cnt,
       ROUND(SUM(rating = 'up') / COUNT(*), 4)   AS up_rate
FROM afv_replicate_feedback
WHERE seg_index = 0 AND created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY DATE(created_at), continuity
ORDER BY day, continuity;

-- 8b. 不满意原因占比，按连贯度对比：share = 该原因出现次数 / 该天该连贯度的不满意反馈条数（可多选，所以同一组的占比之和可大于 1）。
--     seam_jump（接缝跳变）在 smooth 组是否明显低于 fast 组，就是「更连贯」是否真的有用的直接证据
SELECT t.day, t.continuity, t.reason, t.n AS cnt, ROUND(t.n / d.downs, 4) AS share
FROM (SELECT DATE(f.created_at) AS day, f.continuity, r.reason, COUNT(*) AS n
      FROM afv_replicate_feedback f
      JOIN JSON_TABLE(f.reasons, '$[*]' COLUMNS (reason varchar(32) PATH '$')) r
      WHERE f.rating = 'down' AND f.created_at >= CURDATE() - INTERVAL 30 DAY
      GROUP BY DATE(f.created_at), f.continuity, r.reason) t
JOIN (SELECT DATE(created_at) AS day, continuity, COUNT(*) AS downs
      FROM afv_replicate_feedback
      WHERE rating = 'down' AND created_at >= CURDATE() - INTERVAL 30 DAY
      GROUP BY DATE(created_at), continuity) d ON d.day = t.day AND d.continuity = t.continuity
ORDER BY t.day, t.continuity, t.n DESC;

-- 8c. 段级反馈：哪一段最常被点不满意（seg_index >= 1），按连贯度
SELECT continuity, seg_index,
       COUNT(*) AS feedbacks, SUM(rating = 'down') AS down_cnt,
       ROUND(SUM(rating = 'down') / COUNT(*), 4) AS down_rate
FROM afv_replicate_feedback
WHERE seg_index >= 1 AND created_at >= CURDATE() - INTERVAL 30 DAY
GROUP BY continuity, seg_index
ORDER BY continuity, seg_index;

-- 9. 每秒实际供应商成本，按连贯度对比（上线后核实「参考图不计费、价格不变」）。
--    成本 = 出片步骤成功时记在 afv_replicate_step.output.costCny 的网关实际成本（元，网关 call.cost.amount_cny）；
--    秒数 = 冻结在 afv_replicate_job.input.segments[seg_index-1].genDuration 的生成秒数（也是计费秒数）；
--    连贯度 = job.input.continuity（缺省 fast）。bridged = 该段是否真的带了衔接帧（output.bridgeRid 有值）：
--    若 smooth 且 bridged=1 的每秒成本与 fast 基本持平，就证明追加参考图没有增加成本。
--    精修 / 重做步骤同样计入（它们也是真实调用）；没有记录成本的步骤（costCny 缺失）不进分子分母。
SELECT DATE(s.finished_at) AS day,
       COALESCE(JSON_UNQUOTE(JSON_EXTRACT(j.input, '$.continuity')), 'fast') AS continuity,
       (JSON_EXTRACT(s.output, '$.bridgeRid') IS NOT NULL) AS bridged,
       COUNT(*)                                   AS steps,
       ROUND(SUM(CAST(JSON_UNQUOTE(JSON_EXTRACT(s.output, '$.costCny')) AS DECIMAL(14, 4))), 4) AS cost_cny,
       SUM(CAST(JSON_EXTRACT(j.input, CONCAT('$.segments[', s.seg_index - 1, '].genDuration')) AS UNSIGNED)) AS gen_seconds,
       ROUND(SUM(CAST(JSON_UNQUOTE(JSON_EXTRACT(s.output, '$.costCny')) AS DECIMAL(14, 4)))
             / NULLIF(SUM(CAST(JSON_EXTRACT(j.input, CONCAT('$.segments[', s.seg_index - 1, '].genDuration')) AS UNSIGNED)), 0), 4) AS cny_per_second
FROM afv_replicate_step s
JOIN afv_replicate_job j ON j.id = s.job_id
WHERE s.kind = 'gateway' AND s.status = 'SUCCEEDED' AND s.finished_at >= CURDATE() - INTERVAL 30 DAY
  AND JSON_EXTRACT(s.output, '$.costCny') IS NOT NULL
GROUP BY DATE(s.finished_at), continuity, bridged
ORDER BY day, continuity, bridged;
