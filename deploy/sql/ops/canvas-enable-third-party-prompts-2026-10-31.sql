-- 一次性运维脚本：把 8 条第三方来源的官方提示词置为启用（2026-10-31 已确认授权）。
-- 仅用于「按旧版种子（第三方 enabled=0）落库」的环境；新环境直接执行种子即可，不需要本脚本。
-- 不要放进种子或迁移里重复执行：否则运营在后台下架的条目会被重新上架。
-- 可重放：只改 enabled=0 的官方行；执行后可用末尾的查询复核。

UPDATE afv_prompt_library_item SET enabled = 1, updated_at = NOW()
WHERE user_id IS NULL AND enabled = 0
  AND source IN ('Awesome GPT Image','Freestylefly GPT Image 2','DavidWu GPT Image 2','Awesome GPT-4o',
                 'Banana Prompt Quicker','YouMind GPT Image 2','YouMind Nano Banana Pro');

-- 复核：官方提示词按来源与启用状态计数
SELECT source, enabled, COUNT(*) AS n FROM afv_prompt_library_item WHERE user_id IS NULL GROUP BY source, enabled ORDER BY source;
