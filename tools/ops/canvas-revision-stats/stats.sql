-- canvas-revision-stats：画布快照历史（afv_canvas_revision）写入量统计，只读，仅 SELECT。
--
-- 用法（从库 / 低峰执行；不建议在主库高峰跑，表可能较大）：
--   mysql -h <host> -u <ro_user> -p fusion_aigc < tools/ops/canvas-revision-stats/stats.sql
--   可改下面 @days（默认 7）与 @large（大快照阈值，默认 262144 = 256KB，与代码 snapshotLargeGzipBytes 一致）。
--
-- 重要口径：表里每张画布只保留最近 20 行（写入后即裁剪），所以
--   * 「每日写入行数」只是「仍被保留的行」：活跃画布的真实保存次数被低估，不能当写入总量；
--   * 真实写入次数 / 大小分布请看指标 canvas_revision_write_total、canvas_revision_gzip_bytes（见 docs/design/canvas-agent-tools.md 观测一节）；
--   * 本脚本用来人工复核：快照有多大、压缩比、哪些画布行数堆满、哪些画布单份偏大。
-- 压缩前大小 = snapshot_bytes，压缩后大小 = LENGTH(snapshot)（gzip 字节数）。

SET @days  = 7;
SET @large = 262144;
SET @since = NOW() - INTERVAL @days DAY;

-- 1) 近 N 天每日（保留中的）写入量：行数、画布数、压缩前后总量 / 平均 / 最大、压缩比
SELECT DATE(created_at)                              AS day,
       COUNT(*)                                      AS rows_kept,
       COUNT(DISTINCT canvas_id)                     AS canvases,
       ROUND(SUM(LENGTH(snapshot)) / 1048576, 2)     AS gzip_mb,
       ROUND(AVG(LENGTH(snapshot)))                  AS gzip_avg_b,
       MAX(LENGTH(snapshot))                         AS gzip_max_b,
       ROUND(AVG(snapshot_bytes))                    AS raw_avg_b,
       MAX(snapshot_bytes)                           AS raw_max_b,
       ROUND(SUM(snapshot_bytes) / NULLIF(SUM(LENGTH(snapshot)), 0), 2) AS ratio,
       SUM(LENGTH(snapshot) > @large)                AS large_rows
  FROM afv_canvas_revision
 WHERE created_at >= @since
 GROUP BY DATE(created_at)
 ORDER BY day;

-- 2) 按来源（user / agent / ...）拆分：谁在产生写入
SELECT source,
       COUNT(*)                          AS rows_kept,
       ROUND(AVG(LENGTH(snapshot)))      AS gzip_avg_b,
       MAX(LENGTH(snapshot))             AS gzip_max_b
  FROM afv_canvas_revision
 WHERE created_at >= @since
 GROUP BY source
 ORDER BY rows_kept DESC;

-- 3) 每画布行数分布（全表）：行数 = 保留数，堆在 20 的越多说明越多画布在持续高频保存
SELECT cnt AS rows_per_canvas, COUNT(*) AS canvases
  FROM (SELECT canvas_id, COUNT(*) AS cnt FROM afv_canvas_revision GROUP BY canvas_id) t
 GROUP BY cnt
 ORDER BY cnt;

-- 4) 全表占用概览
SELECT COUNT(*)                                  AS total_rows,
       COUNT(DISTINCT canvas_id)                 AS canvases,
       ROUND(SUM(LENGTH(snapshot)) / 1048576, 2) AS gzip_mb,
       ROUND(AVG(LENGTH(snapshot)))              AS gzip_avg_b,
       MAX(LENGTH(snapshot))                     AS gzip_max_b
  FROM afv_canvas_revision;

-- 5) 单份偏大的画布 Top 20（近 N 天内压缩后最大一份，及该画布保留中的总字节）
SELECT canvas_id,
       COUNT(*)                                  AS rows_kept,
       MAX(LENGTH(snapshot))                     AS gzip_max_b,
       ROUND(SUM(LENGTH(snapshot)) / 1048576, 2) AS gzip_total_mb,
       MAX(snapshot_bytes)                       AS raw_max_b,
       MAX(created_at)                           AS last_write
  FROM afv_canvas_revision
 WHERE created_at >= @since
 GROUP BY canvas_id
 ORDER BY gzip_max_b DESC
 LIMIT 20;
